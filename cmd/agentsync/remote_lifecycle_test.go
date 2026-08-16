package main

import (
	"bytes"
	"strings"
	"testing"
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
