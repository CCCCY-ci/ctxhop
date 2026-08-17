package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDirDefaultsToHomeDotAgentsync(t *testing.T) {
	t.Setenv(dirEnv, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".agentsync")
	if got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestDirHonorsExplicitOverride(t *testing.T) {
	t.Setenv(dirEnv, filepath.Join(t.TempDir(), "custom-config"))

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(os.Getenv(dirEnv))
	if got != want {
		t.Fatalf("Dir() = %q, want %q", got, want)
	}
}

func TestMigrateLegacyCopiesAvailableFiles(t *testing.T) {
	t.Setenv(dirEnv, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(filepath.Join(home, ".agentsync")) == filepath.Clean(filepath.Join(configRoot, "agentsync")) {
		t.Skip("legacy and current configuration directories are the same")
	}

	legacy := filepath.Join(configRoot, "agentsync")
	legacyConfig := filepath.Join(legacy, configFile)
	if _, err := os.Stat(legacyConfig); errors.Is(err, os.ErrNotExist) {
		t.Skip("the host has no legacy configuration to migrate")
	}

	target := t.TempDir()
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyLegacyFiles(legacy, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{configFile, secretsFile, deviceKeyFile} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("migrated %s: %v", name, err)
		}
	}
}

func TestCopyLegacyDirectoryCopiesStateFiles(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	nested := filepath.Join(source, "v1", "projects", "project", "sessions")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("cursor-state")
	sourceFile := filepath.Join(nested, "cursor.json")
	if err := os.WriteFile(sourceFile, want, 0o600); err != nil {
		t.Fatal(err)
	}

	targetState := filepath.Join(target, "state")
	if err := copyLegacyDirectory(filepath.Join(source), targetState); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(targetState, "v1", "projects", "project", "sessions", "cursor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("copied state = %q, want %q", got, want)
	}
}
