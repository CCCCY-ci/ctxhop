package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDeviceIDGeneratesAndPersistsOneOpaqueIdentifier(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.Remote.Type = "dir"
	key := []byte("0123456789abcdef0123456789abcdef")

	id, err := EnsureDeviceID(dir, c, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeviceID(id); err != nil {
		t.Fatalf("generated device ID = %q: %v", id, err)
	}
	if c.Device.ID != id {
		t.Fatalf("config device ID = %q, want %q", c.Device.ID, id)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Device.ID != id {
		t.Fatalf("persisted device ID = %q, want %q", loaded.Device.ID, id)
	}

	otherKey := []byte("fedcba9876543210fedcba9876543210")
	again, err := EnsureDeviceID(dir, c, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("existing device ID changed from %q to %q", id, again)
	}
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "0123456789abcdef0123456789abcdef") {
		t.Fatal("identifier key was written to the configuration")
	}
}

func TestValidateDeviceIDRejectsUnsafeValues(t *testing.T) {
	for _, id := range []string{"", "Device", "device-id", "device/id", strings.Repeat("a", maxDeviceIDLength+1)} {
		if err := ValidateDeviceID(id); err == nil || !errors.Is(err, ErrInvalidDeviceID) {
			t.Errorf("ValidateDeviceID(%q) error = %v, want ErrInvalidDeviceID", id, err)
		}
	}
	if err := ValidateDeviceID("device01abc"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDeviceIDRefusesMissingOrInvalidIdentity(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.Remote.Type = "dir"
	if _, err := EnsureDeviceID(dir, c, nil); !errors.Is(err, ErrDeviceIdentityRequired) {
		t.Fatalf("missing identifier key error = %v, want ErrDeviceIdentityRequired", err)
	}
	if _, err := EnsureDeviceID(dir, c, []byte("short")); err == nil {
		t.Fatal("short identifier key unexpectedly succeeded")
	}
	if _, err := EnsureDeviceID("", c, []byte("0123456789abcdef0123456789abcdef")); err == nil {
		t.Fatal("empty config directory unexpectedly succeeded")
	}
	if _, err := EnsureDeviceID(dir, nil, []byte("0123456789abcdef0123456789abcdef")); err == nil {
		t.Fatal("nil config unexpectedly succeeded")
	}

	c.Device.ID = "device-id"
	if _, err := EnsureDeviceID(dir, c, nil); !errors.Is(err, ErrInvalidDeviceID) {
		t.Fatalf("invalid existing ID error = %v, want ErrInvalidDeviceID", err)
	}
}
