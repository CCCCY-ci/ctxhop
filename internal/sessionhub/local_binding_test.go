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

func TestFindLocalBindingByNativeSession(t *testing.T) {
	root := t.TempDir()
	binding := testMaterializedBinding()
	binding.Origin = BindingOrigin{
		Kind:      ReplicaOriginSameAgentRestore,
		BaseHeads: []string{"head"},
	}
	if err := SaveLocalBinding(root, binding); err != nil {
		t.Fatal(err)
	}
	found, err := FindLocalBindingByNativeSession(root, binding.HubID, binding.ProjectID, binding.SessionID, binding.Agent, binding.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if found.ReplicaID != binding.ReplicaID || found.NativeSessionID != binding.NativeSessionID {
		t.Fatalf("found binding = %+v, want %+v", found, binding)
	}
	if _, err := FindLocalBindingByNativeSession(root, binding.HubID, binding.ProjectID, binding.SessionID, binding.Agent, "other-native"); !errors.Is(err, ErrLocalBindingNotFound) {
		t.Fatalf("missing native binding error = %v, want ErrLocalBindingNotFound", err)
	}
}

func TestFindLocalBindingByNativeSessionRejectsAmbiguousState(t *testing.T) {
	root := t.TempDir()
	first := testMaterializedBinding()
	first.Origin = BindingOrigin{Kind: ReplicaOriginSameAgentRestore, BaseHeads: []string{"head"}}
	second := first
	second.ReplicaID = "replica2"
	if err := SaveLocalBinding(root, first); err != nil {
		t.Fatal(err)
	}
	if err := SaveLocalBinding(root, second); err != nil {
		t.Fatal(err)
	}
	_, err := FindLocalBindingByNativeSession(root, first.HubID, first.ProjectID, first.SessionID, first.Agent, first.NativeSessionID)
	if !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("ambiguous binding error = %v, want ErrInvalidModel", err)
	}
}
