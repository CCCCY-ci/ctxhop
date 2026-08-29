package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

var (
	// ErrRecordStreamMismatch reports a stream whose final count or digest does
	// not match the expected authenticated source metadata.
	ErrRecordStreamMismatch = errors.New("syncer: record stream does not match expected metadata")

	// ErrRecordStreamClosed reports use of a record reader after it has been
	// closed.
	ErrRecordStreamClosed = errors.New("syncer: record stream is closed")
)

// RecordReader exposes one canonical record at a time. Implementations own
// their remote reader and must release it from Close. A publisher consumes and
// closes the reader even when a body or remote write fails.
type RecordReader interface {
	Next(context.Context) ([]byte, error)
	Close() error
}

// SliceRecordReader adapts an already materialized stream to RecordReader.
// It is intentionally small and is used by compatibility wrappers and tests;
// migration's production path uses LegacyReplicaReader instead.
type SliceRecordReader struct {
	records [][]byte
	index   int
	closed  bool
}

// NewSliceRecordReader creates a record reader that does not retain the
// caller's mutable record buffers.
func NewSliceRecordReader(records [][]byte) (*SliceRecordReader, error) {
	copyRecords, err := copyRecords(records)
	if err != nil {
		return nil, err
	}
	return &SliceRecordReader{records: copyRecords}, nil
}

// Next returns the next canonical record or io.EOF after the stream ends.
func (r *SliceRecordReader) Next(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.closed {
		return nil, ErrRecordStreamClosed
	}
	if r.index >= len(r.records) {
		return nil, io.EOF
	}
	record := append([]byte(nil), r.records[r.index]...)
	r.index++
	return record, nil
}

// Close releases the in-memory reader. It is idempotent.
func (r *SliceRecordReader) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	r.records = nil
	return nil
}

// ReplicaStreamOptions extends ReplicaPushOptions with an optional
// authenticated end-of-stream check. The expected values normally come from
// the v1 metadata object that a LegacyReplicaReader verifies while streaming.
type ReplicaStreamOptions struct {
	ReplicaPushOptions
	VerifyExpected      bool
	ExpectedRecordCount uint64
	ExpectedHeadDigest  [32]byte
}

// PushReplicaStreamWithCursorStore publishes a source-native Replica from a
// bounded record stream. It buffers at most one v2 shard, skips the durable
// cursor prefix while revalidating its digest, and commits the cursor after
// each immutable shard. It never assembles the complete source stream in
// memory.
func PushReplicaStreamWithCursorStore(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, state ReplicaCursorStore, reader RecordReader, options ReplicaStreamOptions) (result ReplicaPushResult, err error) {
	if ctx == nil {
		return ReplicaPushResult{}, errors.New("syncer: context is required")
	}
	if reader == nil {
		return ReplicaPushResult{}, errors.New("syncer: record reader is required")
	}
	defer func() {
		closeErr := reader.Close()
		if closeErr == nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("syncer: close record reader: %w", closeErr)
			return
		}
		err = errors.Join(err, fmt.Errorf("syncer: close record reader: %w", closeErr))
	}()

	cursor, loadErr := state.Load(ctx)
	if errors.Is(loadErr, ErrNoReplicaCursor) {
		cursor = NewPushCursor()
	} else if loadErr != nil {
		return ReplicaPushResult{}, loadErr
	}
	result, err = pushReplicaStream(ctx, store, recipient, layout, descriptor, cursor, state, reader, options)
	return result, err
}

