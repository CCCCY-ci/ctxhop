package syncer

import (
	"context"
	"reflect"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/environment"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestEnvironmentMetadataRoundTrip(t *testing.T) {
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
	references := []environment.Reference{
		{Kind: "mcp", Name: "browser", Portability: "platform-specific"},
		{Kind: "tool-requirement", Name: "codex", Version: "0.148.0", Portability: "platform-specific"},
	}
	metadata, err := NewEnvironmentMetadata(references)
	if err != nil {
		t.Fatal(err)
	}
	key := "v1/projects/projectone/sessions/sessionone/deviceone/env"
	sealed, err := SealEnvironment(public, key, metadata)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenEnvironment(identity, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opened.References, references) {
		t.Fatalf("references = %#v, want %#v", opened.References, references)
	}
}

func TestFetchMetadataReadsOptionalEnvironmentObject(t *testing.T) {
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
	layout, err := NewObjectLayout("projectone", "sessionone", "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(1, [32]byte{1}, []byte("{\"ok\":true}"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := PutMetadata(ctx, store, public, layout, metadata); err != nil {
		t.Fatal(err)
	}
	references := []environment.Reference{{Kind: "skill", Name: "coding-guidelines", Portability: "portable"}}
	if err := PutEnvironmentReferences(ctx, store, public, layout, references); err != nil {
		t.Fatal(err)
	}
	found, err := FetchMetadata(ctx, store, "projectone", "sessionone", identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !reflect.DeepEqual(found[0].Environment, references) {
		t.Fatalf("metadata = %#v, want environment %#v", found, references)
	}
}

func TestObjectLayoutEnvironmentKeyIsSeparateFromSessionMetadata(t *testing.T) {
	layout, err := NewObjectLayout("projectone", "sessionone", "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := layout.MetadataKey()
	if err != nil {
		t.Fatal(err)
	}
	environmentKey, err := layout.EnvironmentKey()
	if err != nil {
		t.Fatal(err)
	}
	if metadataKey == environmentKey || environmentKey[len(environmentKey)-4:] != "/env" {
		t.Fatalf("metadata key = %q, environment key = %q", metadataKey, environmentKey)
	}
}
