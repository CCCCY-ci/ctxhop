package syncer

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestQueueAdditionalValidationAndStrictTrailingDataBranches(t *testing.T) {
	key, err := NewQueueKey("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	if err := (QueueItem{Key: key, State: QueuePending, Failure: FailureClass("other")}).Validate(); err == nil {
		t.Fatal("QueueItem accepted an unknown failure class")
	}
	if err := (QueueItem{State: QueuePending}).Validate(); err == nil {
		t.Fatal("QueueItem accepted an invalid key")
	}

	store, err := NewQueueStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(nil); err == nil {
		t.Fatal("Load accepted a nil context")
	}
	if err := store.Save(nil, QueueSnapshot{}); err == nil {
		t.Fatal("Save accepted a nil context")
	}
	first, err := NewQueueKey("p", "s", "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewQueueKey("p", "s", "b")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot QueueSnapshot
	if err := snapshot.Enqueue(second); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Enqueue(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}

	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("saved queue is empty")
	}
	if err := os.WriteFile(path, append(data, []byte(" x")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrInvalidQueue) {
		t.Fatalf("malformed trailing data error = %v, want ErrInvalidQueue", err)
	}
}
