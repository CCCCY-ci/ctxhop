package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseInstallArgs(t *testing.T) {
	options, err := parseInstallArgs([]string{"--dir", "custom-bin", "--no-path"})
	if err != nil {
		t.Fatal(err)
	}
	if options.dir != "custom-bin" || !options.noPath {
		t.Fatalf("options = %+v, want custom directory and no-path", options)
	}

	if _, err := parseInstallArgs([]string{"unexpected"}); err == nil {
		t.Fatal("unexpected positional argument was accepted")
	}
}

func TestInstallExecutableFile(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source")
	targetPath := filepath.Join(root, "nested", "ctxhop")
	want := []byte("test executable")
	if err := os.WriteFile(sourcePath, want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installExecutableFile(sourcePath, targetPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("installed bytes = %q, want %q", got, want)
	}
}
