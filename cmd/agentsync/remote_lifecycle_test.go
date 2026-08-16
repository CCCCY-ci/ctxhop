package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestParseRemoteLifecycleOptions(t *testing.T) {
	session, err := parseRemoteOptions([]string{"delete-session", "--yes", "--remote-id", "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if session.action != remoteActionDeleteSession || session.target != "abc123" || !session.remoteID || !session.yes || session.path != "." {
		t.Fatalf("session options = %+v", session)
	}
	project, err := parseRemoteOptions([]string{"delete-project", "--path", "C:\\work"})
	if err != nil {
		t.Fatal(err)
	}
	if project.action != remoteActionDeleteProject || project.path != "C:\\work" {
		t.Fatalf("project options = %+v", project)
	}
	all, err := parseRemoteOptions([]string{"delete-all", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if all.action != remoteActionDeleteAll || !all.yes {
		t.Fatalf("all options = %+v", all)
	}
}

func TestConfirmRemoteDeletion(t *testing.T) {
	var prompt bytes.Buffer
	confirmed, err := confirmRemoteDeletion(strings.NewReader("yes\n"), &prompt, remoteActionDeleteAll, "")
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed || !strings.Contains(prompt.String(), "keyfile") {
		t.Fatalf("confirmation = %v, prompt = %q", confirmed, prompt.String())
	}

	confirmed, err = confirmRemoteDeletion(strings.NewReader("no\n"), &prompt, remoteActionDeleteProject, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Fatal("negative confirmation was accepted")
	}
}

func TestRunRemoteDeleteAllUsesValidatedRemoteAndReportsCount(t *testing.T) {
	configDir, remoteRoot, _, _ := preparePassphraseCommand(t, "alpha-secret-6f2d")
	t.Setenv("AGENTSYNC_CONFIG_DIR", configDir)

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
	if err := runRemoteWithIO([]string{"delete-all", "--yes"}, strings.NewReader(""), &output); err != nil {
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
