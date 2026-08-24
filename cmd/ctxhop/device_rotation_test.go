package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestDeviceRemoveRotatesKeyAndRevokesDevice(t *testing.T) {
	const (
		oldPass = "old passphrase"
		newPass = "new passphrase"
	)
	line := string(rune(10))
	remoteRoot := t.TempDir()
	claudeHome := filepath.Join(t.TempDir(), "missing-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)

	firstConfig := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", firstConfig)
	firstInput := strings.NewReader(oldPass + line + oldPass + line + "saved" + line)
	if err := runInitWithIO([]string{"--backend", "dir", "--path", remoteRoot, "--device-name", "first", "--no-hook"}, firstInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, err := config.Load(firstConfig)
	if err != nil {
		t.Fatal(err)
	}

	invitePath := filepath.Join(t.TempDir(), "first-invite.json")
	if err := runDeviceWithIO([]string{"invite", "--output", invitePath}, ioDiscard{}); err != nil {
		t.Fatalf("create invite: %v", err)
	}

	secondConfig := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", secondConfig)
	secondInput := strings.NewReader(oldPass + line + oldPass + line)
	if err := runInitWithIO([]string{"--invite", invitePath, "--device-name", "second", "--no-hook"}, secondInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("second init: %v", err)
	}
	second, err := config.Load(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondSecrets, err := config.LoadSecrets(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondPrivate, err := crypto.ParseDevicePrivateKey(secondSecrets.DevicePrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CTXHOP_CONFIG_DIR", firstConfig)
	var removeOutput bytes.Buffer
	removeInput := strings.NewReader(oldPass + line + newPass + line + newPass + line + "saved" + line)
	if err := runDeviceWithStreams([]string{"remove", "--yes", second.Device.ID}, removeInput, &removeOutput, &removeOutput); err != nil {
		t.Fatalf("remove device: %v; output=%s", err, removeOutput.String())
	}
	if !strings.Contains(removeOutput.String(), "generation=2") || !strings.Contains(removeOutput.String(), "New Recovery Key") {
		t.Fatalf("remove output = %q", removeOutput.String())
	}

	firstAfter, err := config.Load(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfter.DomainGeneration != 2 {
		t.Fatalf("first generation = %d, want 2", firstAfter.DomainGeneration)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	keyfile, err := syncer.FetchKeyfile(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if keyfile.Generation != 2 {
		t.Fatalf("remote generation = %d, want 2", keyfile.Generation)
	}
	if _, err := keyfile.UnlockKeyRingForDevice(second.Device.ID, secondPrivate); !errors.Is(err, crypto.ErrDeviceRevoked) {
		t.Fatalf("revoked device unlock error = %v", err)
	}
	if _, err := keyfile.UnlockWithPassphrase(oldPass); !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("old passphrase error = %v", err)
	}

	var nextInviteOutput bytes.Buffer
	if err := runDeviceWithIO([]string{"invite", "--output", invitePath}, &nextInviteOutput); err != nil {
		t.Fatalf("create post-rotation invite: %v", err)
	}
	thirdConfig := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", thirdConfig)
	thirdInput := strings.NewReader(newPass + line + newPass + line)
	if err := runInitWithIO([]string{"--invite", invitePath, "--device-name", "third", "--no-hook"}, thirdInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("third init after rotation: %v", err)
	}
	third, err := config.Load(thirdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if third.DomainGeneration != 2 || third.Device.ID == first.Device.ID || third.Device.ID == second.Device.ID {
		t.Fatalf("third config = %+v", third)
	}
}
func TestLegacyDeviceRotationDoesNotPublishIntermediateMigration(t *testing.T) {
	const oldPass = "old passphrase"
	line := string(rune(10))
	remoteRoot := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)

	keyfile, _, err := crypto.NewKeyfile(oldPass)
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.PublishKeyfile(t.Context(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	dataKey, err := keyfile.UnlockWithPassphrase(oldPass)
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	dataKey.Close()
	if err != nil {
		t.Fatal(err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := config.GenerateDeviceID(identifierKey)
	if err != nil {
		t.Fatal(err)
	}

	c := config.New()
	c.Device = config.Device{ID: deviceID, Name: "first", Mode: config.DeviceModeNormal}
	c.Remote = config.Remote{Type: "dir", Path: remoteRoot}
	c.IdentityPublic = public.Bytes()
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveSecrets(configDir, &config.Secrets{IdentifierKey: identifierKey}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	rotationInput := strings.NewReader(oldPass + line + "new passphrase" + line + "new passphrase" + line + "no" + line)
	err = runDeviceWithStreams([]string{"rotate-key"}, rotationInput, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("cancelled rotation error = %v, output=%q", err, output.String())
	}

	keyfile, err = syncer.FetchKeyfile(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if keyfile.Version != 1 || keyfile.IsManaged() {
		t.Fatalf("cancelled legacy rotation published keyfile version %d", keyfile.Version)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets.DevicePrivateKey) != 0 {
		t.Fatal("cancelled legacy rotation persisted a device private key")
	}
}
