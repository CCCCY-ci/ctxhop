package syncflow

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

type queueUpdateRemote struct {
	remote.Remote
	err    error
	after  func()
	cancel context.CancelFunc
}

func (r *queueUpdateRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	err := r.Remote.Put(ctx, key, body, size)
	if r.after != nil {
		r.after()
	}
	if r.cancel != nil {
		r.cancel()
		return context.Canceled
	}
	if r.err != nil {
		return r.err
	}
	return err
}

type queueUpdateFixture struct {
	pusher   QueuedPusher
	queue    syncer.QueueStore
	root     string
	key      syncer.QueueKey
	stream   CanonicalStream
	executor syncer.AppendExecutor
	remote   *queueUpdateRemote
	cursor   syncer.PushCursor
}

func newQueueUpdateFixture(t *testing.T) queueUpdateFixture {
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
	backend, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backendRemote := &queueUpdateRemote{Remote: backend}
	state, err := syncer.NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	cursor := syncer.NewPushCursor()
	if err := state.Save(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
	executor, err := syncer.NewAppendExecutor(backendRemote, public, layout, state, syncer.DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	queue, err := syncer.NewQueueStore(root)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := NewQueuedPusher(queue, syncer.RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute}, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := syncer.NewQueueKey("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	return queueUpdateFixture{
		pusher:   pusher,
		queue:    queue,
		root:     root,
		key:      key,
		stream:   CanonicalStream{Records: [][]byte{[]byte(`{"ok":true}`)}},
		executor: executor,
		remote:   backendRemote,
		cursor:   cursor,
	}
}

func blockQueueState(t *testing.T, root string) {
	t.Helper()
	stateDir := filepath.Join(root, "state")
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueuedPusherReportsQueueFailureUpdateErrors(t *testing.T) {
	failed := newQueueUpdateFixture(t)
	failed.remote.err = errors.New("remote unavailable")
	failed.remote.after = func() { blockQueueState(t, failed.root) }
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	next, err := failed.pusher.PushAt(context.Background(), failed.key, failed.stream, failed.executor, failed.cursor, now)
	if err == nil || !errors.Is(err, ErrQueueUpdate) || !errors.Is(err, failed.remote.err) {
		t.Fatalf("failure update error = %v, want queue and remote errors", err)
	}
	if next != failed.cursor {
		t.Fatalf("cursor after queue failure = %+v, want %+v", next, failed.cursor)
	}

	completed := newQueueUpdateFixture(t)
	completed.remote.after = func() { blockQueueState(t, completed.root) }
	if _, err := completed.pusher.PushAt(context.Background(), completed.key, completed.stream, completed.executor, completed.cursor, now); err == nil || !errors.Is(err, ErrQueueUpdate) {
		t.Fatalf("complete update error = %v, want ErrQueueUpdate", err)
	}
}

func TestQueuedPusherDoesNotClassifyCancellationAfterRemoteStarts(t *testing.T) {
	fixture := newQueueUpdateFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.remote.cancel = cancel
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushAt(ctx, fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled execution error = %v, want context.Canceled", err)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 || snapshot.Items[0].Attempt != 0 || snapshot.Items[0].Failure != syncer.FailureNone {
		t.Fatalf("cancelled queue item = %+v, want an immediately due item", snapshot.Items)
	}
}

func TestQueuedPusherTreatsAlreadyCompletedQueueTaskAsSuccess(t *testing.T) {
	fixture := newQueueUpdateFixture(t)
	fixture.remote.after = func() {
		if err := fixture.queue.Complete(context.Background(), fixture.key); err != nil {
			t.Fatalf("concurrent queue completion: %v", err)
		}
	}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, now); err != nil {
		t.Fatalf("PushAt: %v", err)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after idempotent completion = %+v", snapshot.Items)
	}
}
