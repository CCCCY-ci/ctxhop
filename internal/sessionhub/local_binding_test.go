package sessionhub

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestLocalBindingRoundTripUsesOpaquePathAndAtomicFile(t *testing.T) {
	root := t.TempDir()
	binding := testMaterializedBinding()
	if err := SaveLocalBinding(root, binding); err != nil {
		t.Fatal(err)
	}
	path, err := LocalBindingPath(root, binding)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, binding.NativeSessionID) {
		t.Fatalf("local binding path leaked native session ID: %q", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("binding file was not created: %v", err)
	}
	loaded, err := LoadLocalBinding(root, binding.HubID, binding.ProjectID, binding.SessionID, binding.ReplicaID, binding.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NativeSessionID != binding.NativeSessionID || loaded.ReplicaCursor != binding.ReplicaCursor || loaded.Origin.Kind != binding.Origin.Kind {
		t.Fatalf("loaded binding = %+v, want %+v", loaded, binding)
	}
}

func TestLoadLocalBindingValidatesLookupIdentityAndMissingState(t *testing.T) {
	root := t.TempDir()
	binding := testMaterializedBinding()
	if _, err := LoadLocalBinding(root, binding.HubID, binding.ProjectID, binding.SessionID, binding.ReplicaID, binding.Agent); !errors.Is(err, ErrLocalBindingNotFound) {
		t.Fatalf("missing binding error = %v, want ErrLocalBindingNotFound", err)
	}
	if _, err := LoadLocalBinding(root, "../escape", binding.ProjectID, binding.SessionID, binding.ReplicaID, binding.Agent); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unsafe lookup error = %v, want ErrInvalidIdentity", err)
	}
}