func pushReplicaStream(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, cursor PushCursor, state ReplicaCursorStore, reader RecordReader, options ReplicaStreamOptions) (ReplicaPushResult, error) {
	result := ReplicaPushResult{Cursor: cursor}
	if err := validateReplicaWriteArgs(ctx, store, recipient); err != nil {
		return ReplicaPushResult{}, err
	}
	if err := validateReplicaDescriptorForLayout(layout, descriptor); err != nil {
		return ReplicaPushResult{}, err
	}
	if err := cursor.Validate(); err != nil {
		return ReplicaPushResult{}, err
	}
	if options.Plan.MaxRecords == 0 && options.Plan.MaxEncodedBytes == 0 {
		options.Plan = DefaultPlanOptions()
	}
	if err := options.Plan.validate(); err != nil {
		return ReplicaPushResult{}, err
	}
	if options.VerifyExpected && options.ExpectedRecordCount > maxSessionRecords {
		return ReplicaPushResult{}, fmt.Errorf("syncer: expected record count exceeds %d", maxSessionRecords)
	}
	if options.VerifyExpected && options.ExpectedRecordCount == 0 && options.ExpectedHeadDigest != EmptyDigest() {
		return ReplicaPushResult{}, fmt.Errorf("%w: empty expected stream has a non-empty digest", ErrRecordStreamMismatch)
	}

	if err := ensureReplicaDescriptor(ctx, store, recipient, layout, descriptor, options.Identities); err != nil {
		return result, err
	}

	durable := cursor
	streamCount := uint64(0)
	streamBytes := uint64(0)
	streamDigest := EmptyDigest()
	shardRecords := make([][]byte, 0, options.Plan.MaxRecords)
	shardBytes := 0

	flush := func() error {
		result.Cursor = durable
		if len(shardRecords) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		shard, err := NewShard(durable.RecordCount, durable.HeadDigest, shardRecords)
		if err != nil {
			return err
		}
		part := ShardPart{Number: durable.NextShard, Shard: shard}
		var next PushCursor
		if len(options.Identities) == 0 {
			next, err = PutReplicaShard(ctx, store, recipient, layout, durable, part)
		} else {
			next, err = PutReplicaShardWithIdentities(ctx, store, recipient, layout, durable, part, options.Identities)
		}
		if err != nil {
			result.Cursor = durable
			result.PublishedShards = int(durable.NextShard - cursor.NextShard)
			return err
		}
		if err := state.Save(ctx, next); err != nil {
			result.Cursor = durable
			result.PublishedShards = int(durable.NextShard - cursor.NextShard)
			return errors.Join(ErrReplicaCursorCommit, err)
		}
		durable = next
		result.Cursor = durable
		result.PublishedShards++
		shardRecords = shardRecords[:0]
		shardBytes = 0
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			result.Cursor = durable
			return result, fmt.Errorf("syncer: read record stream: %w", err)
		}
		record, readErr := reader.Next(ctx)
		if errors.Is(readErr, io.EOF) {
			if err := flush(); err != nil {
				return result, fmt.Errorf("syncer: publish streamed Replica: %w", err)
			}
			if options.VerifyExpected && (streamCount != options.ExpectedRecordCount || streamDigest != options.ExpectedHeadDigest) {
				result.Cursor = durable
				return result, fmt.Errorf("%w: expected %d records with digest %x, got %d with digest %x", ErrRecordStreamMismatch, options.ExpectedRecordCount, options.ExpectedHeadDigest, streamCount, streamDigest)
			}
			if streamCount < cursor.RecordCount {
				result.Cursor = durable
				return result, fmt.Errorf("%w: stream ended before durable prefix", ErrLocalHistoryChanged)
			}
			result.Cursor = durable
			if err := state.Save(ctx, durable); err != nil {
				return result, errors.Join(ErrReplicaCursorCommit, err)
			}
			if options.Now.IsZero() {
				options.Now = time.Now().UTC()
			}
			tip := replicaTipFor(descriptor.ReplicaID, durable, options.Now)
			if err := PutReplicaTip(ctx, store, recipient, layout, tip); err != nil {
				result.Cursor = durable
				result.Tip = tip
				return result, err
			}
			result.Cursor = durable
			result.Tip = tip
			return result, nil
		}
		if readErr != nil {
			result.Cursor = durable
			return result, fmt.Errorf("syncer: read record stream: %w", readErr)
		}
		if err := validateRecord(record); err != nil {
			result.Cursor = durable
			return result, err
		}
		if streamCount == maxSessionRecords {
			result.Cursor = durable
			return result, fmt.Errorf("%w: record count exceeds %d", ErrSessionTooLarge, maxSessionRecords)
		}
		if uint64(len(record)) > maxSessionBytes-streamBytes {
			result.Cursor = durable
			return result, fmt.Errorf("%w: record bytes exceed %d", ErrSessionTooLarge, maxSessionBytes)
		}
		streamCount++
		if options.VerifyExpected && streamCount > options.ExpectedRecordCount {
			result.Cursor = durable
			return result, fmt.Errorf("%w: stream contains more than the expected %d records", ErrRecordStreamMismatch, options.ExpectedRecordCount)
		}
		streamBytes += uint64(len(record))
		streamDigest = nextDigest(streamDigest, record)

		if streamCount <= cursor.RecordCount {
			if streamCount == cursor.RecordCount && streamDigest != cursor.HeadDigest {
				return result, fmt.Errorf("%w: local prefix digest does not match the durable cursor", ErrLocalHistoryChanged)
			}
			continue
		}

		if len(shardRecords) != 0 && (len(shardRecords) >= options.Plan.MaxRecords || shardEncodedSize(durable.RecordCount, uint64(len(shardRecords)+1), shardBytes+len(record)) > options.Plan.MaxEncodedBytes) {
			if err := flush(); err != nil {
				return result, fmt.Errorf("syncer: publish streamed Replica: %w", err)
			}
		}
		if len(shardRecords) == 0 && shardEncodedSize(durable.RecordCount, 1, len(record)) > options.Plan.MaxEncodedBytes {
			return result, fmt.Errorf("%w: one record exceeds the configured envelope limit", ErrShardTooLarge)
		}
		shardRecords = append(shardRecords, append([]byte(nil), record...))
		shardBytes += len(record)
	}
}
