package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestHookInstallCommandRegistersCodexForExistingConfig(t *testing.T) {
	configDir := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", configDir)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))

	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: t.TempDir()}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runHookWithIO(
		[]string{"install", "--agent", "codex"},
		strings.NewReader("y\n"),
		&output,
		"C:\\agentsync\\agentsync.exe",
	); err != nil {
		t.Fatalf("runHookWithIO: %v", err)
	}

	loaded, err := config.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Agents["codex"].HookInstalled {
		t.Fatalf("agents = %+v", loaded.Agents)
	}
	hooks, err := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hooks), "agentsync.exe") {
		t.Fatalf("hooks.json = %s", hooks)
	}
	if !strings.Contains(output.String(), "restart Codex") || !strings.Contains(output.String(), "complete") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestParseHookOptions(t *testing.T) {
	cases := []struct {
		args  []string
		agent string
	}{
		{args: []string{"install"}, agent: "all"},
		{args: []string{"install", "--agent", "codex"}, agent: "codex"},
		{args: []string{"install", "--agent", "claude"}, agent: "claude-code"},
	}
	for _, tc := range cases {
		got, err := parseHookOptions(tc.args)
		if err != nil {
			t.Fatalf("parseHookOptions(%v): %v", tc.args, err)
		}
		if got.agent != tc.agent {
			t.Fatalf("parseHookOptions(%v) = %+v", tc.args, got)
		}
	}
	if _, err := parseHookOptions([]string{"remove"}); err == nil {
		t.Fatal("unsupported hook action was accepted")
	}
	if _, err := parseHookOptions([]string{"install", "--agent", "unknown"}); err == nil {
		t.Fatal("unsupported hook agent was accepted")
	}
}

func TestHookCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "hook" {
			if command.run == nil {
				t.Fatal("hook command has no handler")
			}
			return
		}
	}
	t.Fatal("hook command is missing")
}
