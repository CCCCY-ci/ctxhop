package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

func TestParseWatchOptions(t *testing.T) {
	defaults, err := parseWatchOptions(nil)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if defaults.interval != watchDefaultInterval || defaults.once || defaults.json {
		t.Fatalf("defaults = %+v", defaults)
	}

	explicit, err := parseWatchOptions([]string{"--interval", "1m", "--once", "--json"})
	if err != nil {
		t.Fatalf("parse explicit options: %v", err)
	}
	if explicit.interval != time.Minute || !explicit.once || !explicit.json {
		t.Fatalf("explicit = %+v", explicit)
	}

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "too short", args: []string{"--interval", "999ms"}, want: "at least"},
		{name: "too long", args: []string{"--interval", "24h1m"}, want: "at most"},
		{name: "unexpected argument", args: []string{"project"}, want: "unexpected argument"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseWatchOptions(test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestWatchSessionSignatureIsDeterministicAndSensitive(t *testing.T) {
	created := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	updated := created.Add(time.Minute)
	first := []adapter.SessionRef{
		{
			NativeID:    "session-b",
			ProjectPath: "C:\\work\\repo",
			CreatedAt:   created,
			UpdatedAt:   updated,
			Size:        20,
		},
		{
			NativeID:    "session-a",
			ProjectPath: "C:\\work\\repo",
			CreatedAt:   created,
			UpdatedAt:   updated,
			Size:        10,
		},
	}
	reordered := []adapter.SessionRef{first[1], first[0]}
	if got, want := watchSessionSignature(first), watchSessionSignature(reordered); got != want {
		t.Fatalf("reordered signature = %q, want %q", got, want)
	}

	changed := append([]adapter.SessionRef(nil), first...)
	changed[0].Size++
	if got, want := watchSessionSignature(changed), watchSessionSignature(first); got == want {
		t.Fatalf("size change did not change signature: %q", got)
	}
}
func TestWriteWatchEventTextAndJSON(t *testing.T) {
	var textOutput bytes.Buffer
	for _, event := range []watchEvent{
		{Scope: "project", Event: "started", Interval: "30s"},
		{Scope: "project", Event: "push", Pushed: 1, Failed: 0, Skipped: 0},
		{Scope: "project", Event: "error", Error: "push cycle failed; run 'ctxhop push' for details"},
	} {
		if err := writeWatchEvent(&textOutput, false, event); err != nil {
			t.Fatal(err)
		}
	}
	wantText := "watching: interval=30s\npush: pushed: 1, failed: 0, skipped: 0\nwatch error: push cycle failed; run 'ctxhop push' for details\n"
	if textOutput.String() != wantText {
		t.Fatalf("text output = %q, want %q", textOutput.String(), wantText)
	}

	var jsonOutput bytes.Buffer
	if err := writeWatchEvent(&jsonOutput, true, watchEvent{
		Scope: "project", Event: "push", Pushed: 1, Failed: 0, Skipped: 0,
	}); err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(jsonOutput.Bytes()), &event); err != nil {
		t.Fatalf("decode JSON output: %v", err)
	}
	if event["scope"] != "project" || event["event"] != "push" {
		t.Fatalf("event = %+v", event)
	}
	for key, want := range map[string]float64{"pushed": 1, "failed": 0, "skipped": 0} {
		if got, ok := event[key].(float64); !ok || got != want {
			t.Fatalf("event[%q] = %#v, want %v", key, event[key], want)
		}
	}
}
func TestSafeWatchErrorDoesNotExposeDetails(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{err: context.DeadlineExceeded, want: "push cycle timed out"},
		{err: errors.New("watch: Claude Code is not installed"), want: "Claude Code is not installed"},
		{err: errors.New("watch: the current directory has no stable project identity"), want: "the current directory has no stable project identity"},
		{err: errors.New("backend password=secret path=C:\\private"), want: "push cycle failed; run 'ctxhop push' for details"},
	}
	for _, test := range cases {
		if got := safeWatchError(test.err); got != test.want {
			t.Errorf("safeWatchError(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
func TestRunWatchOnceReportsSafeMissingAgentError(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	c := config.New()
	root, pathErr := filepath.Abs(".")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	c.Projects.Bindings = []config.Binding{{
		Identity:  "manual:watch-test",
		LocalRoot: root,
	}}
	c.Remote.Type = "dir"
	c.Remote.Path = t.TempDir()
	if err := c.Save(configDir); err != nil {
		t.Fatalf("save config: %v", err)
	}

	var output bytes.Buffer
	err := runWatchWithIO([]string{"--once", "--json"}, &output)
	if err == nil {
		t.Fatal("missing Claude installation unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "no supported coding agent is installed") {
		t.Fatalf("runWatchWithIO error = %v", err)
	}
	if strings.Contains(output.String(), "missing-claude") {
		t.Fatalf("watch output leaked a local path: %q", output.String())
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("watch events = %q, want startup and error", output.String())
	}
	var started, failed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &started); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &failed); err != nil {
		t.Fatal(err)
	}
	if started["event"] != "started" || failed["event"] != "error" {
		t.Fatalf("events = %+v, %+v", started, failed)
	}
	if failed["error"] != "no supported coding agent is installed" {
		t.Fatalf("error event = %+v", failed)
	}
}

func TestRunWatchRejectsBadOptionBeforeConfiguration(t *testing.T) {
	var output bytes.Buffer
	err := runWatchWithContext(context.Background(), []string{"--interval", "0s"}, &output)
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("error = %v, want interval validation error", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}
