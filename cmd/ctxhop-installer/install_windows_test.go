//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchInstallerWelcomeRejectsEmptyTarget(t *testing.T) {
	if err := launchInstallerWelcome(" "); err == nil {
		t.Fatal("empty installed executable path unexpectedly succeeded")
	}
}

func TestInstallerWelcomeScriptRunsInstalledCLI(t *testing.T) {
	script := installerWelcomeScript()
	if !strings.Contains(script, `.\ctxhop.exe --installer-welcome`) {
		t.Fatalf("welcome script = %q, want installed CLI invocation", script)
	}
	if strings.Contains(script, "del ") {
		t.Fatalf("welcome script deletes itself while cmd is still executing: %q", script)
	}
}

func TestInstallerWelcomeCommandCleansUpScript(t *testing.T) {
	targetPath := filepath.Join(`C:\Users\Ctx Hop`, ".ctxhop", "bin", "ctxhop.exe")
	scriptPath := filepath.Join(filepath.Dir(targetPath), ".ctxhop-welcome-test.cmd")
	if got, want := installerWelcomeCommandLine("cmd.exe", scriptPath), `cmd.exe /D /K "call .ctxhop-welcome-test.cmd & del /f /q .ctxhop-welcome-test.cmd >nul 2>&1"`; got != want {
		t.Fatalf("welcome command line = %q, want %q", got, want)
	}
}
