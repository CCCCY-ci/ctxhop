package syncflow

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestPlanPullExcludesLocalBranchAndSkipsObservedForeignTips(t *testing.T) {
	cursor := syncer.PushCursor{NextShard: 2, RecordCount: 1, HeadDigest: [32]byte{1}}
	local, err := syncer.NewMetadata(1, [32]byte{1}, []byte(`{"device":"local"}`))
	if err != nil {
		t.Fatal(err)
	}
	foreignObserved, err := syncer.NewMetadata(2, [32]byte{2}, []byte(`{"device":"observed"}`))
	if err != nil {
		t.Fatal(err)
	}
	foreignNew, err := syncer.NewMetadata(3, [32]byte{3}, []byte(`{"device":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanPull([]syncer.MetadataRef{
		{DeviceID: "deviceb", Metadata: foreignNew},
		{DeviceID: "devicea", Metadata: local},
		{DeviceID: "devicec", Metadata: foreignObserved},
	}, PullOptions{
		LocalDeviceID: "devicea",
		LocalCursor:   cursor,
		Observed: []RemoteTip{{
			DeviceID:    "devicec",
			RecordCount: 2,
			HeadDigest:  [32]byte{2},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasForeignChanges() {
		t.Fatal("foreign change was not reported")
	}
	if got := plan.ForeignDeviceIDs(); len(got) != 1 || got[0] != "deviceb" {
		t.Fatalf("foreign device IDs = %v, want [deviceb]", got)
	}
	if plan.LocalTip == nil || plan.LocalTip.DeviceID != "devicea" || plan.LocalTip.RecordCount != 1 {
		t.Fatalf("local tip = %+v", plan.LocalTip)
	}

	noChange, err := PlanPull([]syncer.MetadataRef{{DeviceID: "devicea", Metadata: local}}, PullOptions{
		LocalDeviceID: "devicea",
		LocalCursor:   cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if noChange.HasForeignChanges() || len(noChange.ForeignDeviceIDs()) != 0 {
		t.Fatalf("local-only pull plan = %+v", noChange)
	}
}

func TestPlanPullRejectsLocalMismatchAndInvalidTips(t *testing.T) {
	cursor := syncer.PushCursor{NextShard: 2, RecordCount: 1, HeadDigest: [32]byte{1}}
	localAhead, err := syncer.NewMetadata(2, [32]byte{2}, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanPull([]syncer.MetadataRef{{DeviceID: "devicea", Metadata: localAhead}}, PullOptions{LocalDeviceID: "devicea", LocalCursor: cursor}); !errors.Is(err, ErrLocalDeviceStateMismatch) {
		t.Fatalf("local ahead error = %v, want ErrLocalDeviceStateMismatch", err)
	}
	localDiverged, err := syncer.NewMetadata(1, [32]byte{9}, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanPull([]syncer.MetadataRef{{DeviceID: "devicea", Metadata: localDiverged}}, PullOptions{LocalDeviceID: "devicea", LocalCursor: cursor}); !errors.Is(err, ErrLocalDeviceStateMismatch) {
		t.Fatalf("local divergent error = %v, want ErrLocalDeviceStateMismatch", err)
	}
	localStale, err := syncer.NewMetadata(0, syncer.EmptyDigest(), []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := PlanPull([]syncer.MetadataRef{{DeviceID: "devicea", Metadata: localStale}}, PullOptions{LocalDeviceID: "devicea", LocalCursor: cursor}); err != nil || plan.HasForeignChanges() {
		t.Fatalf("stale local metadata plan = %+v, error = %v", plan, err)
	}

	validForeign, err := syncer.NewMetadata(1, [32]byte{4}, []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	invalidOptions := []PullOptions{
		{LocalDeviceID: "DeviceA", LocalCursor: cursor},
		{LocalDeviceID: "devicea", LocalCursor: syncer.PushCursor{}},
		{LocalDeviceID: "devicea", LocalCursor: cursor, Observed: []RemoteTip{{DeviceID: "devicea", RecordCount: 1, HeadDigest: [32]byte{1}}}},
		{LocalDeviceID: "devicea", LocalCursor: cursor, Observed: []RemoteTip{{DeviceID: "deviceb", RecordCount: 0, HeadDigest: [32]byte{1}}}},
	}
	for i, options := range invalidOptions {
		if _, err := PlanPull([]syncer.MetadataRef{{DeviceID: "deviceb", Metadata: validForeign}}, options); !errors.Is(err, ErrInvalidPullRequest) {
			t.Errorf("invalid options %d error = %v, want ErrInvalidPullRequest", i, err)
		}
	}
	if _, err := PlanPull([]syncer.MetadataRef{{DeviceID: "deviceb", Metadata: validForeign}, {DeviceID: "deviceb", Metadata: validForeign}}, PullOptions{LocalDeviceID: "devicea", LocalCursor: cursor}); !errors.Is(err, ErrInvalidPullRequest) {
		t.Fatalf("duplicate remote device error = %v, want ErrInvalidPullRequest", err)
	}
}

func TestFetchPullPlanReadsMetadataOnlyAndDoesNotFetchLocalOrForeignShards(t *testing.T) {
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
	localLayout, err := syncer.NewObjectLayout("project", "session", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	foreignLayout, err := syncer.NewObjectLayout("project", "session", "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	localMetadata, err := syncer.NewMetadata(1, [32]byte{1}, []byte(`{"device":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	foreignMetadata, err := syncer.NewMetadata(2, [32]byte{2}, []byte(`{"device":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	store := &pullRemoteFake{objects: map[string][]byte{}}
	putPullMetadata(t, store, localLayout, public, localMetadata)
	putPullMetadata(t, store, foreignLayout, public, foreignMetadata)
	localShardKey, err := localLayout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	foreignShardKey, err := foreignLayout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[localShardKey] = []byte("local shard body must not be read")
	store.objects[foreignShardKey] = []byte("foreign shard body must not be read")
	store.list = append(store.list, remote.ObjectInfo{Key: localShardKey}, remote.ObjectInfo{Key: foreignShardKey})

	plan, err := FetchPullPlan(context.Background(), store, "project", "session", private, PullOptions{
		LocalDeviceID: "devicea",
		LocalCursor:   syncer.PushCursor{NextShard: 2, RecordCount: 1, HeadDigest: [32]byte{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.ForeignDeviceIDs(); len(got) != 1 || got[0] != "deviceb" {
		t.Fatalf("foreign device IDs = %v, want [deviceb]", got)
	}
	for _, key := range store.getKeys {
		if key == localShardKey || key == foreignShardKey {
			t.Fatalf("pull metadata check read shard %q", key)
		}
	}
}

func TestFetchPullPlanTreatsMissingMetadataAsNoopAndChecksContext(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	options := PullOptions{
		LocalDeviceID: "devicea",
		LocalCursor:   syncer.NewPushCursor(),
	}
	store := &pullRemoteFake{objects: map[string][]byte{}}
	plan, err := FetchPullPlan(context.Background(), store, "project", "session", private, options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.HasForeignChanges() || plan.LocalDeviceID != "devicea" {
		t.Fatalf("missing metadata plan = %+v", plan)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchPullPlan(cancelled, store, "project", "session", private, options); err == nil {
		t.Fatal("cancelled FetchPullPlan unexpectedly succeeded")
	}
	if _, err := FetchPullPlan(nil, store, "project", "session", private, options); err == nil {
		t.Fatal("nil context FetchPullPlan unexpectedly succeeded")
	}
}

type pullRemoteFake struct {
	objects map[string][]byte
	list    []remote.ObjectInfo
	getKeys []string
}

func (f *pullRemoteFake) Name() string { return "pull-fake" }

func (f *pullRemoteFake) List(ctx context.Context, _ string) ([]remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]remote.ObjectInfo(nil), f.list...), nil
}

func (f *pullRemoteFake) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.getKeys = append(f.getKeys, key)
	body, ok := f.objects[key]
	if !ok {
		return nil, remote.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *pullRemoteFake) Put(context.Context, string, io.Reader, int64) error { return nil }

func (f *pullRemoteFake) Delete(context.Context, string) error { return nil }

func (f *pullRemoteFake) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, remote.ErrNotFound
}

func putPullMetadata(t *testing.T, store *pullRemoteFake, layout syncer.ObjectLayout, public *ecdh.PublicKey, metadata syncer.Metadata) {
	t.Helper()
	key, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := syncer.SealMetadata(public, key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[key] = sealed
	store.list = append(store.list, remote.ObjectInfo{Key: key, Size: int64(len(sealed))})
}
