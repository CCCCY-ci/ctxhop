package syncflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestPushSessionAtCanonicalizesBeforeQueuedPublication(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"cwd":"/source/project","message":{"content":"hello"}}`)}}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	next, err := fixture.pusher.PushSessionAt(context.Background(), fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now)
	if err != nil {
		t.Fatalf("PushSessionAt: %v", err)
	}
	if next.RecordCount != 1 || fixture.remote.puts != 1 {
		t.Fatalf("next = %+v, remote puts = %d", next, fixture.remote.puts)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after valid session = %+v", snapshot.Items)
	}
}

func TestPushSessionAtRetainsTerminalCanonicalizationFailuresWithoutRemoteWrites(t *testing.T) {
	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	cases := []struct {
		name     string
		data     adapter.SessionData
		space    adapter.PathSpace
		install  adapter.Installation
		wantErr  error
		wantFail syncer.FailureClass
	}{
		{
			name:     "skipped records",
			data:     adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}, Skipped: 1},
			space:    space,
			install:  adapter.Installation{Compatibility: adapter.CompatFull},
			wantErr:  ErrInvalidSessionSnapshot,
			wantFail: syncer.FailureSessionCorrupt,
		},
		{
			name:     "invalid json",
			data:     adapter.SessionData{Records: [][]byte{[]byte(`{"ok":`)}},
			space:    space,
			install:  adapter.Installation{Compatibility: adapter.CompatFull},
			wantErr:  ErrInvalidSessionSnapshot,
			wantFail: syncer.FailureSessionCorrupt,
		},
		{
			name:     "unknown path-keyed container",
			data:     adapter.SessionData{Records: [][]byte{[]byte(`{"unknownContainer":{"/source/project/secret":true}}`)}},
			space:    space,
			install:  adapter.Installation{Compatibility: adapter.CompatFull},
			wantErr:  ErrSessionNotPushable,
			wantFail: syncer.FailureExcluded,
		},
		{
			name:     "stopped compatibility",
			data:     adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}},
			space:    space,
			install:  adapter.Installation{Compatibility: adapter.CompatStopped, CompatibilityReason: "schema is not understood"},
			wantErr:  ErrSessionNotPushable,
			wantFail: syncer.FailureExcluded,
		},
		{
			name:     "missing path space",
			data:     adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}},
			space:    adapter.PathSpace{AgentHome: space.AgentHome},
			install:  adapter.Installation{Compatibility: adapter.CompatFull},
			wantErr:  ErrInvalidPathSpace,
			wantFail: syncer.FailureExcluded,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
				return syncer.FailureNetwork
			})
			now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
			if _, err := fixture.pusher.PushSessionAt(context.Background(), fixture.key, tc.data, tc.space, tc.install, fixture.executor, fixture.cursor, now); err == nil || !errors.Is(err, tc.wantErr) {
				t.Fatalf("PushSessionAt error = %v, want %v", err, tc.wantErr)
			}
			if fixture.remote.puts != 0 {
				t.Fatalf("remote puts = %d, want 0", fixture.remote.puts)
			}
			snapshot, err := fixture.queue.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Items) != 1 || snapshot.Items[0].State != syncer.QueueBlocked || snapshot.Items[0].Failure != tc.wantFail {
				t.Fatalf("queue item = %+v, want blocked %q", snapshot.Items, tc.wantFail)
			}
		})
	}
}

func TestPushSessionAtHonorsSchedulingAndContextBeforeCanonicalization(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"ok":true}`)}}
	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); err == nil {
		t.Fatal("setup push unexpectedly succeeded")
	}
	puts := fixture.remote.puts
	if _, err := fixture.pusher.PushSessionAt(context.Background(), fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now.Add(time.Second)); !errors.Is(err, ErrQueuedPushNotDue) {
		t.Fatalf("not-due session error = %v, want ErrQueuedPushNotDue", err)
	}
	if fixture.remote.puts != puts {
		t.Fatalf("not-due session remote puts = %d, want %d", fixture.remote.puts, puts)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	other := newQueuedFixture(t, func(error) syncer.FailureClass { return syncer.FailureNetwork })
	if _, err := other.pusher.PushSessionAt(cancelled, other.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, other.executor, other.cursor, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled session error = %v, want context.Canceled", err)
	}
	snapshot, err := other.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 || other.remote.puts != 0 {
		t.Fatalf("cancelled session state = %+v, remote puts = %d", snapshot.Items, other.remote.puts)
	}
}

func TestClassifySessionFailureUsesFiniteTerminalClasses(t *testing.T) {
	if got := ClassifySessionFailure(nil); got != syncer.FailureNone {
		t.Fatalf("nil classification = %q, want FailureNone", got)
	}
	if got := ClassifySessionFailure(ErrSessionNotPushable); got != syncer.FailureExcluded {
		t.Fatalf("not-pushable classification = %q, want FailureExcluded", got)
	}
	if got := ClassifySessionFailure(ErrInvalidPathSpace); got != syncer.FailureExcluded {
		t.Fatalf("path-space classification = %q, want FailureExcluded", got)
	}
	if got := ClassifySessionFailure(ErrInvalidSessionSnapshot); got != syncer.FailureSessionCorrupt {
		t.Fatalf("snapshot classification = %q, want FailureSessionCorrupt", got)
	}
	if got := ClassifySessionFailure(errors.New("unexpected canonicalization error")); got != syncer.FailureSessionCorrupt {
		t.Fatalf("unknown classification = %q, want FailureSessionCorrupt", got)
	}
}

func TestPushSessionAtReopensExcludedAfterCanonicalization(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	policy := syncer.RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: time.Minute}
	if err := fixture.queue.Enqueue(context.Background(), fixture.key); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queue.RecordFailure(context.Background(), fixture.key, syncer.FailureExcluded, time.Time{}, policy); err != nil {
		t.Fatal(err)
	}

	space := adapter.PathSpace{ProjectRoot: `/source/project`, AgentHome: `/source/agent`}
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"newPathField":"/source/project/secret"}`)}}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushSessionAt(context.Background(), fixture.key, data, space, adapter.Installation{Compatibility: adapter.CompatFull}, fixture.executor, fixture.cursor, now); err != nil {
		t.Fatalf("PushSessionAt: %v", err)
	}
	if fixture.remote.puts != 1 {
		t.Fatalf("remote puts = %d, want 1", fixture.remote.puts)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after revalidated success = %+v", snapshot.Items)
	}
}
