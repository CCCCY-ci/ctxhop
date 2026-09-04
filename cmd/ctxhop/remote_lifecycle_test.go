package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestParseRemoteLifecycleOptions(t *testing.T) {
	session, err := parseRemoteOptions([]string{"delete-session", "--remote-id", "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if session.action != remoteActionDeleteSession || session.target != "abc123" || !session.remoteID || session.path != "." {
		t.Fatalf("session options = %+v", session)
	}
	project, err := parseRemoteOptions([]string{"delete-project", "--path", "C:\\work"})
	if err != nil {
		t.Fatal(err)
	}
	if project.action != remoteActionDeleteProject || project.path != "C:\\work" {
		t.Fatalf("project options = %+v", project)
	}
	all, err := parseRemoteOptions([]string{"delete-all"})
	if err != nil {
		t.Fatal(err)
	}
	if all.action != remoteActionDeleteAll {
		t.Fatalf("all options = %+v", all)
	}
}

func TestRunRemoteDeleteAllUsesValidatedRemoteAndReportsCount(t *testing.T) {
	configDir, remoteRoot, _, _ := preparePassphraseCommand(t, "alpha-secret-6f2d")
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)

	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"v1/devices/devicea",
		"v1/projects/projecta/sessions/sessiona/devicea/000001",
	} {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	var output bytes.Buffer
	if err := runRemoteWithIO([]string{"delete-all"}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("runRemoteWithIO(delete-all): %v", err)
	}
	if got := output.String(); !strings.Contains(got, "remote deleted: scope=delete-all objects=3") {
		t.Fatalf("delete-all output = %q", got)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("objects after delete-all = %+v", objects)
	}
}

func TestRunRemoteDeleteProjectKeepsOtherProjectAndGlobalObjects(t *testing.T) {
	configDir, remoteRoot, projectDir, projectID, identifierKey := prepareRemoteLifecycleProject(t)
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)

	otherProjectID, err := crypto.ProjectID(identifierKey, "manual:other-project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	targetKey := "v1/projects/" + projectID + "/sessions/sessiona/devicea/000001"
	otherKey := "v1/projects/" + otherProjectID + "/sessions/sessiona/devicea/000001"
	for _, key := range []string{targetKey, otherKey, "v1/devices/devicea"} {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	var output bytes.Buffer
	if err := runRemoteWithIO([]string{"delete-project", "--path", projectDir}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("runRemoteWithIO(delete-project): %v", err)
	}
	if got := output.String(); !strings.Contains(got, "remote deleted: scope=delete-project objects=1") {
		t.Fatalf("delete-project output = %q", got)
	}
	if _, err := store.Stat(context.Background(), targetKey); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("target project object = %v, want remote.ErrNotFound", err)
	}
	for _, key := range []string{otherKey, "v1/devices/devicea", crypto.KeyfilePath()} {
		if _, err := store.Stat(context.Background(), key); err != nil {
			t.Errorf("unrelated object %s was removed: %v", key, err)
		}
	}
}

func TestRunRemoteDeleteSessionKeepsOtherSessionObjects(t *testing.T) {
	configDir, remoteRoot, projectDir, projectID, identifierKey := prepareRemoteLifecycleProject(t)
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)

	targetSession, err := crypto.SessionID(identifierKey, projectID, "native-session")
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := crypto.SessionID(identifierKey, projectID, "other-session")
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	targetKey := "v1/projects/" + projectID + "/sessions/" + targetSession + "/devicea/000001"
	otherKey := "v1/projects/" + projectID + "/sessions/" + otherSession + "/devicea/000001"
	for _, key := range []string{targetKey, otherKey} {
		if err := store.Put(context.Background(), key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	var output bytes.Buffer
	if err := runRemoteWithIO([]string{"delete-session", "--path", projectDir, "native-session"}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("runRemoteWithIO(delete-session): %v", err)
	}
	if got := output.String(); !strings.Contains(got, "remote deleted: scope=delete-session objects=1") {
		t.Fatalf("delete-session output = %q", got)
	}
	if _, err := store.Stat(context.Background(), targetKey); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("target session object = %v, want remote.ErrNotFound", err)
	}
	for _, key := range []string{otherKey, crypto.KeyfilePath()} {
		if _, err := store.Stat(context.Background(), key); err != nil {
			t.Errorf("unrelated object %s was removed: %v", key, err)
		}
	}
}

func prepareRemoteLifecycleProject(t *testing.T) (string, string, string, string, []byte) {
	t.Helper()
	configDir, remoteRoot, _, _ := preparePassphraseCommand(t, "alpha-secret-6f2d")
	projectDir := t.TempDir()
	identity := "manual:remote-lifecycle"
	identifierKey := []byte("0123456789abcdef0123456789abcdef")

	c, err := config.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	c.Projects.Bindings = []config.Binding{{Identity: identity, LocalRoot: filepath.Clean(projectDir)}}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSecrets(configDir, &config.Secrets{IdentifierKey: identifierKey}); err != nil {
		t.Fatal(err)
	}
	projectID, err := crypto.ProjectID(identifierKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return configDir, remoteRoot, projectDir, projectID, identifierKey
}
