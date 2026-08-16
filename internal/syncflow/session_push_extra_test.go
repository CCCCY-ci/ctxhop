package syncflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestPushSessionWrapperAndArgumentPreconditions(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass { return syncer.FailureNetwork })
	fixture.remote.failures = 0
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}}
	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	next, err := fixture.pusher.PushSession(context.Background(), fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor)
	if err != nil {
		t.Fatalf("PushSession: %v", err)
	}
	if next.RecordCount != 1 {
		t.Fatalf("PushSession cursor = %+v", next)
	}

	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushSessionAt(nil, fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now); err == nil {
		t.Fatal("PushSessionAt accepted nil context")
	}
	if _, err := fixture.pusher.PushSessionAt(context.Background(), syncer.QueueKey{}, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now); err == nil {
		t.Fatal("PushSessionAt accepted invalid key")
	}
	if _, err := fixture.pusher.PushSessionAt(context.Background(), fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, time.Time{}); err == nil {
		t.Fatal("PushSessionAt accepted zero time")
	}
}

func TestPushSessionAtReportsQueueUpdateFailureAfterCanonicalizationRefusal(t *testing.T) {
	fixture := newQueueUpdateFixture(t)
	ctx := &cancelAfterContext{Context: context.Background(), allowed: 6}
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"ok":`)}}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushSessionAt(ctx, fixture.key, data, adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now); err == nil || !errors.Is(err, ErrInvalidSessionSnapshot) || !errors.Is(err, ErrQueueUpdate) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canonicalization queue update error = %v, want snapshot, queue, and cancellation", err)
	}
}
