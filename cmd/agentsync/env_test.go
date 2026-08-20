package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/environment"
)

func TestParseEnvironmentOptions(t *testing.T) {
	options, err := parseEnvironmentOptions([]string{"preview", "--json", "native-session"})
	if err != nil {
		t.Fatal(err)
	}
	if options.action != "preview" || options.session != "native-session" || !options.json {
		t.Fatalf("options = %#v", options)
	}
	if _, err := parseEnvironmentOptions([]string{"apply", "native-session"}); err != nil {
		t.Fatalf("apply should parse before the handler reports it as unavailable: %v", err)
	}
	if _, err := parseEnvironmentOptions([]string{"preview"}); err == nil {
		t.Fatal("missing session ID was accepted")
	}
}

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
		Status:       "observed-only",
		Notes:        []string{"no files changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, marker := range []string{"status: observed-only", "dependencies: 1", "codex", "no files changed"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("output %q does not contain %q", text, marker)
		}
	}
}
