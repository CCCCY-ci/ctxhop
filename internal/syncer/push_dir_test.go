package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestPlannedPartsPublishAndReadBackThroughDirectoryRemote(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`), []byte(`{"n":3}`)}
	plan, err := PlanAppend(NewPushCursor(), records, PlanOptions{MaxRecords: 2, MaxEncodedBytes: maxShardBytes})
	if err != nil {
		t.Fatalf("PlanAppend: %v", err)
	}
	cursor := NewPushCursor()
	for _, part := range plan.Parts {
		cursor, err = PutShard(context.Background(), store, public, layout, cursor, part)
		if err != nil {
			t.Fatalf("PutShard %d: %v", part.Number, err)
		}
	}
	if cursor != plan.Next {
		t.Fatalf("published cursor = %+v, want %+v", cursor, plan.Next)
	}
	branches, err := FetchBranches(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatalf("FetchBranches: %v", err)
	}
	if len(branches) != 1 || branches[0].DeviceID != "device" || !recordsEqual(branches[0].Records, records) {
		t.Fatalf("branches = %+v", branches)
	}

	if _, err := store.Stat(context.Background(), "v1/projects/project/sessions/session/device/000002"); err != nil {
		t.Fatalf("Stat final shard: %v", err)
	}
	if _, err := store.Stat(context.Background(), "v1/projects/project/sessions/session/device/000003"); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("Stat absent shard error = %v, want ErrNotFound", err)
	}
}
