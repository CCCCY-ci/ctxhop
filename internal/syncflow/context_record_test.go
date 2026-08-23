package syncflow

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/project"
)

func TestWorkspaceContextRecordForProducesBoundedLocalMarker(t *testing.T) {
	report := project.Report{
		Verdict: project.Divergent,
		Files: []project.FileReport{
			{Path: "src/z.go", Note: "changed"},
			{Path: "src/a.go", Note: "added"},
		},
	}
	record, err := workspaceContextRecordFor(report)
	if err != nil {
		t.Fatalf("workspaceContextRecordFor: %v", err)
	}
	if len(record) == 0 || !isWorkspaceContextRecord(record) {
		t.Fatalf("record = %q, want workspace context marker", record)
	}
	var decoded workspaceContextRecord
	if err := json.Unmarshal(record, &decoded); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if decoded.Type != "user" || !decoded.IsMeta || decoded.CtxHop.Kind != workspaceContextKind {
		t.Fatalf("decoded marker = %+v", decoded)
	}
	if !bytes.Contains([]byte(decoded.Message.Content), []byte("src/a.go: added")) || !bytes.Contains([]byte(decoded.Message.Content), []byte("src/z.go: changed")) {
		t.Fatalf("content = %q", decoded.Message.Content)
	}
	if record, err := workspaceContextRecordFor(project.Report{Verdict: project.Consistent}); err != nil || record != nil {
		t.Fatalf("consistent record = %q, error = %v", record, err)
	}
}

func TestCanonicalizeSessionFiltersWorkspaceContextMarker(t *testing.T) {
	marker, err := workspaceContextRecordFor(project.Report{
		Verdict: project.Explainable,
		Files:   []project.FileReport{{Path: "README.md", Note: "changed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinary := []byte(`{"ok":true}`)
	stream, err := CanonicalizeSession(
		adapter.SessionData{Records: [][]byte{ordinary, marker}},
		adapter.PathSpace{ProjectRoot: "/project", AgentHome: "/agent"},
		adapter.Installation{Compatibility: adapter.CompatFull},
	)
	if err != nil {
		t.Fatalf("CanonicalizeSession: %v", err)
	}
	if len(stream.Records) != 1 || !bytes.Equal(stream.Records[0], ordinary) {
		t.Fatalf("canonical records = %q", stream.Records)
	}
}

func TestApplyRestoreInjectsWorkspaceContextAfterExplicitDivergenceConsent(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatFull)
	fingerprint := &project.Fingerprint{Head: "head", Branch: "main", Files: map[string]string{}}
	writer := &restoreApplyWriterFake{}
	report := project.Report{Verdict: project.Divergent, Files: []project.FileReport{{Path: "main.go", Note: "modified"}}}
	result, err := applyRestore(
		context.Background(), writer, "/project", "session", plan,
		RestoreApplyOptions{Fingerprint: fingerprint, AllowDivergent: true, InjectWorkspaceContext: true},
		func(context.Context, string, project.Fingerprint) (project.Report, error) { return report, nil },
	)
	if err != nil {
		t.Fatalf("applyRestore: %v", err)
	}
	if !result.ContextInjected || len(writer.records) != len(plan.LocalizedRecords)+1 {
		t.Fatalf("result = %+v, records = %d", result, len(writer.records))
	}
	if !bytes.Equal(writer.records[0], plan.LocalizedRecords[0]) || !isWorkspaceContextRecord(writer.records[len(writer.records)-1]) {
		t.Fatalf("written records = %q", writer.records)
	}
}
func TestCodexWorkspaceContextRecordUsesEventMessageShape(t *testing.T) {
	record, err := codexWorkspaceContextRecordFor(project.Report{
		Verdict: project.Explainable,
		Files:   []project.FileReport{{Path: "main.go", Note: "modified"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded codexWorkspaceContextRecord
	if err := json.Unmarshal(record, &decoded); err != nil {
		t.Fatalf("decode Codex marker: %v", err)
	}
	if decoded.Type != "event_msg" || decoded.Payload.Type != "user_message" || decoded.Payload.CtxHop.Kind != workspaceContextKind {
		t.Fatalf("decoded marker = %+v", decoded)
	}
	if !bytes.Contains([]byte(decoded.Payload.Message), []byte("main.go: modified")) || !isWorkspaceContextRecord(record) {
		t.Fatalf("decoded marker content = %q", decoded.Payload.Message)
	}
}
