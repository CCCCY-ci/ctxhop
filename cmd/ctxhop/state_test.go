package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestProjectStateHasPathOverlap(t *testing.T) {
	workspace := &workspacePreviewReport{Changes: []workspaceChange{{Path: "src/main.go", State: "changed"}}}
	git := &gitPreviewReport{Dirty: []gitEntryReport{{Path: "src/main.go"}}}
	if !projectStateHasPathOverlap(workspace, git) {
		t.Fatal("expected workspace and Git paths to overlap")
	}

	git.Dirty[0].Path = "docs/readme.md"
	if projectStateHasPathOverlap(workspace, git) {
		t.Fatal("unrelated paths were reported as overlapping")
	}
}

func TestStateIsNotPublicCommand(t *testing.T) {
	var output bytes.Buffer
	if err := writeCommandDiscovery(&output, []string{"state"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("state discovery error = %v, output = %q", err, output.String())
	}
}
