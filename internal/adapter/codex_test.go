package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexLayoutDiscoversReadsAndWritesSessions(t *testing.T) {
	home := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "source project")
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(home, "sessions", "2026", "08", "20", "rollout-2026-08-20T10-20-30-"+id+".jsonl")
	arguments, err := json.Marshal(map[string]string{"file_path": filepath.Join(projectRoot, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{
		codexTestRecord(t, "2026-08-20T10:20:30Z", "session_meta", map[string]any{
			"session_id":  id,
			"cwd":         projectRoot,
			"cli_version": "0.148.0",
			"timestamp":   "2026-08-20T10:20:30Z",
		}),
		codexTestRecord(t, "2026-08-20T10:20:31Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "continue the Codex adapter",
		}),
		codexTestRecord(t, "2026-08-20T10:20:32Z", "turn_context", map[string]any{
			"cwd":             projectRoot,
			"workspace_roots": []string{projectRoot},
		}),
		codexTestRecord(t, "2026-08-20T10:20:33Z", "response_item", map[string]any{
			"type":      "function_call",
			"name":      "apply_patch",
			"arguments": string(arguments),
		}),
	}
	if err := writeSessionAt(path, records); err != nil {
		t.Fatal(err)
	}

	layout := CodexLayout{Home: home}
	refs, err := layout.DiscoverSessions(projectRoot)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want one session", refs)
	}
	ref := refs[0]
	if ref.Agent != "codex" || ref.NativeID != id || ref.ProjectPath != projectRoot {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.Title != "continue the Codex adapter" || ref.Size == 0 {
		t.Fatalf("ref metadata = %+v", ref)
	}

	data, err := layout.ReadSession(ref)
	if err != nil || len(data.Records) != len(records) {
		t.Fatalf("ReadSession records=%d error=%v", len(data.Records), err)
	}
	accesses := layout.TouchedFiles(data.Records, projectRoot)
	if len(accesses) != 1 || accesses[0].Path != "main.go" || !accesses[0].Written {
		t.Fatalf("touched files = %+v", accesses)
	}

	newID := "22222222-2222-4222-8222-222222222222"
	newRecords := [][]byte{
		codexTestRecord(t, "2026-08-20T11:20:30Z", "session_meta", map[string]any{
			"session_id": newID,
			"cwd":        projectRoot,
			"timestamp":  "2026-08-20T11:20:30Z",
		}),
		codexTestRecord(t, "2026-08-20T11:20:31Z", "event_msg", map[string]any{
			"type":    "user_message",
			"message": "restored Codex session",
		}),
	}
	if err := layout.WriteSession(projectRoot, newID, newRecords); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	if err := layout.WriteSession(projectRoot, newID, newRecords); !strings.Contains(err.Error(), "session already exists") {
		t.Fatalf("second WriteSession error = %v", err)
	}
	if err := layout.ReplaceSession(projectRoot, newID, newRecords); err != nil {
		t.Fatalf("ReplaceSession: %v", err)
	}
	written, err := layout.ReadSession(SessionRef{NativeID: newID})
	if err != nil || len(written.Records) != len(newRecords) {
		t.Fatalf("written records=%d error=%v", len(written.Records), err)
	}
}

func TestCodexLayoutSkipsAnotherProject(t *testing.T) {
	home := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "project")
	otherRoot := filepath.Join(t.TempDir(), "other")
	id := "33333333-3333-4333-8333-333333333333"
	path := filepath.Join(home, "sessions", "2026", "08", "20", "rollout-2026-08-20T10-20-30-"+id+".jsonl")
	if err := writeSessionAt(path, [][]byte{codexTestRecord(t, "2026-08-20T10:20:30Z", "session_meta", map[string]any{
		"session_id": id,
		"cwd":        otherRoot,
	})}); err != nil {
		t.Fatal(err)
	}
	refs, err := (CodexLayout{Home: home}).DiscoverSessions(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %+v, want no sessions from another project", refs)
	}
}

func TestCodexCanonicalizesWorkspaceRootsAndEmbeddedToolArguments(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "source project")
	agentHome := filepath.Join(t.TempDir(), "codex home")
	arguments, err := json.Marshal(map[string]string{"file_path": filepath.Join(projectRoot, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	record := codexTestRecord(t, "2026-08-20T10:20:30Z", "response_item", map[string]any{
		"workspace_roots": []string{projectRoot},
		"arguments":       string(arguments),
	})
	canonicalizer := NewCanonicalizer(PathSpace{ProjectRoot: projectRoot, AgentHome: agentHome})
	canonical, err := canonicalizer.Record(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), TokenProject) || strings.Contains(string(canonical), projectRoot) {
		t.Fatalf("canonical record = %s", canonical)
	}
	localized, err := Localize(canonical, PathSpace{ProjectRoot: filepath.Join(t.TempDir(), "target project"), AgentHome: filepath.Join(t.TempDir(), "target codex")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(localized), TokenProject) {
		t.Fatalf("localized record still contains token: %s", localized)
	}
}

func TestCodexDetectReadsRecordedVersionWithoutVersionGating(t *testing.T) {
	home := t.TempDir()
	id := "44444444-4444-4444-8444-444444444444"
	path := filepath.Join(home, "sessions", "2026", "08", "20", "rollout-2026-08-20T10-20-30-"+id+".jsonl")
	if err := writeSessionAt(path, [][]byte{codexTestRecord(t, "2026-08-20T10:20:30Z", "session_meta", map[string]any{
		"session_id":  id,
		"cwd":         t.TempDir(),
		"cli_version": "9.99.0",
	})}); err != nil {
		t.Fatal(err)
	}
	installation, err := (CodexLayout{Home: home}).Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if installation.Version != "9.99.0" || installation.Compatibility != CompatFull {
		t.Fatalf("installation = %+v", installation)
	}
}

func codexTestRecord(t *testing.T, timestamp, recordType string, payload map[string]any) []byte {
	t.Helper()
	record := map[string]any{"timestamp": timestamp, "type": recordType, "payload": payload}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCodexDefaultHomeResolvesOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", override)
	got, err := DefaultCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	if got != override {
		t.Fatalf("DefaultCodexHome = %q, want %q", got, override)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("override unexpectedly exists: %v", err)
	}
}
