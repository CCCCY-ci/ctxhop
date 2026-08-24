//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchInstallerWelcomeRejectsEmptyTarget(t *testing.T) {
	if err := launchInstallerWelcome(" "); err == nil {
		t.Fatal("empty installed executable path unexpectedly succeeded")
	}
}

func TestInstallerWelcomeEnvironmentFindsInstalledCommand(t *testing.T) {
	targetPath := filepath.Join(`C:\Users\Ctx Hop`, ".ctxhop", "bin", "ctxhop.exe")
	installDir := filepath.Dir(targetPath)
	wantPrefix := "PATH=" + installDir + string(os.PathListSeparator)
	var pathEntry string
	for _, entry := range installerWelcomeEnvironment(targetPath) {
		if strings.HasPrefix(strings.ToUpper(entry), "PATH=") {
			pathEntry = entry
		}
	}
	if !strings.HasPrefix(pathEntry, wantPrefix) {
		t.Fatalf("welcome PATH = %q, want prefix %q", pathEntry, wantPrefix)
	}
	if !strings.Contains(pathEntry, os.Getenv("PATH")) {
		t.Fatalf("welcome PATH did not preserve the existing PATH: %q", pathEntry)
	}
}

func TestInstallerWelcomeCommandQuotesPathsWithSpaces(t *testing.T) {
	targetPath := filepath.Join(`C:\Users\Ctx Hop`, ".ctxhop", "bin", "ctxhop.exe")
	want := `mode con: cols=120 lines=32 >nul 2>&1 & "` + targetPath + `" --installer-welcome`
	if got := installerWelcomeCommandLine(targetPath); got != want {
		t.Fatalf("welcome command = %q, want %q", got, want)
	}
}
