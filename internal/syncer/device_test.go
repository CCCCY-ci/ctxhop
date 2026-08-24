package syncer

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestDeviceRecordTransportAndExactKeyBinding(t *testing.T) {
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

	lastActive := time.Date(2026, 8, 15, 2, 3, 4, 123456789, time.FixedZone("SGT", 8*60*60))
	record, err := NewDeviceRecord("devicea", "workstation", "windows", lastActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := PutDeviceRecord(context.Background(), store, public, record); err != nil {
		t.Fatalf("PutDeviceRecord: %v", err)
	}
	records, err := FetchDeviceRecords(context.Background(), store, identity)
	if err != nil {
		t.Fatalf("FetchDeviceRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d device records, want 1", len(records))
	}
	if records[0].DeviceID != record.DeviceID || records[0].Name != record.Name || records[0].System != record.System {
		t.Fatalf("record = %+v, want %+v", records[0], record)
	}
	if !records[0].LastActiveAt.Equal(lastActive.UTC()) {
		t.Fatalf("last active = %s, want %s", records[0].LastActiveAt, lastActive.UTC())
	}

	key, err := DeviceKey(record.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealDeviceRecord(public, key, record)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := DeviceKey("deviceb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDeviceRecord(identity, wrongKey, sealed); err == nil {
		t.Fatal("OpenDeviceRecord accepted ciphertext at a different key")
	}
}

func TestDeviceRecordParsingRejectsNewerAndTrailingData(t *testing.T) {
	lastActive := "2026-08-15T02:03:04Z"
	newer := []byte("{\"version\":2,\"name\":\"workstation\",\"system\":\"windows\",\"lastActiveAt\":\"" + lastActive + "\"}")
	if _, err := parseDeviceRecord("devicea", newer); !errors.Is(err, ErrUnsupportedDeviceRecord) {
		t.Fatalf("newer record = %v, want ErrUnsupportedDeviceRecord", err)
	}

	trailing := []byte("{\"version\":1,\"name\":\"workstation\",\"system\":\"windows\",\"lastActiveAt\":\"" + lastActive + "\"} {}")
	if _, err := parseDeviceRecord("devicea", trailing); err == nil || !errors.Is(err, ErrInvalidDeviceRecord) {
		t.Fatalf("trailing record = %v, want ErrInvalidDeviceRecord", err)
	}
}

func TestDeviceBranchDiscoveryAndCleanupStayWithinTarget(t *testing.T) {
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

	record, err := NewDeviceRecord("devicea", "workstation", "windows", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := PutDeviceRecord(context.Background(), store, public, record); err != nil {
		t.Fatal(err)
	}
	target, err := NewObjectLayout("projecta", "sessiona", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewObjectLayout("projecta", "sessiona", "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	targetMeta, err := target.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	targetShard, err := target.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	otherShard, err := other.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	keyfilePath := crypto.KeyfilePath()
	for _, key := range []string{targetMeta, targetShard, otherShard, keyfilePath} {
		if err := store.Put(context.Background(), key, strings.NewReader("opaque"), int64(len("opaque"))); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}

	activities, err := DiscoverDeviceBranches(context.Background(), store)
	if err != nil {
		t.Fatalf("DiscoverDeviceBranches: %v", err)
	}
	ids := make([]string, 0, len(activities))
	for _, activity := range activities {
		ids = append(ids, activity.DeviceID)
	}
	if want := []string{"devicea", "deviceb"}; !equalStrings(ids, want) {
		t.Fatalf("device IDs = %v, want %v", ids, want)
	}

	keys, err := DeviceDataKeys(context.Background(), store, "devicea")
	if err != nil {
		t.Fatalf("DeviceDataKeys: %v", err)
	}
	deviceRecordKey, err := DeviceKey("devicea")
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{deviceRecordKey, targetMeta, targetShard}
	sort.Strings(wantKeys)
	if !equalStrings(keys, wantKeys) {
		t.Fatalf("device keys = %v, want %v", keys, wantKeys)
	}

	removed, err := DeleteDeviceData(context.Background(), store, "devicea")
	if err != nil {
		t.Fatalf("DeleteDeviceData: %v", err)
	}
	if removed != len(wantKeys) {
		t.Fatalf("removed = %d, want %d", removed, len(wantKeys))
	}
	if _, err := store.Stat(context.Background(), otherShard); err != nil {
		t.Fatalf("other device shard was removed: %v", err)
	}
	if _, err := store.Stat(context.Background(), keyfilePath); err != nil {
		t.Fatalf("shared keyfile was removed: %v", err)
	}
	if _, err := store.Stat(context.Background(), targetShard); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("target shard stat = %v, want remote.ErrNotFound", err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
