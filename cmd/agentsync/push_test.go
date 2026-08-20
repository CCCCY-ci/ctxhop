package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

func TestPushDiscoveredSessionPublishesShardsAndSummary(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	stateRoot := t.TempDir()
	gitInit := exec.Command("git", "init", "-q")
	gitInit.Dir = projectRoot
	if output, err := gitInit.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, output)
	}
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

	layout := adapter.Layout{Home: home}
	ref := adapter.SessionRef{
		NativeID:  "native-one",
		Title:     "continue sync",
		CreatedAt: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC),
	}
	path := layout.SessionFile(projectRoot, ref.NativeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	record := []byte(`{"type":"user","cwd":"` + filepath.ToSlash(projectRoot) + `","timestamp":"2026-08-15T02:00:00Z","message":{"role":"user","content":"hello"}}`)
	if err := os.WriteFile(path, append(record, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	projectID := "projectone"
	deviceID := "deviceone"
	queue, err := syncer.NewQueueStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := syncflow.NewQueuedPusher(queue, syncer.DefaultRetryPolicy(), classifyPushFailure)
	if err != nil {
		t.Fatal(err)
	}
	installation := adapter.Installation{DataDir: home, Compatibility: adapter.CompatFull}
	space := adapter.PathSpace{ProjectRoot: projectRoot, AgentHome: home}
	summary := pushDiscoveredSessions(context.Background(), deviceID, identifierKey, projectID, layout, installation, space, store, public, pusher, stateRoot, projectRoot, []adapter.SessionRef{ref})
	if summary.Pushed != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v", summary)
	}

	sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	objectLayout, err := syncer.NewObjectLayout(projectID, sessionID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "v1/projects/"+projectID+"/sessions/"+sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 3 {
		t.Fatalf("remote objects = %+v, want one shard, one environment object and one metadata object", objects)
	}
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.FetchMetadata(context.Background(), store, projectID, sessionID, identity)
	if err != nil || len(metadata) != 1 {
		t.Fatalf("metadata = %+v, error = %v", metadata, err)
	}
	decoded, err := syncflow.DecodeSessionSummary(metadata[0].Metadata.Payload)
	if err != nil || decoded.NativeID != ref.NativeID || decoded.Title != ref.Title || decoded.Fingerprint == nil {
		t.Fatalf("summary = %+v, error = %v", decoded, err)
	}
	if strings.Contains(string(metadata[0].Metadata.Payload), "\"dependencies\"") {
		t.Fatal("environment references must stay outside the legacy session summary")
	}
	cursorStore, err := syncer.NewCursorStore(stateRoot, objectLayout)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := cursorStore.Load(context.Background())
	if err != nil || cursor.RecordCount != 1 {
		t.Fatalf("cursor = %+v, error = %v", cursor, err)
	}
}
