package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestAppendExecutorPublishesAndCommitsEachShard(t *testing.T) {
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
	initial := NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	remoteStore := &pushRemoteFake{objects: make(map[string][]byte)}
	executor, err := NewAppendExecutor(
		remoteStore,
		public,
		layout,
		state,
		PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes},
	)
	if err != nil {
		t.Fatalf("NewAppendExecutor: %v", err)
	}
	records := [][]byte{
		[]byte(`{"n":1,"message":"first"}`),
		[]byte(`{"n":2,"message":"second"}`),
		[]byte(`{"n":3,"message":"third"}`),
	}

	got, err := executor.Execute(context.Background(), initial, records)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want, err := PlanAppend(initial, records, PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes})
	if err != nil {
		t.Fatal(err)
	}
	if got != want.Next {
		t.Fatalf("final cursor = %+v, want %+v", got, want.Next)
	}
	loaded, err := state.Load(context.Background())
	if err != nil {
		t.Fatalf("Load committed cursor: %v", err)
	}
	if loaded != got {
		t.Fatalf("loaded cursor = %+v, want %+v", loaded, got)
	}
	if len(remoteStore.puts) != len(want.Parts) {
		t.Fatalf("remote writes = %d, want %d", len(remoteStore.puts), len(want.Parts))
	}
	for _, write := range remoteStore.puts {
		for _, record := range records {
			if bytes.Contains(write.body, record) {
				t.Fatalf("remote object %q contains plaintext record", write.key)
			}
		}
	}
}

func TestAppendExecutorReturnsLastDurableCursorOnRemoteFailure(t *testing.T) {
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
	initial := NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	putErr := errors.New("remote unavailable")
	remoteStore := &pushRemoteFake{objects: make(map[string][]byte), putErr: putErr}
	executor, err := NewAppendExecutor(remoteStore, public, layout, state, PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes})
	if err != nil {
		t.Fatal(err)
	}

	got, err := executor.Execute(context.Background(), initial, [][]byte{[]byte(`{"n":1}`)})
	if err == nil || !errors.Is(err, putErr) {
		t.Fatalf("Execute error = %v, want remote failure", err)
	}
	if got != initial {
		t.Fatalf("failed execution cursor = %+v, want %+v", got, initial)
	}
	loaded, err := state.Load(context.Background())
	if err != nil {
		t.Fatalf("Load cursor after remote failure: %v", err)
	}
	if loaded != initial {
		t.Fatalf("cursor after remote failure = %+v, want %+v", loaded, initial)
	}
}

