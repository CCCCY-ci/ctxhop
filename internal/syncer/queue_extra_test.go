package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQueueKeyOrderingAndFailureEnums(t *testing.T) {
	keys := []QueueKey{
		{ProjectID: "p2", SessionID: "s", DeviceID: "d"},
		{ProjectID: "p1", SessionID: "s2", DeviceID: "d"},
		{ProjectID: "p1", SessionID: "s1", DeviceID: "d2"},
		{ProjectID: "p1", SessionID: "s1", DeviceID: "d1"},
	}
	if !keys[3].less(keys[2]) || !keys[2].less(keys[1]) || !keys[1].less(keys[0]) {
		t.Fatal("QueueKey.less does not order project, session, and device IDs")
	}
	if keys[0].less(keys[0]) {
		t.Fatal("QueueKey.less is not strict")
	}
	for _, failure := range []FailureClass{
		FailureNone,
		FailureNetwork,
		FailureUnknown,
		FailureCredentials,
		FailurePermission,
		FailureStorageFull,
		FailureSessionCorrupt,
		FailureExcluded,
	} {
		if err := failure.Validate(); err != nil {
			t.Fatalf("Validate(%q): %v", failure, err)
		}
	}
	if err := FailureClass("other").Validate(); err == nil {
		t.Fatal("unknown failure class unexpectedly validated")
	}
	if !FailureNetwork.Retryable() || !FailureUnknown.Retryable() || FailureCredentials.Retryable() {
		t.Fatal("failure retryability classification is incorrect")
	}
	if got := DefaultRetryPolicy(); got.BaseDelay <= 0 || got.MaxDelay < got.BaseDelay {
		t.Fatalf("DefaultRetryPolicy = %+v", got)
	}
}

func TestQueueSnapshotRejectsInvalidOperationsAndExhaustedRetry(t *testing.T) {
	key, err := NewQueueKey("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	badKey := QueueKey{ProjectID: "P", SessionID: "s", DeviceID: "d"}
	var snapshot QueueSnapshot
	if err := snapshot.Enqueue(badKey); err == nil {
		t.Fatal("Enqueue accepted an invalid key")
	}
	if err := snapshot.Complete(badKey); err == nil {
		t.Fatal("Complete accepted an invalid key")
	}
	if _, err := snapshot.RecordFailure(badKey, FailureNetwork, time.Now(), DefaultRetryPolicy()); err == nil {
		t.Fatal("RecordFailure accepted an invalid key")
	}
	if err := snapshot.Enqueue(key); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.RecordFailure(key, FailureNone, time.Now(), DefaultRetryPolicy()); err == nil {
		t.Fatal("RecordFailure accepted FailureNone")
	}
	if _, err := snapshot.RecordFailure(key, FailureClass("other"), time.Now(), DefaultRetryPolicy()); err == nil {
		t.Fatal("RecordFailure accepted an unknown failure class")
	}
	if _, err := snapshot.RecordFailure(key, FailureNetwork, time.Time{}, DefaultRetryPolicy()); err == nil {
		t.Fatal("RecordFailure accepted a zero retry time")
	}
	if _, err := snapshot.RecordFailure(key, FailureNetwork, time.Now(), RetryPolicy{}); err == nil {
		t.Fatal("RecordFailure accepted an invalid policy")
	}
	if _, err := snapshot.RecordFailure(QueueKey{ProjectID: "p", SessionID: "s", DeviceID: "e"}, FailureNetwork, time.Now(), DefaultRetryPolicy()); !errors.Is(err, ErrQueueItemMissing) {
		t.Fatalf("missing RecordFailure error = %v, want ErrQueueItemMissing", err)
	}

	exhausted := QueueSnapshot{Items: []QueueItem{{
		Key:           key,
		Attempt:       ^uint32(0),
		NextAttemptAt: time.Unix(1, 0).UTC(),
		State:         QueuePending,
		Failure:       FailureNetwork,
	}}}
	if _, err := exhausted.RecordFailure(key, FailureNetwork, time.Now(), DefaultRetryPolicy()); !errors.Is(err, ErrRetryExhausted) {
		t.Fatalf("exhausted RecordFailure error = %v, want ErrRetryExhausted", err)
	}

	q := QueueSnapshot{Items: []QueueItem{
		{Key: QueueKey{ProjectID: "p", SessionID: "s", DeviceID: "b"}, State: QueuePending, Failure: FailureNetwork, Attempt: 1, NextAttemptAt: time.Unix(20, 0).UTC()},
		{Key: QueueKey{ProjectID: "p", SessionID: "s", DeviceID: "a"}, State: QueuePending, Failure: FailureNetwork, Attempt: 1, NextAttemptAt: time.Unix(20, 0).UTC()},
	}}
	if got := q.Due(time.Unix(20, 0).UTC()); len(got) != 2 || got[0].Key.DeviceID != "a" || got[1].Key.DeviceID != "b" {
		t.Fatalf("same-time due order = %+v", got)
	}
}

func TestQueueStoreWrapperErrorsAndReadFailures(t *testing.T) {
	root := t.TempDir()
	store, err := NewQueueStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewQueueKey("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(context.Background(), QueueKey{ProjectID: "P", SessionID: "s", DeviceID: "d"}); err == nil {
		t.Fatal("wrapper Enqueue accepted an invalid key")
	}
	if err := store.Complete(context.Background(), QueueKey{ProjectID: "P", SessionID: "s", DeviceID: "d"}); err == nil {
		t.Fatal("wrapper Complete accepted an invalid key")
	}
	if _, err := store.RecordFailure(context.Background(), key, FailureCredentials, time.Time{}, DefaultRetryPolicy()); err != nil {
		t.Fatalf("wrapper terminal RecordFailure: %v", err)
	}
	if _, err := store.RecordFailure(context.Background(), key, FailureNetwork, time.Now(), DefaultRetryPolicy()); !errors.Is(err, ErrQueueItemBlocked) {
		t.Fatalf("wrapper blocked RecordFailure = %v, want ErrQueueItemBlocked", err)
	}
	if due, err := store.Due(context.Background(), time.Now()); err != nil || len(due) != 0 {
		t.Fatalf("wrapper Due blocked = %+v, %v", due, err)
	}

	badRoot := t.TempDir()
	badStore, err := NewQueueStore(badRoot)
	if err != nil {
		t.Fatal(err)
	}
	path, err := badStore.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := badStore.Load(context.Background()); err == nil {
		t.Fatal("Load directory unexpectedly succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Enqueue(ctx, key); err == nil {
		t.Fatal("cancelled wrapper Enqueue unexpectedly succeeded")
	}
	if err := store.Complete(ctx, key); err == nil {
		t.Fatal("cancelled wrapper Complete unexpectedly succeeded")
	}
	if _, err := store.RecordFailure(ctx, key, FailureNetwork, time.Now(), DefaultRetryPolicy()); err == nil {
		t.Fatal("cancelled wrapper RecordFailure unexpectedly succeeded")
	}
	if _, err := store.Due(ctx, time.Now()); err == nil {
		t.Fatal("cancelled wrapper Due unexpectedly succeeded")
	}
}
