package syncflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestQueuedPusherPushWrapperAndClassifierEnumValidation(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	next, err := fixture.pusher.Push(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if next.RecordCount != 1 {
		t.Fatalf("Push cursor = %+v", next)
	}

	invalid := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureClass("not-a-class")
	})
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := invalid.pusher.PushAt(context.Background(), invalid.key, invalid.stream, invalid.executor, invalid.cursor, now); !errors.Is(err, ErrInvalidFailureClassification) {
		t.Fatalf("unknown classifier error = %v, want ErrInvalidFailureClassification", err)
	}
}

func TestNewQueuedPusherRejectsInvalidQueueAndPrepareFilesystemFailure(t *testing.T) {
	classifier := FailureClassifier(func(error) syncer.FailureClass { return syncer.FailureNetwork })
	if _, err := NewQueuedPusher(syncer.QueueStore{}, syncer.DefaultRetryPolicy(), classifier); err == nil {
		t.Fatal("zero queue unexpectedly accepted")
	}

	root := t.TempDir()
	queue, err := syncer.NewQueueStore(root)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := NewQueuedPusher(queue, syncer.DefaultRetryPolicy(), classifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := syncer.NewQueueKey("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	stream := CanonicalStream{Records: [][]byte{[]byte(`{"ok":true}`)}}
	if _, err := pusher.PushAt(context.Background(), key, stream, syncer.AppendExecutor{}, syncer.NewPushCursor(), time.Now()); err == nil {
		t.Fatal("prepare filesystem failure unexpectedly succeeded")
	}
}
