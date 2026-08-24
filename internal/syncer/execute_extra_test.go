package syncer

import (
	"context"
	"errors"
	"testing"
)

func TestAppendExecutorRejectsInvalidAndCancelledExecutionContexts(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewAppendExecutor(&pushRemoteFake{}, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{[]byte(`{"ok":true}`)}

	if _, err := executor.Execute(nil, NewPushCursor(), records); err == nil {
		t.Fatal("Execute accepted a nil context")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Execute(cancelled, NewPushCursor(), records); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Execute error = %v, want context.Canceled", err)
	}
	lateCancellation := &cancelAfterFirstCheckContext{}
	if _, err := executor.Execute(lateCancellation, NewPushCursor(), records); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-plan cancellation error = %v, want context.Canceled", err)
	}
	if _, err := executor.Execute(context.Background(), NewPushCursor(), [][]byte{[]byte("not json")}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid record error = %v, want ErrInvalidRecord", err)
	}
}

func TestNewAppendExecutorRejectsCursorStoreWithoutRoot(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	state := CursorStore{layout: layout}
	if _, err := NewAppendExecutor(&pushRemoteFake{}, public, layout, state, DefaultPlanOptions()); err == nil {
		t.Fatal("NewAppendExecutor accepted a cursor store without a root")
	}
}
