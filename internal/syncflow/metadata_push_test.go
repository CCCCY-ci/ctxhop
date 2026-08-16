package syncflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestQueuedPusherPublishesMetadataAfterDurableShardPush(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	next, err := fixture.pusher.PushWithMetadataAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, []byte(`{"fingerprint":"opaque"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if next.RecordCount != 1 || fixture.remote.puts != 2 {
		t.Fatalf("next = %+v, remote puts = %d, want one shard and one metadata object", next, fixture.remote.puts)
	}
	objects, err := fixture.remote.List(context.Background(), "v1/projects/project/sessions/session")
	if err != nil {
		t.Fatal(err)
	}
	foundMetadata := false
	for _, object := range objects {
		if object.Key == "v1/projects/project/sessions/session/device/meta" {
			foundMetadata = true
		}
	}
	if !foundMetadata {
		t.Fatalf("remote objects = %+v, metadata object not found", objects)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after metadata success = %+v", snapshot.Items)
	}
}

func TestQueuedPusherRejectsMetadataBeforeRemoteWork(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	if _, err := fixture.pusher.PushWithMetadataAt(context.Background(), fixture.key, fixture.stream, fixture.executor, fixture.cursor, []byte("not json"), now); err == nil {
		t.Fatal("invalid metadata unexpectedly succeeded")
	}
	if fixture.remote.puts != 0 {
		t.Fatalf("invalid metadata caused %d remote puts", fixture.remote.puts)
	}
	snapshot, err := fixture.queue.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 0 {
		t.Fatalf("queue after invalid metadata = %+v", snapshot.Items)
	}
}

func TestCanonicalStreamPushWithMetadataChecksContextAndCursor(t *testing.T) {
	stream := CanonicalStream{Records: [][]byte{[]byte(`{"ok":true}`)}}
	if _, err := stream.PushWithMetadata(nil, syncer.AppendExecutor{}, syncer.NewPushCursor(), []byte(`{"ok":true}`)); err == nil {
		t.Fatal("nil context unexpectedly succeeded")
	}
	if _, err := stream.PushWithMetadata(context.Background(), syncer.AppendExecutor{}, syncer.PushCursor{}, []byte(`{"ok":true}`)); err == nil {
		t.Fatal("invalid cursor unexpectedly succeeded")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.PushWithMetadata(cancelled, syncer.AppendExecutor{}, syncer.NewPushCursor(), []byte(`{"ok":true}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PushWithMetadata error = %v, want context.Canceled", err)
	}
}
