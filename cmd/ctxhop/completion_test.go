package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCompletionCommandRendersAllSupportedShells(t *testing.T) {
	cases := []struct {
		shell  string
		marker string
	}{
		{shell: "bash", marker: "complete -F _ctxhop_completion ctxhop"},
		{shell: "zsh", marker: "#compdef ctxhop"},
		{shell: "fish", marker: "complete -c ctxhop"},
		{shell: "powershell", marker: "Register-ArgumentCompleter"},
		{shell: "pwsh", marker: "Register-ArgumentCompleter"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			var output bytes.Buffer
			if err := runCompletionWithIO([]string{tc.shell}, &output); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if !strings.Contains(text, tc.marker) || !strings.Contains(text, "resume") || !strings.Contains(text, "--allow-divergent") || !strings.Contains(text, "--workspace") || !strings.Contains(text, "hook") || !strings.Contains(text, "--help") {
				t.Fatalf("completion output is incomplete: %s", text)
			}
			if strings.Contains(text, "session preview") || strings.Contains(text, "state preview") || strings.Contains(text, "--include-project-state") || strings.Contains(text, "--include-workspace") || strings.Contains(text, "--include-git-state") || strings.Contains(text, "--session") {
				t.Fatalf("completion output contains a removed project-state option: %s", text)
			}
			if strings.Contains(text, `\n`) {
				t.Fatal("completion output contains a literal backslash-n")
			}
		})
	}
}

func TestCompletionRejectsUnknownShellAndRegistersCommand(t *testing.T) {
	if _, err := parseCompletionShell([]string{"cmd"}); err == nil {
		t.Fatal("unknown shell was accepted")
	}
	if _, err := parseCompletionShell(nil); err == nil {
		t.Fatal("missing shell was accepted")
	}
	for _, command := range commands {
		if command.name == "completion" {
			if command.run == nil {
				t.Fatal("completion command has no handler")
			}
			return
		}
	}
	t.Fatal("completion command is missing")
}
