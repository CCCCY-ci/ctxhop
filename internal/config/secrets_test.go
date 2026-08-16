package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearCredentialEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv(envAccessKeyID, "")
	t.Setenv(envSecretAccessKey, "")
	t.Setenv(envSessionToken, "")
}

func TestSecretsRoundTrip(t *testing.T) {
	clearCredentialEnvironment(t)
	dir := t.TempDir()
	want := &Secrets{
		Credentials:   Credentials{AccessKeyID: "stored-id", SecretAccessKey: "stored-secret", SessionToken: "stored-token"},
		IdentifierKey: []byte("0123456789abcdef0123456789abcdef"),
	}

	if err := SaveSecrets(dir, want); err != nil {
		t.Fatalf("SaveSecrets: %v", err)
	}
	got, err := LoadSecrets(dir)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if got.Credentials != want.Credentials || string(got.IdentifierKey) != string(want.IdentifierKey) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if info, err := os.Stat(filepath.Join(dir, deviceKeyFile)); err != nil || info.Size() != deviceKeyLen {
		t.Fatalf("device key stat = %v, info = %+v", err, info)
	}
}

func TestEnvironmentCredentialsOverrideStoredCredentials(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSecrets(dir, &Secrets{
		Credentials:   Credentials{AccessKeyID: "stored-id", SecretAccessKey: "stored-secret"},
		IdentifierKey: []byte("0123456789abcdef0123456789abcdef"),
	}); err != nil {
		t.Fatalf("SaveSecrets: %v", err)
	}
	t.Setenv(envAccessKeyID, "environment-id")
	t.Setenv(envSecretAccessKey, "environment-secret")
	t.Setenv(envSessionToken, "environment-token")

	got, err := LoadSecrets(dir)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	wantCredentials := Credentials{AccessKeyID: "environment-id", SecretAccessKey: "environment-secret", SessionToken: "environment-token"}
	if got.Credentials != wantCredentials {
		t.Fatalf("credentials = %+v, want %+v", got.Credentials, wantCredentials)
	}
	if string(got.IdentifierKey) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("identifier key was not preserved: %x", got.IdentifierKey)
	}
}

func TestPartialEnvironmentIsRejected(t *testing.T) {
	dir := t.TempDir()
	clearCredentialEnvironment(t)
	t.Setenv(envAccessKeyID, "environment-id")

	_, err := LoadSecrets(dir)
	if !errors.Is(err, ErrPartialEnvironment) {
		t.Fatalf("LoadSecrets error = %v, want ErrPartialEnvironment", err)
	}
}

func TestDeviceKeyReportsEntropyFailureWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	previous := readRandom
	readRandom = func([]byte) (int, error) {
		return 0, errors.New("entropy unavailable")
	}
	t.Cleanup(func() { readRandom = previous })

	_, err := deviceKey(dir)
	if err == nil || !strings.Contains(err.Error(), "generate this machine") {
		t.Fatalf("deviceKey error = %v, want entropy error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, deviceKeyFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("device key exists after entropy failure: %v", statErr)
	}
}

func TestLoadSecretsReportsDamagedDeviceKey(t *testing.T) {
	clearCredentialEnvironment(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, secretsFile), []byte("sealed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, deviceKeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSecrets(dir)
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("LoadSecrets error = %v, want damaged key error", err)
	}
}
