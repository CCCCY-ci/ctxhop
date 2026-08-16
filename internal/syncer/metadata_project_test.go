package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestFetchProjectMetadataReadsOnlyMetadataObjects(t *testing.T) {
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
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	firstLayout, err := NewObjectLayout("projectone", "sessionone", "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	secondLayout, err := NewObjectLayout("projectone", "sessiontwo", "devicetwo")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewMetadata(3, [32]byte{1}, []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewMetadata(5, [32]byte{2}, []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := PutMetadata(context.Background(), store, public, firstLayout, first); err != nil {
		t.Fatal(err)
	}
	if err := PutMetadata(context.Background(), store, public, secondLayout, second); err != nil {
		t.Fatal(err)
	}

	objects, err := store.List(context.Background(), "v1/projects/projectone/sessions/sessionone")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("objects before shard fixture = %d, want one", len(objects))
	}
	shardKey, err := firstLayout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), shardKey, bytesReader([]byte("not metadata")), int64(len("not metadata"))); err != nil {
		t.Fatal(err)
	}

	groups, err := FetchProjectMetadata(context.Background(), store, "projectone", identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].SessionID != "sessionone" || groups[1].SessionID != "sessiontwo" {
		t.Fatalf("groups = %+v, want sorted session groups", groups)
	}
	if len(groups[0].Devices) != 1 || groups[0].Devices[0].DeviceID != "deviceone" {
		t.Fatalf("first devices = %+v", groups[0].Devices)
	}
	if groups[0].Devices[0].Metadata.RecordCount != 3 || groups[1].Devices[0].Metadata.RecordCount != 5 {
		t.Fatalf("metadata groups = %+v", groups)
	}
}

func TestFetchProjectMetadataMissingIsExplicit(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	_, err = FetchProjectMetadata(context.Background(), store, "projectone", identity)
	if !errors.Is(err, ErrNoRemoteMetadata) {
		t.Fatalf("error = %v, want ErrNoRemoteMetadata", err)
	}
}
