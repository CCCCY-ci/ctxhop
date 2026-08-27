package sessionhub

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDefaultRegistryPersistsAStableHub(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	first, err := NewDefaultRegistry(key, time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDefaultRegistry(key, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	firstHub, ok := first.DefaultHub()
	if !ok {
		t.Fatal("first registry has no default hub")
	}
	secondHub, ok := second.DefaultHub()
	if !ok {
		t.Fatal("second registry has no default hub")
	}
	if firstHub.Descriptor.HubID != secondHub.Descriptor.HubID {
		t.Fatalf("default hub changed from %q to %q", firstHub.Descriptor.HubID, secondHub.Descriptor.HubID)
	}

	dir := t.TempDir()
	if err := SaveRegistry(dir, first); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Hubs) != 1 {
		t.Fatalf("loaded registry = %+v", loaded)
	}
	loadedHub, ok := loaded.DefaultHub()
	if !ok || loadedHub.Descriptor.CreatedAt != firstHub.Descriptor.CreatedAt {
		t.Fatalf("loaded hub = %+v, want %+v", loadedHub, firstHub)
	}
}

func TestRegistryCreatesProjectLegacySessionAndBinding(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	registry, err := NewDefaultRegistry(key, time.Unix(100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.EnsureProject(key, ProjectIdentityRemote, "github.com/example/app", time.Unix(200, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	creator := SessionCreator{Agent: "claude-code", DeviceID: "device01"}
	session, err := registry.EnsureLegacySession(key, project.Descriptor.ProjectID, "legacyone", "first title", time.Unix(300, 0).UTC(), creator)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BindNativeSession(project.Descriptor.ProjectID, session.Descriptor.SessionID, NativeSessionBinding{
		Agent:           "claude-code",
		NativeSessionID: "native-one",
		LegacySessionID: "legacyone",
		BoundAt:         time.Unix(302, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	found, ok := registry.FindSessionByNative(project.Descriptor.ProjectID, "claude-code", "native-one", "legacyone")
	if !ok || found.Descriptor.SessionID != session.Descriptor.SessionID {
		t.Fatalf("found = %+v, ok=%t", found, ok)
	}
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}

	other, err := registry.EnsureLegacySession(key, project.Descriptor.ProjectID, "legacytwo", "second title", time.Unix(400, 0).UTC(), creator)
	if err != nil {
		t.Fatal(err)
	}
	err = registry.BindNativeSession(project.Descriptor.ProjectID, other.Descriptor.SessionID, NativeSessionBinding{
		Agent:           "claude-code",
		NativeSessionID: "native-one",
		BoundAt:         time.Unix(401, 0).UTC(),
	})
	if !errors.Is(err, ErrNativeSessionAlreadyBound) {
		t.Fatalf("duplicate binding error = %v, want ErrNativeSessionAlreadyBound", err)
	}
}

func TestRegistryRejectsMissingFileWithoutCreatingIt(t *testing.T) {
	_, err := LoadRegistry(t.TempDir())
	if !errors.Is(err, ErrRegistryNotFound) {
		t.Fatalf("LoadRegistry error = %v, want ErrRegistryNotFound", err)
	}
}
