package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestReadMaskedSecretInputShowsMaskAndSupportsBackspace(t *testing.T) {
	var output bytes.Buffer
	value, err := readMaskedSecretInput(strings.NewReader("ab\bc\n"), &output)
	if err != nil {
		t.Fatalf("readMaskedSecretInput: %v", err)
	}
	if got, want := string(value), "ac"; got != want {
		t.Fatalf("value = %q, want %q", got, want)
	}
	if got, want := output.String(), "**\b \b*"; got != want {
		t.Fatalf("mask output = %q, want %q", got, want)
	}
	if strings.Contains(output.String(), "ac") {
		t.Fatal("mask output contains the secret")
	}
}

func TestReadCommandOptionalSecretAllowsEmpty(t *testing.T) {
	var output bytes.Buffer
	value, err := readCommandOptionalSecret(strings.NewReader("\n"), &output, "test", "Token (optional): ")
	if err != nil {
		t.Fatalf("readCommandOptionalSecret: %v", err)
	}
	if value != "" {
		t.Fatalf("value = %q, want empty", value)
	}
	if !strings.Contains(output.String(), "Token (optional): ") {
		t.Fatalf("prompt output = %q", output.String())
	}
}

func TestReadCommandSecretStillRejectsEmpty(t *testing.T) {
	if _, err := readCommandSecret(strings.NewReader("\n"), &bytes.Buffer{}, "test", "Secret: "); err == nil || !strings.Contains(err.Error(), "secret cannot be empty") {
		t.Fatalf("readCommandSecret empty error = %v", err)
	}
}
func TestWriteRecoveryResetReminder(t *testing.T) {
	var prompt bytes.Buffer
	if err := writeRecoveryResetReminder(&prompt); err != nil {
		t.Fatalf("writeRecoveryResetReminder() error = %v", err)
	}
	if got, want := prompt.String(), recoveryResetReminder+"\n"; got != want {
		t.Fatalf("reminder = %q, want %q", got, want)
	}
}

func TestWriteRecoveryResetReminderRejectsMissingPrompt(t *testing.T) {
	if err := writeRecoveryResetReminder(nil); err == nil || err.Error() != "passphrase: prompt output is required" {
		t.Fatalf("writeRecoveryResetReminder(nil) error = %v", err)
	}
}

func TestPassphraseChangeReplacesRemoteEnvelopeWithoutChangingDataKey(t *testing.T) {
	const oldPassphrase = "alpha-secret-6f2d"
	const newPassphrase = "beta-secret-91ac"

	configDir, remoteRoot, keyfile, _ := preparePassphraseCommand(t, oldPassphrase)
	before, err := keyfile.UnlockWithPassphrase(oldPassphrase)
	if err != nil {
		t.Fatalf("unlock before change: %v", err)
	}
	beforePublic, err := before.IdentityPublic()
	if err != nil {
		t.Fatalf("identity before change: %v", err)
	}
	before.Close()

	var output bytes.Buffer
	input := strings.NewReader(oldPassphrase + "\n" + newPassphrase + "\n" + newPassphrase + "\n")
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	if err := runPassphraseWithIO([]string{"change"}, input, &output); err != nil {
		t.Fatalf("runPassphraseWithIO(change): %v", err)
	}
	if got := output.String(); !strings.Contains(got, "passphrase: change") {
		t.Fatalf("change output = %q", got)
	}
	if strings.Contains(output.String(), oldPassphrase) || strings.Contains(output.String(), newPassphrase) {
		t.Fatalf("change output leaked a passphrase: %q", output.String())
	}

	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := syncer.FetchKeyfile(context.Background(), store)
	if err != nil {
		t.Fatalf("fetch updated keyfile: %v", err)
	}
	if _, err := updated.UnlockWithPassphrase(oldPassphrase); !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("old passphrase error = %v, want ErrWrongPassphrase", err)
	}
	after, err := updated.UnlockWithPassphrase(newPassphrase)
	if err != nil {
		t.Fatalf("new passphrase does not unlock: %v", err)
	}
	afterPublic, err := after.IdentityPublic()
	if err != nil {
		t.Fatalf("identity after change: %v", err)
	}
	after.Close()
	if !bytes.Equal(beforePublic.Bytes(), afterPublic.Bytes()) {
		t.Fatal("passphrase change replaced the data-key identity")
	}
}

