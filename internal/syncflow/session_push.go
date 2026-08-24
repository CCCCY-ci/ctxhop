package syncflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// PushSession runs a strict adapter snapshot through canonicalization and the
// queued push flow using the current wall-clock time.
func (p QueuedPusher) PushSession(ctx context.Context, key syncer.QueueKey, data adapter.SessionData, space adapter.PathSpace, installation adapter.Installation, executor syncer.AppendExecutor, cursor syncer.PushCursor) (syncer.PushCursor, error) {
	return p.PushSessionAt(ctx, key, data, space, installation, executor, cursor, time.Now())
}

// PushSessionAt runs a strict adapter snapshot through canonicalization and
// the queued push flow at a caller-supplied time. Canonicalization refusals
// become terminal queue metadata and never reach the executor.
func (p QueuedPusher) PushSessionAt(ctx context.Context, key syncer.QueueKey, data adapter.SessionData, space adapter.PathSpace, installation adapter.Installation, executor syncer.AppendExecutor, cursor syncer.PushCursor, now time.Time) (syncer.PushCursor, error) {
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
	reopenFailure, err := p.prepareSession(ctx, key, now)
	if err != nil {
		return syncer.PushCursor{}, err
	}

	stream, err := CanonicalizeSession(data, space, installation)
	if err != nil {
		if reopenFailure != syncer.FailureNone {
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

	if err := p.reopenRevalidated(ctx, key, reopenFailure); err != nil {
		return syncer.PushCursor{}, err
	}

	return p.PushAt(ctx, key, stream, executor, cursor, now)
}

// ClassifySessionFailure maps known canonicalization refusals to terminal,
// non-secret queue classes. It intentionally has no retryable fallback: an
// unexpected canonicalization error is treated as a corrupt source rather
// than retried indefinitely.
func ClassifySessionFailure(err error) syncer.FailureClass {
	if err == nil {
		return syncer.FailureNone
	}
	switch {
	case errors.Is(err, ErrSessionNotPushable), errors.Is(err, ErrInvalidPathSpace):
		return syncer.FailureExcluded
	case errors.Is(err, ErrInvalidSessionSnapshot):
		return syncer.FailureSessionCorrupt
	default:
		return syncer.FailureSessionCorrupt
	}
}
