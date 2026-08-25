package syncflow

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

type queuedFixture struct {
	pusher   QueuedPusher
	queue    syncer.QueueStore
	key      syncer.QueueKey
	stream   CanonicalStream
	executor syncer.AppendExecutor
	remote   *queuedFailRemote
	cursor   syncer.PushCursor
}

type queuedFailRemote struct {
	remote.Remote
	failures int
	puts     int
	err      error
}

func (r *queuedFailRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.puts++
	if r.failures > 0 {
		r.failures--
		return r.err
	}
	return r.Remote.Put(ctx, key, body, size)
}

func newQueuedFixture(t *testing.T, classify FailureClassifier) queuedFixture {
	t.Helper()
	dataKey := crypto.NewDataKey()
	t.Cleanup(dataKey.Close)
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := syncer.NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	backendRoot := t.TempDir()
	cleanupQueuedTempRoot(t, backendRoot)
	backend, err := remote.NewDir(backendRoot)
	if err != nil {
		t.Fatal(err)
	}
	failing := &queuedFailRemote{
		Remote:   backend,
		failures: 1,
		err:      errors.New("remote unavailable"),
	}
	stateRoot := t.TempDir()
	cleanupQueuedTempRoot(t, stateRoot)
	state, err := syncer.NewCursorStore(stateRoot, layout)
	if err != nil {
		t.Fatal(err)
	}
	cursor := syncer.NewPushCursor()
	if err := state.Save(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
	options := syncer.DefaultPlanOptions()
	options.MaxRecords = 1
	executor, err := syncer.NewAppendExecutor(failing, public, layout, state, options)
	if err != nil {
		t.Fatal(err)
	}
	queueRoot := t.TempDir()
	cleanupQueuedTempRoot(t, queueRoot)
	queue, err := syncer.NewQueueStore(queueRoot)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := NewQueuedPusher(queue, syncer.RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: time.Minute}, classify)
	if err != nil {
		t.Fatal(err)
	}
	key, err := syncer.NewQueueKey("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	return queuedFixture{
		pusher:   pusher,
		queue:    queue,
		key:      key,
		stream:   CanonicalStream{Records: [][]byte{[]byte(`{"ok":true}`)}},
		executor: executor,
		remote:   failing,
		cursor:   cursor,
	}
}

func cleanupQueuedTempRoot(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		var lastErr error
		for attempt := 0; attempt < 20; attempt++ {
			lastErr = os.RemoveAll(root)
			if lastErr == nil || errors.Is(lastErr, os.ErrNotExist) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Errorf("remove queued test temp root: %v", lastErr)
	})
}

func TestQueuedPusherBackoffShortCircuitAndSuccessfulCleanup(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)

	next, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now)
	if err == nil || !errors.Is(err, fixture.remote.err) {
		t.Fatalf("first PushAt error = %v, want remote error", err)
	}
	if next != fixture.cursor || fixture.remote.puts != 1 {
		t.Fatalf("failed push next = %+v, puts = %d", next, fixture.remote.puts)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].Attempt != 1 || snapshot.Items[0].Failure != syncer.FailureNetwork {
		t.Fatalf("retry queue = %+v", snapshot.Items)
	}
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now.Add(9*time.Second)); !errors.Is(err, ErrQueuedPushNotDue) {
		t.Fatalf("early retry error = %v, want ErrQueuedPushNotDue", err)
	}
	if fixture.remote.puts != 1 {
		t.Fatalf("early retry remote puts = %d, want 1", fixture.remote.puts)
	}

	fixture.remote.failures = 0
	next, err = fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now.Add(10*time.Second))
	if err != nil {
		t.Fatalf("successful retry: %v", err)
	}
	if next.RecordCount != 1 || next.NextShard != 2 || fixture.remote.puts != 2 {
		t.Fatalf("successful retry next = %+v, puts = %d", next, fixture.remote.puts)
	}
	snapshot, err = fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after success = %+v, want empty", snapshot.Items)
	}
}

func TestQueuedPusherBlocksTerminalFailuresAndLeavesCancelledTasksDue(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureCredentials
	})
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); err == nil || !errors.Is(err, fixture.remote.err) {
		t.Fatalf("terminal PushAt error = %v, want remote error", err)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].State != syncer.QueueBlocked || snapshot.Items[0].Failure != syncer.FailureCredentials {
		t.Fatalf("blocked queue = %+v", snapshot.Items)
	}
	puts := fixture.remote.puts
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); !errors.Is(err, syncer.ErrQueueItemBlocked) {
		t.Fatalf("blocked retry error = %v, want ErrQueueItemBlocked", err)
	}
	if fixture.remote.puts != puts {
		t.Fatalf("blocked retry remote puts = %d, want %d", fixture.remote.puts, puts)
	}

	cancelledFixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelledFixture.pusher.PushAt(cancelled, cancelledFixture.key, cancelledFixture.stream, cancelledFixture.executor, cancelledFixture.cursor, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PushAt error = %v, want context.Canceled", err)
	}
	snapshot, err = cancelledFixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("cancelled queue = %+v, want empty", snapshot.Items)
	}
}

func TestNewQueuedPusherAndClassifierValidation(t *testing.T) {
	queue, err := syncer.NewQueueStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	policy := syncer.DefaultRetryPolicy()
	if _, err := NewQueuedPusher(queue, policy, nil); !errors.Is(err, ErrFailureClassifierRequired) {
		t.Fatalf("nil classifier error = %v, want ErrFailureClassifierRequired", err)
	}
	if _, err := NewQueuedPusher(queue, syncer.RetryPolicy{}, func(error) syncer.FailureClass { return syncer.FailureNetwork }); err == nil {
		t.Fatal("invalid policy unexpectedly accepted")
	}
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNone
	})
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); !errors.Is(err, ErrInvalidFailureClassification) {
		t.Fatalf("invalid classifier error = %v, want ErrInvalidFailureClassification", err)
	}
	if _, err := fixture.pusher.PushAt(context.Background(), syncer.QueueKey{}, fixture.stream, fixture.executor, fixture.cursor, now); err == nil {
		t.Fatal("invalid key unexpectedly accepted")
	}
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, time.Time{}); err == nil {
		t.Fatal("zero queue time unexpectedly accepted")
	}
	if _, err := fixture.pusher.PushAt(nil, fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); err == nil {
		t.Fatal("nil context unexpectedly accepted")
	}
}
