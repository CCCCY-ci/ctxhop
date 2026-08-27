package main

import (
	"context"
	"crypto/ecdh"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestPublishNativeReplicaCreatesSourceNativeV2View(t *testing.T) {
	projectRoot := t.TempDir()
	configDir := t.TempDir()
	home := t.TempDir()
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
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	ref := adapter.SessionRef{
		Agent:     "codex",
		NativeID:  "native-one",
		Title:     "cross agent context",
		CreatedAt: createdAt,
	}
	space := adapter.PathSpace{ProjectRoot: projectRoot, AgentHome: home}
	data := adapter.SessionData{Records: [][]byte{[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(projectRoot) + `","message":{"role":"user","content":"hello"}}`)}}
	layout := adapter.Layout{Home: home}
	installation := adapter.Installation{DataDir: home, Compatibility: adapter.CompatFull, Version: "0.42.0"}
	if err := publishReplicaProjectScope(context.Background(), "deviceone", identifierKey, "manual:app", project.KindManual, store, public); err != nil {
		t.Fatalf("publishReplicaProjectScope: %v", err)
	}
	if err := publishNativeReplica(context.Background(), configDir, "deviceone", identifierKey, "manual:app", layout, installation, store, public, configDir, ref, "legacyone", data, space, nil); err != nil {
		t.Fatalf("publishNativeReplica: %v", err)
	}
	// A second invocation resumes the local cursor and republishes only the
	// mutable tip; no private content identity is required for this legacy
	// domain retry.
	if err := publishNativeReplica(context.Background(), configDir, "deviceone", identifierKey, "manual:app", layout, installation, store, public, configDir, ref, "legacyone", data, space, nil); err != nil {
		t.Fatalf("idempotent publishNativeReplica: %v", err)
	}

	hubKey, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	projectKey, err := sessionhub.DeriveProjectKey(identifierKey, hubKey, "manual:app")
	if err != nil {
		t.Fatal(err)
	}
	sessionKey, err := sessionhub.DeriveNativeLogicalSessionKey(identifierKey, projectKey, "codex", ref.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	projectLayout, err := syncer.NewProjectHubLayout(hubKey, projectKey)
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.FetchProjectReplicaMetadata(context.Background(), store, projectLayout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[0].SessionID != sessionKey || len(metadata[0].Replicas) != 1 {
		t.Fatalf("project Replica metadata = %+v, want one logical Session", metadata)
	}
	if metadata[0].SessionDescriptor == nil || metadata[0].SessionDescriptor.Title != ref.Title {
		t.Fatalf("logical Session descriptor = %+v", metadata[0].SessionDescriptor)
	}
	replica := metadata[0].Replicas[0]
	if replica.Descriptor.Source.Agent != "codex" || replica.Descriptor.Source.AgentVersion != installation.Version || replica.Tip == nil || replica.Tip.RecordCount != 1 {
		t.Fatalf("Replica metadata = %+v", replica)
	}

	snapshot, err := syncer.FetchCompleteReplica(context.Background(), store, replica.Layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica: %v", err)
	}
	if len(snapshot.Records) != 1 || !strings.Contains(string(snapshot.Records[0]), `"content":"hello"`) {
		t.Fatalf("Replica records = %s", snapshot.Records)
	}
	objects, err := store.List(context.Background(), "v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("direct v2 publication unexpectedly wrote v1 objects: %+v", objects)
	}
}
