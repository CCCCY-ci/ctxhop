package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestProjectAnnouncementTransportAndDiscovery(t *testing.T) {
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

	announcedAt := time.Date(2026, 8, 17, 12, 34, 56, 0, time.UTC)
	record, err := NewProjectAnnouncement("projectone", "deviceone", "remote", "github.com/example/projectone", announcedAt)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ProjectAnnouncementKey(record.ProjectID, record.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealProjectAnnouncement(public, key, record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(record.Identity)) {
		t.Fatal("project identity appears in encrypted announcement")
	}
	opened, err := OpenProjectAnnouncement(identity, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if opened != record {
		t.Fatalf("opened = %+v, want %+v", opened, record)
	}
	if err := PutProjectAnnouncement(context.Background(), store, public, record); err != nil {
		t.Fatal(err)
	}

	announcements, err := FetchProjectAnnouncements(context.Background(), store, []*ecdh.PrivateKey{identity}, map[string]struct{}{"deviceone": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(announcements) != 1 || announcements[0] != record {
		t.Fatalf("announcements = %+v, want one %+v", announcements, record)
	}
}

func TestProjectAnnouncementRejectsConflictingEnvelopeAndKey(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewProjectAnnouncement("projectone", "deviceone", "manual", "manual:projectone", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ProjectAnnouncementKey(record.ProjectID, record.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SealProjectAnnouncement(public, key+"/wrong", record); err == nil {
		t.Fatal("SealProjectAnnouncement accepted an invalid key")
	}

	newer := []byte(`{"version":2,"projectId":"projectone","deviceId":"deviceone","identityKind":"manual","identity":"manual:projectone","announcedAt":"2026-08-17T12:34:56Z"}`)
	if _, err := parseProjectAnnouncement("projectone", "deviceone", newer); !errors.Is(err, ErrUnsupportedProjectAnnouncement) {
		t.Fatalf("newer announcement = %v, want ErrUnsupportedProjectAnnouncement", err)
	}
}

func TestDeviceDataKeysIncludesProjectAnnouncements(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ProjectAnnouncementKey("projectone", "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("announcement")
	if err := store.Put(context.Background(), key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	keys, err := DeviceDataKeys(context.Background(), store, "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, got := range keys {
		if got == key {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("DeviceDataKeys = %v, want %q", keys, key)
	}
}
