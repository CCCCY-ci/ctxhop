package syncflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

// PushWithMetadata publishes a canonical stream and then publishes its
// durable tip metadata. The metadata step never causes a second shard read or
// a remote listing.
func (s CanonicalStream) PushWithMetadata(ctx context.Context, executor syncer.AppendExecutor, cursor syncer.PushCursor, payload []byte) (syncer.PushCursor, error) {
	if ctx == nil {
		return syncer.PushCursor{}, errors.New("syncflow: context is required")
	}
	if _, err := syncer.NewMetadata(cursor.RecordCount, cursor.HeadDigest, payload); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: prepare session metadata: %w", err)
	}
	next, err := s.Push(ctx, executor, cursor)
	if err != nil {
		return next, err
	}
	if err := executor.PublishMetadata(ctx, next, payload); err != nil {
		return next, err
	}
	return next, nil
}

// PushWithMetadata publishes a queued canonical stream and its metadata tip.
// A metadata failure is classified and retained in the same pending queue as
// a shard failure, so a retry can publish metadata without republishing the
// already durable suffix.
func (p QueuedPusher) PushWithMetadata(ctx context.Context, key syncer.QueueKey, stream CanonicalStream, executor syncer.AppendExecutor, cursor syncer.PushCursor, payload []byte) (syncer.PushCursor, error) {
	return p.PushWithMetadataAt(ctx, key, stream, executor, cursor, payload, time.Now())
}

// PushWithMetadataAt is the deterministic form of PushWithMetadata.
func (p QueuedPusher) PushWithMetadataAt(ctx context.Context, key syncer.QueueKey, stream CanonicalStream, executor syncer.AppendExecutor, cursor syncer.PushCursor, payload []byte, now time.Time) (syncer.PushCursor, error) {
	if ctx == nil {
		return syncer.PushCursor{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: queued push: %w", err)
	}
	if err := key.Validate(); err != nil {
		return syncer.PushCursor{}, err
	}
	if now.IsZero() {
		return syncer.PushCursor{}, errors.New("syncflow: queue time is required")
	}
	if _, err := syncer.NewMetadata(cursor.RecordCount, cursor.HeadDigest, payload); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: prepare session metadata: %w", err)
	}
	if err := p.prepare(ctx, key, now); err != nil {
		return syncer.PushCursor{}, err
	}

	next, pushErr := stream.Push(ctx, executor, cursor)
	if pushErr != nil {
		return next, p.recordMetadataPushFailure(ctx, key, pushErr, now)
	}
	if err := executor.PublishMetadata(ctx, next, payload); err != nil {
		return next, p.recordMetadataPushFailure(ctx, key, err, now)
	}
	if err := p.queue.Complete(ctx, key); err != nil {
		return next, fmt.Errorf("%w: complete task: %w", ErrQueueUpdate, err)
	}
	return next, nil
}

// PushSessionWithMetadata runs strict adapter canonicalization, queued shard
// publication, and metadata publication as one retryable task.
func (p QueuedPusher) PushSessionWithMetadata(ctx context.Context, key syncer.QueueKey, data adapter.SessionData, space adapter.PathSpace, installation adapter.Installation, executor syncer.AppendExecutor, cursor syncer.PushCursor, payload []byte) (syncer.PushCursor, error) {
	return p.PushSessionWithMetadataAt(ctx, key, data, space, installation, executor, cursor, payload, time.Now())
}

// PushSessionWithMetadataAt is the deterministic form of
// PushSessionWithMetadata.
func (p QueuedPusher) PushSessionWithMetadataAt(ctx context.Context, key syncer.QueueKey, data adapter.SessionData, space adapter.PathSpace, installation adapter.Installation, executor syncer.AppendExecutor, cursor syncer.PushCursor, payload []byte, now time.Time) (syncer.PushCursor, error) {
	if ctx == nil {
		return syncer.PushCursor{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: queued session push: %w", err)
	}
	if err := key.Validate(); err != nil {
		return syncer.PushCursor{}, err
	}
	if now.IsZero() {
		return syncer.PushCursor{}, errors.New("syncflow: queue time is required")
	}
	if _, err := syncer.NewMetadata(cursor.RecordCount, cursor.HeadDigest, payload); err != nil {
		return syncer.PushCursor{}, fmt.Errorf("syncflow: prepare session metadata: %w", err)
	}
	reopen, err := p.prepareSession(ctx, key, now)
	if err != nil {
		return syncer.PushCursor{}, err
	}

	stream, err := CanonicalizeSession(data, space, installation)
	if err != nil {
		if reopen {
			return syncer.PushCursor{}, err
		}
		failure := ClassifySessionFailure(err)
		if failure == syncer.FailureNone {
			return syncer.PushCursor{}, err
		}
		if _, queueErr := p.queue.RecordFailure(ctx, key, failure, now, p.policy); queueErr != nil {
			return syncer.PushCursor{}, errors.Join(err, fmt.Errorf("%w: record session failure: %w", ErrQueueUpdate, queueErr))
		}
		return syncer.PushCursor{}, err
	}

	if err := p.reopenExcluded(ctx, key, reopen); err != nil {
		return syncer.PushCursor{}, err
	}

	return p.PushWithMetadataAt(ctx, key, stream, executor, cursor, payload, now)
}

func (p QueuedPusher) recordMetadataPushFailure(ctx context.Context, key syncer.QueueKey, pushErr error, now time.Time) error {
	if errors.Is(pushErr, context.Canceled) || errors.Is(pushErr, context.DeadlineExceeded) {
		return pushErr
	}
	failure := p.classify(pushErr)
	if err := failure.Validate(); err != nil || failure == syncer.FailureNone {
		if err == nil {
			err = errors.New("failure classifier returned FailureNone")
		}
		return fmt.Errorf("%w: %v", ErrInvalidFailureClassification, err)
	}
	if _, err := p.queue.RecordFailure(ctx, key, failure, now, p.policy); err != nil {
		return errors.Join(pushErr, fmt.Errorf("%w: record failure: %w", ErrQueueUpdate, err))
	}
	return pushErr
}
