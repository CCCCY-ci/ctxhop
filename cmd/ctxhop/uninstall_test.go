package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

func TestPathWithin(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, ".ctxhop", "logs")
	sibling := filepath.Join(root, ".ctxhop-old")

	if !pathWithin(filepath.Join(root, ".ctxhop"), child) {
		t.Fatalf("pathWithin did not recognise %q below .ctxhop", child)
	}
	if pathWithin(filepath.Join(root, ".ctxhop"), sibling) {
		t.Fatalf("pathWithin treated sibling %q as a child", sibling)
	}
	if !pathWithin(root, filepath.Join(root, ".ctxhop")) {
		t.Fatal("pathWithin did not recognise a parent directory")
	}
}

func TestEnsureRemoteDataPreservedRefusesOverlappingDirBackend(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, ".ctxhop")

	for _, remotePath := range []string{
		filepath.Join(configDir, "sync"),
		filepath.Join(root, "sync", "..", ".ctxhop"),
		root,
	} {
		c := config.New()
		c.Remote = config.Remote{Type: "dir", Path: remotePath}
		if err := ensureRemoteDataPreserved(configDir, c); err == nil {
			t.Fatalf("overlapping remote path %q was accepted", remotePath)
		}
	}

	remote := filepath.Join(root, "sync")
	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: remote}
	if err := ensureRemoteDataPreserved(configDir, c); err != nil {
		t.Fatalf("separate remote path was rejected: %v", err)
	}

	c.Remote = config.Remote{Type: "s3", Bucket: "test-bucket"}
	if err := ensureRemoteDataPreserved(configDir, c); err != nil {
		t.Fatalf("S3 configuration was rejected: %v", err)
	}
}

func TestEnsureRemoteDataPreservedRefusesSymlinkedDirBackend(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, ".ctxhop")
	remotePath := filepath.Join(root, "sync-alias")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configDir, remotePath); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: remotePath}
	if err := ensureRemoteDataPreserved(configDir, c); err == nil {
		t.Fatal("symlinked remote path was accepted")
	}
}

func TestUninstallRemovesInstalledHooksWhenConfigIsMissing(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), ".ctxhop")
	claudeHome := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)

	if err := (adapter.Layout{Home: claudeHome}).InstallHook("ctxhop-test", false); err != nil {
		t.Fatalf("install Claude hook: %v", err)
	}
	if err := (adapter.CodexLayout{Home: codexHome}).InstallHook("ctxhop-test", false); err != nil {
		t.Fatalf("install Codex hook: %v", err)
	}

	installDir := t.TempDir()
	target := filepath.Join(installDir, installedExecutableName())
	if err := os.WriteFile(target, []byte("executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallLocalFiles(uninstallOptions{dir: installDir}); err != nil {
		t.Fatalf("uninstallLocalFiles: %v", err)
	}

	claudeInstalled, err := (adapter.Layout{Home: claudeHome}).HookInstalled()
	if err != nil {
		t.Fatal(err)
	}
	if claudeInstalled {
		t.Fatal("Claude Hook remained after uninstall without local configuration")
	}
	codexInstalled, err := (adapter.CodexLayout{Home: codexHome}).HookInstalled()
	if err != nil {
		t.Fatal(err)
	}
	if codexInstalled {
		t.Fatal("Codex Hook remained after uninstall without local configuration")
	}
}

func TestRemoveInstalledExecutableKeepsDirectoryBackend(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "bin")
	configDir := filepath.Join(root, ".ctxhop")
	remoteDir := filepath.Join(root, "sync")
	target := filepath.Join(installDir, installedExecutableName())

	for _, path := range []string{installDir, configDir, remoteDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, []byte("executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("local state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteDir, "session"), []byte("remote data"), 0o600); err != nil {
		t.Fatal(err)
	}

	scheduled, err := removeInstalledExecutable(target, configDir, false)
	if err != nil {
		t.Fatalf("removeInstalledExecutable: %v", err)
	}
	if scheduled {
		t.Fatal("non-running uninstall unexpectedly scheduled removal")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("installed executable still exists, stat error = %v", err)
	}
	if _, err := os.Stat(configDir); !os.IsNotExist(err) {
		t.Fatalf("configuration directory still exists, stat error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(remoteDir, "session"))
	if err != nil {
		t.Fatalf("directory-backend data was removed: %v", err)
	}
	if string(data) != "remote data" {
		t.Fatalf("directory-backend data changed to %q", data)
	}
}

func TestValidateUninstallConfigDirRefusesHomeAndFilesystemRoot(t *testing.T) {
	if home, err := os.UserHomeDir(); err == nil {
		if err := validateUninstallConfigDir(home); err == nil {
			t.Fatalf("user home %q was accepted for removal", home)
		}
	}

	root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
	if err := validateUninstallConfigDir(root); err == nil {
		t.Fatalf("filesystem root %q was accepted for removal", root)
	}
}
