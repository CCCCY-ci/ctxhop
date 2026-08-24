package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCommandTableIsUniqueAndFullyImplemented(t *testing.T) {
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if command.name == "" {
			t.Fatal("command table contains an empty name")
		}
		if _, exists := seen[command.name]; exists {
			t.Fatalf("command %q appears more than once", command.name)
		}
		seen[command.name] = struct{}{}
		if command.run == nil {
			t.Fatalf("command %q is declared without a handler", command.name)
		}
	}

	for _, name := range []string{"init", "install", "uninstall", "status", "list", "resume", "history", "passphrase", "stats", "push", "watch", "hook", "doctor", "device", "remote", "project", "pull", "version", "help", "completion"} {
		if _, exists := seen[name]; !exists {
			t.Errorf("expected command %q is missing", name)
		}
	}
}

func TestHelpListsTheImplementedCommandSurface(t *testing.T) {
	var output bytes.Buffer
	if err := writeHelp(&output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "not implemented yet") {
		t.Fatalf("help advertises an unimplemented command: %s", text)
	}
	if !strings.Contains(text, "ctxhop <command> [arguments]") {
		t.Fatalf("help does not use the ctxhop command name: %s", text)
	}
	legacyUsage := strings.ToLower("Agent" + "Sync <command>")
	if strings.Contains(text, legacyUsage) {
		t.Fatalf("help still advertises the legacy command name: %s", text)
	}
	for _, command := range commands {
		if !strings.Contains(text, "  "+command.name) {
			t.Errorf("help does not list command %q", command.name)
		}
	}
}

func TestVersionUsesCtxHopName(t *testing.T) {
	var output bytes.Buffer
	old := os.Stdout
	defer func() { os.Stdout = old }()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	if err := runVersion(nil); err != nil {
		_ = write.Close()
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&output, read); err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "ctxhop ") {
		t.Fatalf("version output = %q, want ctxhop prefix", output.String())
	}
}

func TestRunRejectsUnknownCommands(t *testing.T) {
	err := run([]string{"does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("run error = %v, want unknown command", err)
	}
	for _, removed := range []string{"session", "state", "restore"} {
		err := run([]string{removed})
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("removed command %q returned %v", removed, err)
		}
	}
}

func TestCommandHelpPathRecognizesNestedHelpFlags(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{args: []string{"status", "--help"}, want: []string{"status"}},
		{args: []string{"resume", "-h"}, want: []string{"resume"}},
		{args: []string{"remote", "delete-all", "--help"}, want: []string{"remote", "delete-all"}},
	}
	for _, tc := range cases {
		got, ok := commandHelpPath(tc.args)
		if !ok || strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("commandHelpPath(%v) = %v, %v; want %v, true", tc.args, got, ok, tc.want)
		}
	}
	if got, ok := commandHelpPath([]string{"status", "--json"}); ok || got != nil {
		t.Fatalf("commandHelpPath without help = %v, %v; want nil, false", got, ok)
	}
}
