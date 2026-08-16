package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestInitDisplaysAndChecksDomainFingerprint(t *testing.T) {
	remoteRoot := t.TempDir()
	firstConfig := t.TempDir()
	claudeHome := filepath.Join(t.TempDir(), "missing-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("AGENTSYNC_CONFIG_DIR", firstConfig)

	var firstOutput bytes.Buffer
	firstInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "first-device",
		"--no-hook",
	}, firstInput, &firstOutput, "test-agentsync"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, err := config.Load(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := syncDomainFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstOutput.String(), "sync domain fingerprint: "+fingerprint) {
		t.Fatalf("init output does not show fingerprint %q: %s", fingerprint, firstOutput.String())
	}

	secondConfig := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", secondConfig)
	secondInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\n")
	err = runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "second-device",
		"--expect-domain-fingerprint", strings.Repeat("z", 26),
		"--no-hook",
	}, secondInput, ioDiscard{}, "test-agentsync")
	if err == nil || !strings.Contains(err.Error(), "expected domain fingerprint") {
		t.Fatalf("wrong expected fingerprint error = %v", err)
	}
	if _, err := config.Load(secondConfig); !errors.Is(err, config.ErrNotInitialised) {
		t.Fatalf("configuration after rejected domain = %v", err)
	}
}
