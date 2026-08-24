package syncflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrQueueUpdate reports that the queue could not be updated after a push
	// attempt. The executor's cursor remains the source of durable progress.
	ErrQueueUpdate = errors.New("syncflow: pending queue update failed")

	// ErrQueuedPushNotDue reports that backoff has not elapsed for a pending
	// task.
	ErrQueuedPushNotDue = errors.New("syncflow: pending push is not due")

	// ErrFailureClassifierRequired reports a missing failure classifier.
	ErrFailureClassifierRequired = errors.New("syncflow: failure classifier is required")

	// ErrInvalidFailureClassification reports a classifier result outside the
	// queue's finite failure enum.
	ErrInvalidFailureClassification = errors.New("syncflow: invalid failure classification")
)

// FailureClassifier maps an execution error to a safe queue failure class.
// It must never inspect or persist session content, local paths, credentials,
// or remote addresses.
type FailureClassifier func(error) syncer.FailureClass

// QueuedPusher coordinates one canonical stream with durable retry metadata.
type QueuedPusher struct {
	queue    syncer.QueueStore
	policy   syncer.RetryPolicy
	classify FailureClassifier
}

// NewQueuedPusher validates a queue, retry policy, and explicit error
// classifier. The classifier is required so backend-specific failures are not
// guessed from human-readable error strings.
func NewQueuedPusher(queue syncer.QueueStore, policy syncer.RetryPolicy, classify FailureClassifier) (QueuedPusher, error) {
	if classify == nil {
		return QueuedPusher{}, ErrFailureClassifierRequired
	}
	if err := policy.Validate(); err != nil {
		return QueuedPusher{}, err
	}
	if _, err := queue.Load(context.Background()); err != nil {
		return QueuedPusher{}, fmt.Errorf("syncflow: validate pending queue: %w", err)
	}
	return QueuedPusher{
		queue:    queue,
		policy:   policy,
		classify: classify,
	}, nil
}

// Push runs a queued push using the current wall-clock time.
func (p QueuedPusher) Push(ctx context.Context, key syncer.QueueKey, stream CanonicalStream, executor syncer.AppendExecutor, cursor syncer.PushCursor) (syncer.PushCursor, error) {
	return p.PushAt(ctx, key, stream, executor, cursor, time.Now())
}

// PushAt runs a queued push at a caller-supplied time. It exists so scheduling
// and retry behavior can be deterministic for callers and tests.
func (p QueuedPusher) PushAt(ctx context.Context, key syncer.QueueKey, stream CanonicalStream, executor syncer.AppendExecutor, cursor syncer.PushCursor, now time.Time) (syncer.PushCursor, error) {
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

	if err := p.prepare(ctx, key, now); err != nil {
		return syncer.PushCursor{}, err
	}

	next, pushErr := stream.Push(ctx, executor, cursor)
	if pushErr != nil {
		if errors.Is(pushErr, context.Canceled) || errors.Is(pushErr, context.DeadlineExceeded) {
			return next, pushErr
		}
		failure := p.classify(pushErr)
		if err := failure.Validate(); err != nil || failure == syncer.FailureNone {
			if err == nil {
				err = errors.New("failure classifier returned FailureNone")
			}
			return next, fmt.Errorf("%w: %v", ErrInvalidFailureClassification, err)
		}
		if _, err := p.queue.RecordFailure(ctx, key, failure, now, p.policy); err != nil {
			return next, errors.Join(pushErr, fmt.Errorf("%w: record failure: %w", ErrQueueUpdate, err))
		}
		return next, pushErr
	}

	if err := p.queue.Complete(ctx, key); err != nil {
		return next, fmt.Errorf("%w: complete task: %w", ErrQueueUpdate, err)
	}
	return next, nil
}

func (p QueuedPusher) prepare(ctx context.Context, key syncer.QueueKey, now time.Time) error {
	err := p.queue.Update(ctx, func(snapshot *syncer.QueueSnapshot) error {
		item, err := snapshot.Item(key)
		if errors.Is(err, syncer.ErrQueueItemMissing) {
			return snapshot.Enqueue(key)
		}
		if err != nil {
			return err
		}
		if item.State == syncer.QueueBlocked {
			return syncer.ErrQueueItemBlocked
		}
		if !item.NextAttemptAt.IsZero() && item.NextAttemptAt.After(now) {
			return fmt.Errorf("%w: retry is scheduled for %s", ErrQueuedPushNotDue, item.NextAttemptAt.UTC().Format(time.RFC3339Nano))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("syncflow: update pending queue: %w", err)
	}
	return nil
}

// prepareSession preserves the normal scheduling check, but identifies a
// source-data task that may be safely reconsidered after the current snapshot
// has been canonicalized again. This repairs queue entries written while an
// Agent was still writing a session without reopening them before validation.
func (p QueuedPusher) prepareSession(ctx context.Context, key syncer.QueueKey, now time.Time) (syncer.FailureClass, error) {
	if err := p.prepare(ctx, key, now); err == nil {
		return syncer.FailureNone, nil
	} else if !errors.Is(err, syncer.ErrQueueItemBlocked) {
		return syncer.FailureNone, err
	}

	item, err := p.queue.Item(ctx, key)
	if err != nil {
		return syncer.FailureNone, fmt.Errorf("syncflow: inspect blocked task: %w", err)
	}
	switch item.Failure {
	case syncer.FailureExcluded, syncer.FailureSessionCorrupt:
		return item.Failure, nil
	default:
		return syncer.FailureNone, syncer.ErrQueueItemBlocked
	}
}

func (p QueuedPusher) reopenRevalidated(ctx context.Context, key syncer.QueueKey, failure syncer.FailureClass) error {
	if failure == syncer.FailureNone {
		return nil
	}
	if err := p.queue.Reopen(ctx, key, failure); err != nil {
		return fmt.Errorf("syncflow: reopen revalidated task: %w", err)
	}
	return nil
}
