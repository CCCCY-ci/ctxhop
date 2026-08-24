package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
)

func TestFindEnvironmentSessionMatchesRemoteThenNativeID(t *testing.T) {
	sessions := []listSession{
		{RemoteID: "remote-one", NativeID: "native-one"},
		{RemoteID: "remote-two", NativeID: "native-two"},
	}
	if got := findEnvironmentSession(sessions, "remote-two"); got == nil || got.RemoteID != "remote-two" {
		t.Fatalf("remote match = %#v", got)
	}
	if got := findEnvironmentSession(sessions, "native-one"); got == nil || got.RemoteID != "remote-one" {
		t.Fatalf("native match = %#v", got)
	}
	if got := findEnvironmentSession(sessions, "missing"); got != nil {
		t.Fatalf("missing match = %#v", got)
	}
}

func TestWriteEnvironmentPreviewTextIsMetadataOnly(t *testing.T) {
	var output bytes.Buffer
	err := writeEnvironmentPreviewText(&output, environmentPreviewReport{
		Scope:        "project",
		Session:      "remote-one",
		Agent:        "codex",
		NativeID:     "native-one",
		Dependencies: []environment.Reference{{Kind: "tool-requirement", Name: "codex", Version: "0.148.0", Portability: "platform-specific"}},
		Changes: []environmentComponentChange{{
			Component: environment.Component{Kind: "skill", Name: "coding-guidelines", Scope: "global"},
			Path:      "C:/Users/test/.codex/skills/coding-guidelines/SKILL.md",
			State:     environment.ComponentStateChanged,
		}},
		Status: "observed-only",
		Notes:  []string{"no files changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, marker := range []string{"status: observed-only", "dependencies: 1", "changes: 1", "state=changed", "no files changed"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("output %q does not contain %q", text, marker)
		}
	}
}
