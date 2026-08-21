package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const defaultMaxRecordsPerShard = 256

var (
	// ErrInvalidPushCursor reports a cursor that cannot describe a durable
	// local device prefix.
	ErrInvalidPushCursor = errors.New("syncer: invalid push cursor")

	// ErrLocalHistoryChanged reports a local stream that no longer has the
	// prefix recorded by the push cursor.
	ErrLocalHistoryChanged = errors.New("syncer: local history changed")

	// ErrInvalidPushPart reports a shard that cannot be the next cursor step.
	ErrInvalidPushPart = errors.New("syncer: invalid push shard transition")

	// ErrShardTooLarge reports a record or shard that exceeds the configured
	// encoded envelope limit.
	ErrShardTooLarge = errors.New("syncer: shard is too large")

	// ErrShardSequenceExhausted reports that the fixed-width object namespace
	// has no sequence number left.
	ErrShardSequenceExhausted = errors.New("syncer: shard sequence exhausted")
)

// PushCursor is the local source of truth for the next device-owned shard.
//
// The cursor is intentionally local state. A remote listing may be stale, and
// using it to choose the next sequence can overwrite a shard after an
// interrupted or eventually consistent write.
type PushCursor struct {
	NextShard   uint64
	RecordCount uint64
	HeadDigest  [32]byte
}

// NewPushCursor returns the state before the first shard is published.
func NewPushCursor() PushCursor {
	return PushCursor{
		NextShard:  1,
		HeadDigest: EmptyDigest(),
	}
}

// Validate checks that a cursor describes a possible local device prefix.
func (c PushCursor) Validate() error {
	if c.NextShard == 0 || c.NextShard > maxShardNumber+1 {
		return fmt.Errorf("%w: next shard is outside the supported range", ErrInvalidPushCursor)
	}
	if c.RecordCount == 0 && c.HeadDigest != EmptyDigest() {
		return fmt.Errorf("%w: empty prefix has a non-empty digest", ErrInvalidPushCursor)
	}
	return nil
}

// Advance verifies and applies one successful shard publication.
func (c PushCursor) Advance(part ShardPart) (PushCursor, error) {
	if err := c.Validate(); err != nil {
		return PushCursor{}, err
	}
	if part.Number == 0 || part.Number > maxShardNumber || part.Number != c.NextShard {
		return PushCursor{}, fmt.Errorf("%w: shard sequence does not follow the cursor", ErrInvalidPushPart)
	}
	if err := part.Shard.Validate(); err != nil {
		return PushCursor{}, fmt.Errorf("%w: %v", ErrInvalidPushPart, err)
	}
	if part.Shard.Base != c.RecordCount {
		return PushCursor{}, fmt.Errorf("%w: shard base does not follow the cursor", ErrInvalidPushPart)
	}
	if part.Shard.PrefixDigest != c.HeadDigest {
		return PushCursor{}, fmt.Errorf("%w: shard prefix digest does not follow the cursor", ErrInvalidPushPart)
	}
	if ^uint64(0)-c.RecordCount < part.Shard.Count() {
		return PushCursor{}, fmt.Errorf("%w: record count overflows", ErrInvalidPushPart)
	}

	next := c.NextShard + 1
	return PushCursor{
		NextShard:   next,
		RecordCount: c.RecordCount + part.Shard.Count(),
		HeadDigest:  part.Shard.Digest(),
	}, nil
}

// PlanOptions controls how a local suffix is split into immutable shards.
type PlanOptions struct {
	// MaxRecords is the largest number of records in one shard.
	MaxRecords int

	// MaxEncodedBytes is the largest deterministic plaintext envelope. It may
	// be lower than the format hard cap, but never higher.
	MaxEncodedBytes int
}

// DefaultPlanOptions returns conservative defaults for a push plan.
func DefaultPlanOptions() PlanOptions {
	return PlanOptions{
		MaxRecords:      defaultMaxRecordsPerShard,
		MaxEncodedBytes: maxShardBytes,
	}
}

func (o PlanOptions) validate() error {
	if o.MaxRecords <= 0 {
		return errors.New("syncer: maximum records per shard must be positive")
	}
	if o.MaxEncodedBytes <= 0 || o.MaxEncodedBytes > maxShardBytes {
		return fmt.Errorf("syncer: encoded shard limit must be between 1 and %d bytes", maxShardBytes)
	}
	return nil
}

