package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestParseMaterializeOptionsAcceptsDocumentedPositionalForm(t *testing.T) {
	options, err := parseMaterializeOptions([]string{
		"logical-session",
		"--to", "codex",
		"--context", "causal-head",
		"--head", "contribution-head",
		"--preview",
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.sessionID != "logical-session" || options.targetAgent != "codex" || options.contextPolicy != materializeContextCausal || !options.preview || !options.json {
		t.Fatalf("options = %+v", options)
	}
	if len(options.heads) != 1 || options.heads[0] != "contribution-head" {
		t.Fatalf("heads = %v", options.heads)
	}
}

func TestParseMaterializeOptionsDefaultsToDirectExecution(t *testing.T) {
	options, err := parseMaterializeOptions([]string{
		"logical-session",
		"--to", "codex",
		"--context", "causal-head",
		"--head", "contribution-head",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.apply || options.preview {
		t.Fatalf("options = %+v", options)
	}
}

func TestParseMaterializeOptionsValidatesContextSelectors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing target", args: []string{"session", "--preview"}, want: "--to is required"},
		{name: "agent source missing", args: []string{"session", "--to", "codex", "--context", "agent-only", "--preview"}, want: "--source is required"},
		{name: "head on all heads", args: []string{"session", "--to", "codex", "--context", "all-heads", "--head", "head", "--preview"}, want: "--head is only valid"},
		{name: "source on causal head", args: []string{"session", "--to", "codex", "--source", "claude-code", "--preview"}, want: "--source is only valid"},
		{name: "unknown policy", args: []string{"session", "--to", "codex", "--context", "future", "--preview"}, want: "unsupported --context"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseMaterializeOptions(test.args); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestMaterializeApplyRequestIdentityAndFallbackTimeAreStable(t *testing.T) {
	first := materializeOptions{
		sessionID:     "session",
		targetAgent:   "codex",
		contextPolicy: materializeContextCausal,
		heads:         []string{"headb", "heada"},
		apply:         true,
	}
	second := first
	second.heads = []string{"heada", "headb"}
	firstID := materializeRequestID("hub", "project", first)
	secondID := materializeRequestID("hub", "project", second)
	if firstID != secondID || materializeNativeSessionID(firstID) != materializeNativeSessionID(secondID) || materializeStableTargetTime(firstID) != materializeStableTargetTime(secondID) {
		t.Fatalf("apply identity is not stable: %q/%q", firstID, secondID)
	}
}

func TestWriteMaterializePreviewOutputOmitsEncodedRecords(t *testing.T) {
	report := materializePreviewReport{
		Preview:       true,
		Scope:         "project",
		HubID:         "hub",
		ProjectID:     "project",
		SessionID:     "session",
		ContextPolicy: materializeContextCausal,
		SelectedHeads: []string{"head"},
		Coverage: sessionhub.Coverage{
			SelectedIDs: []string{"root", "head"},
			OmittedIDs:  []string{"other"},
		},
		Sources: []syncflow.MaterializeSourceSummary{{
			ContributionID: "head",
			SourceAgent:    "claude-code",
			ReplicaID:      "replica",
			StartRecord:    0,
			EndRecord:      1,
			RecordCount:    1,
			ContextItems:   1,
		}},
		TargetAgent:          "codex",
		TargetNativeID:       "new-target",
		TargetAdapterVersion: "codex-materialize-v1",
		SelectedRecordCount:  1,
		ContextItems:         1,
		Stats:                adapter.MaterializeStats{Converted: 1},
		WriteStatus:          "new-target-session-not-written",
	}
	var textOutput bytes.Buffer
	if err := writeMaterializePreviewText(&textOutput, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(textOutput.String(), "source sessions, Agent files, LocalBinding and Remote objects: unchanged") || strings.Contains(textOutput.String(), "encoded") {
		t.Fatalf("text output = %q", textOutput.String())
	}
	var jsonOutput bytes.Buffer
	if err := writeMaterializePreviewJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["encodedRecords"]; exists {
		t.Fatalf("JSON output exposed encoded records: %s", jsonOutput.String())
	}
}

func TestWriteMaterializeApplyTextReportsCommittedTargetWithoutPreviewClaim(t *testing.T) {
	report := materializePreviewReport{
		Preview:        false,
		Scope:          "project",
		HubID:          "hub",
		ProjectID:      "project",
		SessionID:      "session",
		ContextPolicy:  materializeContextCausal,
		TargetAgent:    "codex",
		TargetNativeID: "ctxhop-target",
		WriteStatus:    "created-and-committed",
	}
	var output bytes.Buffer
	if err := writeMaterializePreviewText(&output, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "unchanged\n") || !strings.Contains(output.String(), "target Agent session and local binding: committed") {
		t.Fatalf("apply text output = %q", output.String())
	}
}

func TestMaterializeTargetReportsMissingAgentWithoutFallback(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()+"-missing-claude")
	t.Setenv("CODEX_HOME", t.TempDir()+"-missing-codex")
	_, _, err := materializeTarget(t.Context(), "codex")
	if !errors.Is(err, adapter.ErrNotInstalled) && !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("materializeTarget error = %v, want not installed", err)
	}
}
