package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestDeleteRemoteSessionKeepsAdjacentNamespaces(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	objects := []string{
		"v1/projects/p/sessions/s/devicea/000001",
		"v1/projects/p/sessions/s/devicea/meta",
		"v1/projects/p/sessions/s2/devicea/000001",
		"v1/projects/p2/sessions/s/devicea/000001",
		"v1/keyfile",
	}
	for _, key := range objects {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	removed, err := DeleteRemoteSession(context.Background(), store, "p", "s")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, key := range objects[:2] {
		if _, err := store.Stat(context.Background(), key); !errors.Is(err, remote.ErrNotFound) {
			t.Errorf("deleted key %s still exists: %v", key, err)
		}
	}
	for _, key := range objects[2:] {
		if _, err := store.Stat(context.Background(), key); err != nil {
			t.Errorf("adjacent key %s was deleted: %v", key, err)
		}
	}
}

func TestDeleteRemoteProjectKeepsOtherProjectsAndGlobalObjects(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"v1/projects/p/sessions/s/devicea/000001",
		"v1/projects/p/project",
		"v1/projects/p2/sessions/s/devicea/000001",
		"v1/devices/devicea",
		"v1/keyfile",
	}
	for _, key := range keys {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	removed, err := DeleteRemoteProject(context.Background(), store, "p")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, err := store.Stat(context.Background(), keys[2]); err != nil {
		t.Errorf("other project was deleted: %v", err)
	}
	if _, err := store.Stat(context.Background(), keys[3]); err != nil {
		t.Errorf("device record was deleted: %v", err)
	}
	if _, err := store.Stat(context.Background(), keys[4]); err != nil {
		t.Errorf("keyfile was deleted: %v", err)
	}
}

func TestDeleteRemoteAllIncludesKeyfile(t *testing.T) {
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"v1/keyfile", "v1/devices/devicea", "v1/projects/p/sessions/s/devicea/000001"} {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	removed, err := DeleteRemoteAll(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("objects after delete-all = %+v", objects)
	}
}

func TestRemoteDeletionPrefixesRejectUnsafeIdentifiers(t *testing.T) {
	if _, err := ProjectRemotePrefix(""); err == nil {
		t.Fatal("empty project prefix unexpectedly accepted")
	}
	if _, err := SessionRemotePrefix("project", "session/id"); err == nil {
		t.Fatal("session prefix with separator unexpectedly accepted")
	}
}