// AppendPlan is the immutable work and resulting cursor for one local push.
// Parts are ordered by their device-local object sequence.
type AppendPlan struct {
	Parts []ShardPart
	Next  PushCursor
}

// PlanAppend validates a local canonical stream and plans only its suffix
// after cursor. It never infers progress from remote storage.
func PlanAppend(cursor PushCursor, records [][]byte, options PlanOptions) (AppendPlan, error) {
	if err := cursor.Validate(); err != nil {
		return AppendPlan{}, err
	}
	if err := options.validate(); err != nil {
		return AppendPlan{}, err
	}

	if _, err := DigestRecords(records); err != nil {
		return AppendPlan{}, fmt.Errorf("syncer: validate local records: %w", err)
	}
	if cursor.RecordCount > uint64(len(records)) {
		return AppendPlan{}, fmt.Errorf("%w: local stream is shorter than the durable prefix", ErrLocalHistoryChanged)
	}
	prefixEnd := int(cursor.RecordCount)
	// DigestRecords above validated every record. Reuse the validated bytes for
	// the prefix digest without performing a second validation pass.
	prefixDigest := EmptyDigest()
	for _, record := range records[:prefixEnd] {
		prefixDigest = nextDigest(prefixDigest, record)
	}
	if prefixDigest != cursor.HeadDigest {
		return AppendPlan{}, fmt.Errorf("%w: local prefix digest does not match the cursor", ErrLocalHistoryChanged)
	}
	if cursor.RecordCount == uint64(len(records)) {
		return AppendPlan{Next: cursor}, nil
	}

	parts := make([]ShardPart, 0)
	base := cursor.RecordCount
	digest := cursor.HeadDigest
	sequence := cursor.NextShard
	start := prefixEnd
	for start < len(records) {
		if sequence == 0 || sequence > maxShardNumber {
			return AppendPlan{}, ErrShardSequenceExhausted
		}

		end, shard, err := planShard(base, digest, records, start, options)
		if err != nil {
			return AppendPlan{}, err
		}
		parts = append(parts, ShardPart{Number: sequence, Shard: shard})
		base += shard.Count()
		digest = shard.Digest()
		start = end
		sequence++
	}

	return AppendPlan{
		Parts: parts,
		Next: PushCursor{
			NextShard:   sequence,
			RecordCount: base,
			HeadDigest:  digest,
		},
	}, nil
}

// PutShard encrypts and publishes one cursor-checked shard. The returned
// cursor is durable only when the remote write returns nil; callers should
// persist it atomically with their local queue state.
func PutShard(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, cursor PushCursor, part ShardPart) (PushCursor, error) {
	if ctx == nil {
		return PushCursor{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return PushCursor{}, errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return PushCursor{}, errors.New("syncer: recipient key is required")
	}
	next, err := cursor.Advance(part)
	if err != nil {
		return PushCursor{}, err
	}
	key, err := layout.ShardKey(part.Number)
	if err != nil {
		return PushCursor{}, err
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: publish shard: %w", err)
	}
	sealed, err := SealShard(recipient, key, part.Shard)
	if err != nil {
		return PushCursor{}, fmt.Errorf("syncer: seal shard for publication: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: publish shard: %w", err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: publish shard: %w", err)
	}
	return next, nil
}

func planShard(base uint64, prefixDigest [32]byte, records [][]byte, start int, options PlanOptions) (int, Shard, error) {
	end := start
	candidateEnd := start
	for end < len(records) && end-start < options.MaxRecords {
		end++
		shard := Shard{
			Base:         base,
			PrefixDigest: prefixDigest,
			Records:      records[start:end],
		}
		encoded, err := shard.MarshalBinary()
		if err != nil {
			if end == start+1 {
				return 0, Shard{}, fmt.Errorf("%w: one record cannot fit in a shard", ErrShardTooLarge)
			}
			end--
			break
		}
		if len(encoded) > options.MaxEncodedBytes {
			if end == start+1 {
				return 0, Shard{}, fmt.Errorf("%w: one record exceeds the configured envelope limit", ErrShardTooLarge)
			}
			end--
			break
		}
		candidateEnd = end
	}
	if candidateEnd == start {
		return 0, Shard{}, fmt.Errorf("%w: no record fits in a shard", ErrShardTooLarge)
	}
	return candidateEnd, Shard{
		Base:         base,
		PrefixDigest: prefixDigest,
		Records:      cloneRecords(records[start:candidateEnd]),
	}, nil
}
