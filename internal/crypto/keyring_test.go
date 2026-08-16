package crypto

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"testing"
)

func TestManagedKeyfileRotationRevokesDeviceAndKeepsHistory(t *testing.T) {
	const oldPass = "old passphrase"
	kf, oldRecovery, err := NewKeyfile(oldPass)
	if err != nil {
		t.Fatal(err)
	}
	deviceA, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceB, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	if err := MigrateKeyfile(kf, oldPass, "devicea", deviceA.PublicKey()); err != nil {
		t.Fatalf("MigrateKeyfile: %v", err)
	}
	if err := RegisterManagedDevice(kf, oldPass, "deviceb", deviceB.PublicKey()); err != nil {
		t.Fatalf("RegisterManagedDevice: %v", err)
	}

	before, err := kf.UnlockKeyRingForDevice("devicea", deviceA)
	if err != nil {
		t.Fatalf("unlock device A before rotation: %v", err)
	}
	beforeCurrent := before.Current()
	if beforeCurrent == nil {
		t.Fatal("device A has no current epoch")
	}
	sealedOld, err := Encrypt(beforeCurrent.IdentityPublic, "v1/history", []byte("old history"))
	if err != nil {
		before.Close()
		t.Fatal(err)
	}
	before.Close()

	const newPass = "new passphrase"
	newRecovery, err := RotateManagedKeyfile(kf, oldPass, newPass, "deviceb")
	if err != nil {
		t.Fatalf("RotateManagedKeyfile: %v", err)
	}
	if newRecovery == oldRecovery {
		t.Fatal("rotation reused the recovery key")
	}

	after, err := kf.UnlockKeyRingForDevice("devicea", deviceA)
	if err != nil {
		t.Fatalf("unlock device A after rotation: %v", err)
	}
	defer after.Close()
	if after.Generation != 2 || after.Current() == nil {
		t.Fatalf("device A ring after rotation = %+v", after)
	}
	if len(after.Epochs) != 2 {
		t.Fatalf("device A retained %d epochs, want 2", len(after.Epochs))
	}
	if _, err := Decrypt(after.Epochs[0].IdentityPrivate, "v1/history", sealedOld); err != nil {
		t.Fatalf("historical ciphertext became unreadable: %v", err)
	}

	if _, err := kf.UnlockKeyRingForDevice("deviceb", deviceB); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("device B unlock error = %v, want ErrDeviceRevoked", err)
	}
	if _, err := kf.UnlockWithPassphrase(oldPass); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("old passphrase unlock error = %v, want ErrWrongPassphrase", err)
	}
	if _, err := kf.UnlockWithRecoveryKey(oldRecovery); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Fatalf("old recovery unlock error = %v, want ErrWrongRecoveryKey", err)
	}
	opened, err := kf.UnlockKeyRingWithPassphrase(newPass)
	if err != nil {
		t.Fatalf("new passphrase unlock: %v", err)
	}
	defer opened.Close()
	if !bytes.Equal(opened.IdentifierKey, after.IdentifierKey) {
		t.Fatal("identifier key changed during content-key rotation")
	}
	if recovered, err := kf.UnlockKeyRingWithRecoveryKey(newRecovery); err != nil {
		t.Fatalf("new recovery unlock: %v", err)
	} else {
		recovered.Close()
	}
}

func TestManagedKeyfileRejectsDuplicateDeviceIDAfterRevocation(t *testing.T) {
	kf, _, err := NewKeyfile("old")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateKeyfile(kf, "old", "devicea", first.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateManagedKeyfile(kf, "old", "new", "devicea"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterManagedDevice(kf, "new", "devicea", second.PublicKey()); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("re-registration error = %v, want ErrDeviceRevoked", err)
	}
}
func TestManagedKeyfileRotationKeepsActiveMembers(t *testing.T) {
	const (
		oldPass = "old passphrase"
		newPass = "new passphrase"
	)
	kf, _, err := NewKeyfile(oldPass)
	if err != nil {
		t.Fatal(err)
	}
	deviceA, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceB, err := NewDevicePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateKeyfile(kf, oldPass, "devicea", deviceA.PublicKey()); err != nil {
		t.Fatal(err)
	}
	if err := RegisterManagedDevice(kf, oldPass, "deviceb", deviceB.PublicKey()); err != nil {
		t.Fatal(err)
	}

	recovery, err := RotateManagedKeyfile(kf, oldPass, newPass, "")
	if err != nil {
		t.Fatalf("RotateManagedKeyfile: %v", err)
	}
	if kf.Generation != 2 || len(kf.Epochs) != 2 {
		t.Fatalf("rotated keyfile = generation %d, epochs %d", kf.Generation, len(kf.Epochs))
	}
	for _, member := range kf.Members {
		if member.RevokedAtGeneration != 0 {
			t.Fatalf("active rotation revoked member %q", member.DeviceID)
		}
	}
	for deviceID, private := range map[string]*ecdh.PrivateKey{"devicea": deviceA, "deviceb": deviceB} {
		ring, err := kf.UnlockKeyRingForDevice(deviceID, private)
		if err != nil {
			t.Fatalf("unlock %s after active rotation: %v", deviceID, err)
		}
		if ring.Generation != 2 || len(ring.Epochs) != 2 {
			t.Fatalf("%s ring = generation %d, epochs %d", deviceID, ring.Generation, len(ring.Epochs))
		}
		ring.Close()
	}
	if _, err := kf.UnlockWithPassphrase(oldPass); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("old passphrase error = %v", err)
	}
	if _, err := kf.UnlockKeyRingWithRecoveryKey(recovery); err != nil {
		t.Fatalf("new recovery unlock: %v", err)
	}
}
