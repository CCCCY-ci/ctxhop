package syncflow

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestQueuedPusherRetriesMetadataWithoutRepublishingDurableShard(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
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
	metadataKey, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	remoteStore := &metadataOnlyFailRemote{
		Remote:      backend,
		MetadataKey: metadataKey,
		Failure:     errors.New("metadata unavailable"),
		Fail:        true,
	}
	state, err := syncer.NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	cursor := syncer.NewPushCursor()
	if err := state.Save(context.Background(), cursor); err != nil {
		t.Fatal(err)
	}
	executor, err := syncer.NewAppendExecutor(remoteStore, public, layout, state, syncer.DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := syncer.NewQueueStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := NewQueuedPusher(queue, syncer.RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: time.Minute}, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := syncer.NewQueueKey("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	stream := CanonicalStream{Records: [][]byte{[]byte(`{"ok":true}`)}}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	next, err := pusher.PushWithMetadataAt(context.Background(), key, stream, executor, cursor, []byte(`{"fingerprint":"opaque"}`), now)
	if err == nil || !errors.Is(err, remoteStore.Failure) {
		t.Fatalf("first metadata publish error = %v, want metadata failure", err)
	}
	if next.RecordCount != 1 || remoteStore.ShardPuts != 1 || remoteStore.MetadataPuts != 1 {
		t.Fatalf("first attempt next = %+v, shard puts = %d, metadata puts = %d", next, remoteStore.ShardPuts, remoteStore.MetadataPuts)
	}
	loaded, err := state.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded != next {
		t.Fatalf("durable cursor = %+v, returned = %+v", loaded, next)
	}

	remoteStore.Fail = false
	retried, err := pusher.PushWithMetadataAt(context.Background(), key, stream, executor, next, []byte(`{"fingerprint":"opaque"}`), now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retried != next || remoteStore.ShardPuts != 1 || remoteStore.MetadataPuts != 2 {
		t.Fatalf("retry cursor = %+v, shard puts = %d, metadata puts = %d", retried, remoteStore.ShardPuts, remoteStore.MetadataPuts)
	}
	snapshot, err := queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after metadata retry = %+v", snapshot.Items)
	}
}

type metadataOnlyFailRemote struct {
	remote.Remote
	MetadataKey  string
	Failure      error
	Fail         bool
	ShardPuts    int
	MetadataPuts int
}

func (r *metadataOnlyFailRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if key == r.MetadataKey {
		r.MetadataPuts++
		if r.Fail {
			return r.Failure
		}
	} else {
		r.ShardPuts++
	}
	return r.Remote.Put(ctx, key, body, size)
}