func TestPassphraseResetKeepsRecoveryKeyAndDataKey(t *testing.T) {
	const oldPassphrase = "alpha-secret-6f2d"
	const newPassphrase = "gamma-secret-3b7e"

	configDir, remoteRoot, keyfile, recovery := preparePassphraseCommand(t, oldPassphrase)
	before, err := keyfile.UnlockWithRecoveryKey(recovery)
	if err != nil {
		t.Fatalf("unlock with recovery key before reset: %v", err)
	}
	beforePublic, err := before.IdentityPublic()
	if err != nil {
		t.Fatalf("identity before reset: %v", err)
	}
	before.Close()

	var output bytes.Buffer
	input := strings.NewReader(recovery + "\n" + newPassphrase + "\n" + newPassphrase + "\n")
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	if err := runPassphraseWithIO([]string{"reset"}, input, &output); err != nil {
		t.Fatalf("runPassphraseWithIO(reset): %v", err)
	}
	if !strings.Contains(output.String(), recoveryResetReminder) || !strings.Contains(output.String(), "passphrase: reset") {
		t.Fatalf("reset output = %q", output.String())
	}
	if strings.Contains(output.String(), recovery) || strings.Contains(output.String(), newPassphrase) {
		t.Fatalf("reset output leaked a secret: %q", output.String())
	}

	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := syncer.FetchKeyfile(context.Background(), store)
	if err != nil {
		t.Fatalf("fetch reset keyfile: %v", err)
	}
	if _, err := updated.UnlockWithPassphrase(oldPassphrase); !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("old passphrase error after reset = %v, want ErrWrongPassphrase", err)
	}
	after, err := updated.UnlockWithPassphrase(newPassphrase)
	if err != nil {
		t.Fatalf("new passphrase after reset does not unlock: %v", err)
	}
	afterPublic, err := after.IdentityPublic()
	if err != nil {
		t.Fatalf("identity after reset: %v", err)
	}
	after.Close()
	if !bytes.Equal(beforePublic.Bytes(), afterPublic.Bytes()) {
		t.Fatal("passphrase reset replaced the data-key identity")
	}
	recovered, err := updated.UnlockWithRecoveryKey(recovery)
	if err != nil {
		t.Fatalf("recovery key stopped working after reset: %v", err)
	}
	recovered.Close()
}

func TestPassphraseChangeValidationFailureLeavesRemoteEnvelopeUntouched(t *testing.T) {
	const oldPassphrase = "alpha-secret-6f2d"
	const newPassphrase = "beta-secret-91ac"

	configDir, remoteRoot, _, _ := preparePassphraseCommand(t, oldPassphrase)
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)

	var output bytes.Buffer
	wrongCurrent := strings.NewReader("wrong-secret\n" + newPassphrase + "\n" + newPassphrase + "\n")
	err := runPassphraseWithIO([]string{"change"}, wrongCurrent, &output)
	if !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("wrong current passphrase error = %v, want ErrWrongPassphrase", err)
	}

	mismatched := strings.NewReader(oldPassphrase + "\n" + newPassphrase + "\nother-secret\n")
	if err := runPassphraseWithIO([]string{"change"}, mismatched, &output); err == nil || !strings.Contains(err.Error(), "new encryption passwords do not match") {
		t.Fatalf("mismatched new passphrase error = %v", err)
	}

	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := syncer.FetchKeyfile(context.Background(), store)
	if err != nil {
		t.Fatalf("fetch keyfile after rejected changes: %v", err)
	}
	if _, err := unchanged.UnlockWithPassphrase(oldPassphrase); err != nil {
		t.Fatalf("old passphrase stopped working after rejected changes: %v", err)
	}
	if _, err := unchanged.UnlockWithPassphrase(newPassphrase); !errors.Is(err, crypto.ErrWrongPassphrase) {
		t.Fatalf("new passphrase error after rejected changes = %v, want ErrWrongPassphrase", err)
	}
}
func preparePassphraseCommand(t *testing.T, passphrase string) (string, string, *crypto.Keyfile, string) {
	t.Helper()
	configDir := t.TempDir()
	remoteRoot := t.TempDir()
	keyfile, recovery, err := crypto.NewKeyfile(passphrase)
	if err != nil {
		t.Fatal(err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.PublishKeyfile(context.Background(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: filepath.Clean(remoteRoot)}
	c.IdentityPublic = public.Bytes()
	c.DomainFingerprint, err = syncDomainFingerprint(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	return configDir, remoteRoot, keyfile, recovery
}
