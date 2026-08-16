package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestDeviceInviteInitializesSecondDeviceInSameDomain(t *testing.T) {
	remoteRoot := t.TempDir()
	firstConfig := t.TempDir()
	invitePath := filepath.Join(t.TempDir(), "agentsync-invite.json")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("AGENTSYNC_CONFIG_DIR", firstConfig)

	firstInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "first-device",
		"--no-hook",
	}, firstInput, ioDiscard{}, "test-agentsync"); err != nil {
		t.Fatalf("first init: %v", err)
	}

	var inviteOutput bytes.Buffer
	if err := runDeviceWithIO([]string{"invite", "--output", invitePath}, &inviteOutput); err != nil {
		t.Fatalf("create device invite: %v", err)
	}
	if !strings.Contains(inviteOutput.String(), "device invite: wrote") {
		t.Fatalf("invite output = %q", inviteOutput.String())
	}
	inviteData, err := os.ReadFile(invitePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inviteData), initTestPassphrase) {
		t.Fatal("device invite contains the passphrase")
	}

	first, err := config.Load(firstConfig)
	if err != nil {
		t.Fatal(err)
	}

	secondConfig := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", secondConfig)
	var secondOutput bytes.Buffer
	secondInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\n")
	if err := runInitWithIO([]string{
		"--invite", invitePath,
		"--device-name", "second-device",
		"--no-hook",
	}, secondInput, &secondOutput, "test-agentsync"); err != nil {
		t.Fatalf("second init with invite: %v", err)
	}
	if !strings.Contains(secondOutput.String(), "sync domain joined via invitation from: first-device") {
		t.Fatalf("second init output = %q", secondOutput.String())
	}

	second, err := config.Load(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	if first.Device.ID == second.Device.ID {
		t.Fatalf("two devices reused the same ID: %q", first.Device.ID)
	}
	if first.DomainFingerprint == "" || first.DomainFingerprint != second.DomainFingerprint {
		t.Fatalf("domain fingerprints differ: first=%q second=%q", first.DomainFingerprint, second.DomainFingerprint)
	}
	if !bytes.Equal(first.IdentityPublic, second.IdentityPublic) {
		t.Fatal("second device did not pin the existing domain identity")
	}
	if first.Remote != second.Remote {
		t.Fatalf("remote settings differ: first=%+v second=%+v", first.Remote, second.Remote)
	}
}

func TestDeviceInviteTamperingDoesNotSaveSecondConfiguration(t *testing.T) {
	remoteRoot := t.TempDir()
	firstConfig := t.TempDir()
	invitePath := filepath.Join(t.TempDir(), "agentsync-invite.json")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("AGENTSYNC_CONFIG_DIR", firstConfig)

	firstInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "first-device",
		"--no-hook",
	}, firstInput, ioDiscard{}, "test-agentsync"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	if err := runDeviceWithIO([]string{"invite", "--output", invitePath}, ioDiscard{}); err != nil {
		t.Fatalf("create device invite: %v", err)
	}

	data, err := os.ReadFile(invitePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), "\"name\": \"first-device\"", "\"name\": \"tampered\"", 1)
	if tampered == string(data) {
		t.Fatal("test invitation did not contain the expected issuer name")
	}
	if err := os.WriteFile(invitePath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	secondConfig := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", secondConfig)
	secondInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\n")
	err = runInitWithIO([]string{
		"--invite", invitePath,
		"--device-name", "second-device",
		"--no-hook",
	}, secondInput, ioDiscard{}, "test-agentsync")
	if err == nil || !strings.Contains(err.Error(), "invitation proof") {
		t.Fatalf("tampered invite error = %v", err)
	}
	if _, err := config.Load(secondConfig); !errors.Is(err, config.ErrNotInitialised) {
		t.Fatalf("configuration after tampered invite = %v", err)
	}
}
