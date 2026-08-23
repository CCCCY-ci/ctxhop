package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
)

func TestWriteEnvironmentPreviewTextIncludesLocalRequirementState(t *testing.T) {
	var output bytes.Buffer
	report := environmentPreviewReport{
		Agent:     "codex",
		HookState: "installed",
		Requirements: []environmentRequirementChange{{
			Dependency:   environment.Reference{Kind: "tool-requirement", Name: "codex", Version: "0.148.0"},
			State:        "available",
			LocalVersion: "0.149.0",
			Reason:       "compatibility is determined from session fields, not version number",
		}},
		Status: "observed-only",
	}
	if err := writeEnvironmentPreviewText(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, marker := range []string{"local hook: installed", "tool requirements: 1", "state=available", "local-version=0.149.0", "version number"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("output %q does not contain %q", text, marker)
		}
	}
}
