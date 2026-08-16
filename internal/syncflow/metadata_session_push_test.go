package syncflow

import (
	"context"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestQueuedPusherPushSessionWithMetadataCanonicalizesBeforePublishing(t *testing.T) {
	fixture := newQueuedFixture(t, func(error) syncer.FailureClass {
		return syncer.FailureNetwork
	})
	fixture.remote.failures = 0
	data := adapter.SessionData{Records: [][]byte{
		[]byte(`{"cwd":"D:\\Source\\Project","message":{"content":"one"}}`),
	}}
	now := time.Date(2026, time.August, 14, 0, 0, 0, 0, time.UTC)
	next, err := fixture.pusher.PushSessionWithMetadataAt(
		context.Background(),
		fixture.key,
		data,
		adapter.PathSpace{ProjectRoot: `D:\Source\Project`, AgentHome: `D:\Source\Agent`},
		adapter.Installation{Compatibility: adapter.CompatFull},
		fixture.executor,
		fixture.cursor,
		[]byte(`{"fingerprint":"opaque"}`),
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if next.RecordCount != 1 || fixture.remote.puts != 2 {
		t.Fatalf("next = %+v, remote puts = %d, want shard and metadata", next, fixture.remote.puts)
	}
}
