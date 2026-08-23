package syncflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

type cancelAfterContext struct {
	context.Context
	allowed int
	calls   int
}

func (c *cancelAfterContext) Err() error {
	c.calls++
	if c.calls > c.allowed {
		return context.Canceled
	}
	return nil
}

func TestQueuedPusherPrepareHandlesOtherTasksAndSaveCancellation(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	other, err := syncer.NewQueueKey("other", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.queue.Enqueue(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelAfterContext{Context: context.Background(), allowed: 3}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushAt(ctx, fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare cancellation error = %v, want context.Canceled", err)
	}
	if fixture.remote.puts != 0 {
		t.Fatalf("remote puts after prepare cancellation = %d, want 0", fixture.remote.puts)
	}
}
