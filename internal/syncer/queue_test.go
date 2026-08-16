package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueSnapshotBackoffTerminalStateAndDueOrdering(t *testing.T) {
	first, err := NewQueueKey("project", "session", "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewQueueKey("project", "session", "b")
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
	if err := snapshot.Enqueue(first); err != nil {
		t.Fatalf("idempotent Enqueue: %v", err)
	}
	if len(snapshot.Items) != 2 {
		t.Fatalf("queue length = %d, want 2", len(snapshot.Items))
	}

	now := time.Date(2026, time.August, 14, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	policy := RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: 25 * time.Second}
	item, err := snapshot.RecordFailure(first, FailureNetwork, now, policy)
	if err != nil {
		t.Fatalf("first RecordFailure: %v", err)
	}
	if item.Attempt != 1 || !item.NextAttemptAt.Equal(now.UTC().Add(10*time.Second)) {
		t.Fatalf("first retry item = %+v", item)
	}
	if got := snapshot.Due(now.Add(9 * time.Second)); len(got) != 1 || got[0].Key != second {
		t.Fatalf("due before first retry = %+v, want only second", got)
	}
	if got := snapshot.Due(now.Add(10 * time.Second)); len(got) != 2 || got[0].Key != second || got[1].Key != first {
		t.Fatalf("due at first retry = %+v, want initial item before retry", got)
	}

	item, err = snapshot.RecordFailure(first, FailureUnknown, now.Add(10*time.Second), policy)
	if err != nil {
		t.Fatalf("second RecordFailure: %v", err)
	}
	if item.Attempt != 2 || !item.NextAttemptAt.Equal(now.UTC().Add(30*time.Second)) {
		t.Fatalf("second retry item = %+v", item)
	}
	item, err = snapshot.RecordFailure(first, FailureNetwork, now.Add(30*time.Second), policy)
	if err != nil {
		t.Fatalf("third RecordFailure: %v", err)
	}
	if item.Attempt != 3 || !item.NextAttemptAt.Equal(now.UTC().Add(55*time.Second)) {
		t.Fatalf("capped retry item = %+v", item)
	}

	blocked, err := snapshot.RecordFailure(second, FailureCredentials, time.Time{}, policy)
	if err != nil {
		t.Fatalf("terminal RecordFailure: %v", err)
	}
	if blocked.State != QueueBlocked || blocked.Failure != FailureCredentials || !blocked.NextAttemptAt.IsZero() {
		t.Fatalf("blocked item = %+v", blocked)
	}
	if got := snapshot.Due(now.Add(10 * time.Minute)); len(got) != 1 || got[0].Key != first {
		t.Fatalf("due includes blocked item or omits retry item = %+v", got)
	}
	if _, err := snapshot.RecordFailure(second, FailureNetwork, now, policy); !errors.Is(err, ErrQueueItemBlocked) {
		t.Fatalf("blocked retry error = %v, want ErrQueueItemBlocked", err)
	}
	if err := snapshot.Complete(first); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := snapshot.Complete(first); !errors.Is(err, ErrQueueItemMissing) {
		t.Fatalf("second Complete error = %v, want ErrQueueItemMissing", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("final Validate: %v", err)
	}
}

func TestRetryPolicyAndQueueStateValidation(t *testing.T) {
	policy := RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: 25 * time.Second}
	for attempt, want := range map[uint32]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 25 * time.Second, 100: 25 * time.Second} {
		got, err := policy.Delay(attempt)
		if err != nil {
			t.Fatalf("Delay(%d): %v", attempt, err)
		}
		if got != want {
			t.Fatalf("Delay(%d) = %s, want %s", attempt, got, want)
		}
	}
	for name, invalid := range map[string]RetryPolicy{
		"zero base":      {MaxDelay: time.Second},
		"max below base": {BaseDelay: 2 * time.Second, MaxDelay: time.Second},
		"max too large":  {BaseDelay: time.Second, MaxDelay: 366 * 24 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invalid.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
	if _, err := policy.Delay(0); err == nil {
		t.Fatal("Delay(0) unexpectedly succeeded")
	}

	key := QueueKey{ProjectID: "p", SessionID: "s", DeviceID: "d"}
	validTime := time.Unix(10, 0).UTC()
	cases := map[string]QueueItem{
		"empty state":        {Key: key},
		"pending terminal":   {Key: key, State: QueuePending, Failure: FailurePermission},
		"pending no retry":   {Key: key, State: QueuePending, Failure: FailureNetwork},
		"blocked no failure": {Key: key, State: QueueBlocked},
		"blocked retryable":  {Key: key, State: QueueBlocked, Failure: FailureNetwork},
		"blocked with time":  {Key: key, State: QueueBlocked, Failure: FailurePermission, NextAttemptAt: validTime},
		"unknown state":      {Key: key, State: QueueState("other")},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			if err := item.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
	if err := (QueueSnapshot{Items: []QueueItem{
		{Key: key, State: QueuePending},
		{Key: key, State: QueuePending},
	}}).Validate(); err == nil {
		t.Fatal("duplicate queue items unexpectedly validated")
	}

	var nilSnapshot *QueueSnapshot
	if err := nilSnapshot.Enqueue(key); err == nil {
		t.Fatal("nil Enqueue unexpectedly succeeded")
	}
	if err := nilSnapshot.Complete(key); err == nil {
		t.Fatal("nil Complete unexpectedly succeeded")
	}
	if _, err := nilSnapshot.RecordFailure(key, FailureNetwork, validTime, policy); err == nil {
		t.Fatal("nil RecordFailure unexpectedly succeeded")
	}
}

func TestQueueStoreRoundTripStrictlyAndAtomically(t *testing.T) {
	root := t.TempDir()
	store, err := NewQueueStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(context.Background()); err != nil || len(got.Items) != 0 {
		t.Fatalf("missing queue Load = %+v, %v", got, err)
	}
	key, err := NewQueueKey("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(context.Background(), key); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.Enqueue(context.Background(), key); err != nil {
		t.Fatalf("idempotent Enqueue: %v", err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"version":1,"items":[{"projectId":"project","sessionId":"session","deviceId":"device","attempt":0,"nextAttemptAt":"","state":"pending","failure":""}]}` + "\n")
	if !bytes.Equal(data, want) {
		t.Fatalf("queue file = %q, want %q", data, want)
	}

	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.FixedZone("local", 8*60*60))
	policy := RetryPolicy{BaseDelay: time.Minute, MaxDelay: time.Hour}
	item, err := store.RecordFailure(context.Background(), key, FailureNetwork, now, policy)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if item.Attempt != 1 || !item.NextAttemptAt.Equal(now.UTC().Add(time.Minute)) {
		t.Fatalf("stored retry item = %+v", item)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load retry: %v", err)
	}
	if len(loaded.Items) != 1 || loaded.Items[0] != item {
		t.Fatalf("loaded retry = %+v, want %+v", loaded.Items, item)
	}
	if due, err := store.Due(context.Background(), now.Add(time.Minute)); err != nil || len(due) != 1 || due[0] != item {
		t.Fatalf("Due = %+v, %v", due, err)
	}
	if err := store.Complete(context.Background(), key); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := store.Complete(context.Background(), key); !errors.Is(err, ErrQueueItemMissing) {
		t.Fatalf("second Complete error = %v, want ErrQueueItemMissing", err)
	}
	loaded, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load empty queue: %v", err)
	}
	if len(loaded.Items) != 0 {
		t.Fatalf("empty queue items = %+v", loaded.Items)
	}
	if leftovers, err := filepath.Glob(path + ".*.tmp"); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary queue files = %v, glob error = %v", leftovers, err)
	}
}

func TestQueueStoreRejectsDamagedVersionsAndUnknownFields(t *testing.T) {
	store, err := NewQueueStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), QueueSnapshot{}); err != nil {
		t.Fatal(err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	empty := `{"version":1,"items":[]}`
	pending := `{"version":1,"items":[{"projectId":"p","sessionId":"s","deviceId":"d","attempt":1,"nextAttemptAt":"2026-08-14T00:00:01Z","state":"pending","failure":"network"}]}`
	blocked := `{"version":1,"items":[{"projectId":"p","sessionId":"s","deviceId":"d","attempt":0,"nextAttemptAt":"","state":"blocked","failure":"credentials"}]}`
	duplicate := `{"version":1,"items":[{"projectId":"p","sessionId":"s","deviceId":"d","attempt":0,"nextAttemptAt":"","state":"pending","failure":""},{"projectId":"p","sessionId":"s","deviceId":"d","attempt":0,"nextAttemptAt":"","state":"pending","failure":""}]}`
	cases := map[string]struct {
		content string
		want    error
	}{
		"invalid json":            {content: "not json", want: ErrInvalidQueue},
		"unknown field":           {content: empty[:len(empty)-1] + `,"extra":true}`, want: ErrInvalidQueue},
		"trailing value":          {content: empty + ` {}`, want: ErrInvalidQueue},
		"future version":          {content: strings.Replace(empty, `"version":1`, `"version":2`, 1), want: ErrUnsupportedQueue},
		"old version":             {content: strings.Replace(empty, `"version":1`, `"version":0`, 1), want: ErrInvalidQueue},
		"bad time":                {content: strings.Replace(pending, `2026-08-14T00:00:01Z`, "not time", 1), want: ErrInvalidQueue},
		"invalid identifier":      {content: strings.Replace(pending, `"projectId":"p"`, `"projectId":"P"`, 1), want: ErrInvalidQueue},
		"unknown state":           {content: strings.Replace(pending, `"state":"pending"`, `"state":"other"`, 1), want: ErrInvalidQueue},
		"blocked missing failure": {content: strings.Replace(blocked, `"failure":"credentials"`, `"failure":""`, 1), want: ErrInvalidQueue},
		"duplicate item":          {content: duplicate, want: ErrInvalidQueue},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestQueueStoreValidatesDependenciesCancellationAndFilesystemErrors(t *testing.T) {
	if _, err := NewQueueStore(" "); err == nil {
		t.Fatal("NewQueueStore accepted an empty root")
	}
	var zero QueueStore
	if _, err := zero.Load(context.Background()); err == nil {
		t.Fatal("zero store Load unexpectedly succeeded")
	}
	if err := zero.Save(context.Background(), QueueSnapshot{}); err == nil {
		t.Fatal("zero store Save unexpectedly succeeded")
	}
	store, err := NewQueueStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(cancelled); err == nil {
		t.Fatal("cancelled Load unexpectedly succeeded")
	}
	if err := store.Save(cancelled, QueueSnapshot{}); err == nil {
		t.Fatal("cancelled Save unexpectedly succeeded")
	}
	if err := store.Save(context.Background(), QueueSnapshot{Items: []QueueItem{{
		Key: QueueKey{ProjectID: "p", SessionID: "s", DeviceID: "d"},
	}}}); err == nil {
		t.Fatal("invalid zero-state item unexpectedly saved")
	}

	brokenRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenRoot, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := NewQueueStore(brokenRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = broken.Save(context.Background(), QueueSnapshot{})
	if err == nil || strings.Contains(err.Error(), brokenRoot) {
		t.Fatalf("filesystem error = %v, expected a redacted failure", err)
	}
}

func TestNewQueueKeyUsesObjectIdentifierRules(t *testing.T) {
	cases := []struct {
		name    string
		project string
		session string
		device  string
	}{
		{name: "empty project"},
		{name: "uppercase", project: "P", session: "s", device: "d"},
		{name: "separator", project: "p", session: "s-x", device: "d"},
		{name: "too long", project: strings.Repeat("p", 129), session: "s", device: "d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewQueueKey(tc.project, tc.session, tc.device); err == nil {
				t.Fatal("NewQueueKey unexpectedly succeeded")
			}
		})
	}
}