func TestAppendExecutorKeepsOldCursorWhenCursorCommitFailsAndCanRetry(t *testing.T) {
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
	root := t.TempDir()
	state, err := NewCursorStore(root, layout)
	if err != nil {
		t.Fatal(err)
	}
	initial := NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	statePathBlocker := filepath.Join(root, "state")
	if err := os.RemoveAll(statePathBlocker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePathBlocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteStore := &pushRemoteFake{objects: make(map[string][]byte)}
	executor, err := NewAppendExecutor(remoteStore, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{[]byte(`{"n":1}`)}

	got, err := executor.Execute(context.Background(), initial, records)
	if err == nil || !errors.Is(err, ErrCursorCommit) {
		t.Fatalf("Execute error = %v, want ErrCursorCommit", err)
	}
	if got != initial {
		t.Fatalf("cursor after failed commit = %+v, want %+v", got, initial)
	}
	if len(remoteStore.puts) != 1 || len(remoteStore.objects) != 1 {
		t.Fatalf("remote after failed commit = %d writes, %d objects", len(remoteStore.puts), len(remoteStore.objects))
	}

	if err := os.Remove(statePathBlocker); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	got, err = executor.Execute(context.Background(), initial, records)
	if err != nil {
		t.Fatalf("retry Execute: %v", err)
	}
	if got.RecordCount != 1 || got.NextShard != 2 {
		t.Fatalf("retry cursor = %+v", got)
	}
	if len(remoteStore.puts) != 2 || len(remoteStore.objects) != 1 {
		t.Fatalf("retry remote = %d writes, %d objects", len(remoteStore.puts), len(remoteStore.objects))
	}
}

func TestAppendExecutorReportsCancellationAfterRemoteBeforeCursorSave(t *testing.T) {
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
	initial := NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	remoteStore := &cancelAfterPutRemote{
		Remote: &pushRemoteFake{objects: make(map[string][]byte)},
		cancel: cancel,
	}
	executor, err := NewAppendExecutor(remoteStore, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}

	got, err := executor.Execute(ctx, initial, [][]byte{[]byte(`{"n":1}`)})
	if err == nil || !errors.Is(err, ErrCursorCommit) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute error = %v, want cursor commit and cancellation", err)
	}
	if got != initial {
		t.Fatalf("cancelled execution cursor = %+v, want %+v", got, initial)
	}
	underlying := remoteStore.Remote.(*pushRemoteFake)
	if len(underlying.puts) != 1 {
		t.Fatalf("remote writes = %d, want 1", len(underlying.puts))
	}
}

func TestNewAppendExecutorValidatesDependenciesAndLayout(t *testing.T) {
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
	otherLayout, err := NewObjectLayout("other", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	otherState, err := NewCursorStore(t.TempDir(), otherLayout)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		store     remote.Remote
		recipient interface{}
		layout    ObjectLayout
		state     CursorStore
		options   PlanOptions
		want      error
	}{
		{name: "nil remote", recipient: public, layout: layout, state: state, options: DefaultPlanOptions()},
		{name: "nil recipient", store: &pushRemoteFake{}, layout: layout, state: state, options: DefaultPlanOptions()},
		{name: "invalid layout", store: &pushRemoteFake{}, recipient: public, layout: ObjectLayout{}, state: state, options: DefaultPlanOptions()},
		{name: "invalid options", store: &pushRemoteFake{}, recipient: public, layout: layout, state: state, options: PlanOptions{MaxRecords: 0, MaxEncodedBytes: maxShardBytes}},
		{name: "mismatched state", store: &pushRemoteFake{}, recipient: public, layout: layout, state: otherState, options: DefaultPlanOptions(), want: ErrExecutorLayoutMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recipient := public
			if tc.recipient == nil {
				recipient = nil
			}
			_, err := NewAppendExecutor(tc.store, recipient, tc.layout, tc.state, tc.options)
			if err == nil {
				t.Fatal("NewAppendExecutor unexpectedly succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := NewAppendExecutor(&pushRemoteFake{}, public, layout, CursorStore{}, DefaultPlanOptions()); err == nil {
		t.Fatal("NewAppendExecutor accepted zero cursor store")
	}
}

func TestAppendExecutorEmptySuffixDoesNotRewriteState(t *testing.T) {
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
	initial := NewPushCursor()
	if err := state.Save(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	path, err := state.filePath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	remoteStore := &pushRemoteFake{objects: make(map[string][]byte)}
	executor, err := NewAppendExecutor(remoteStore, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	got, err := executor.Execute(context.Background(), initial, nil)
	if err != nil {
		t.Fatalf("Execute empty suffix: %v", err)
	}
	if got != initial || len(remoteStore.puts) != 0 {
		t.Fatalf("empty execution cursor = %+v, writes = %d", got, len(remoteStore.puts))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("empty execution rewrote the cursor state")
	}
}

type cancelAfterPutRemote struct {
	remote.Remote
	cancel context.CancelFunc
	puts   int
}

func (r *cancelAfterPutRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	err := r.Remote.Put(ctx, key, body, size)
	if err == nil {
		r.puts++
		if r.puts == 1 {
			r.cancel()
		}
	}
	return err
}

var _ remote.Remote = (*cancelAfterPutRemote)(nil)
