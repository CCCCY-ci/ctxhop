package crypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestKeyfilePathIsFixed(t *testing.T) {
	// New devices find the envelope without being told where it is.
	if KeyfilePath() == "" || !strings.HasPrefix(KeyfilePath(), "v1/") {
		t.Errorf("KeyfilePath() = %q", KeyfilePath())
	}
}

func TestAnEmptyPassphraseIsRefusedEverywhere(t *testing.T) {
	// An empty passphrase would derive a key anyone could reproduce, which is
	// indistinguishable from storing the data unencrypted.
	if _, _, err := NewKeyfile(""); err == nil {
		t.Error("NewKeyfile accepted an empty passphrase")
	}

	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kf.UnlockWithPassphrase(""); err == nil {
		t.Error("UnlockWithPassphrase accepted an empty passphrase")
	}
	if err := kf.ChangePassphrase(testPassphrase, ""); err == nil {
		t.Error("ChangePassphrase accepted an empty replacement")
	}
	if err := kf.ResetPassphrase(recovery, ""); err == nil {
		t.Error("ResetPassphrase accepted an empty replacement")
	}

	// After every refusal the keyfile still works.
	if _, err := kf.UnlockWithPassphrase(testPassphrase); err != nil {
		t.Errorf("a refused operation damaged the keyfile: %v", err)
	}
}

func TestResetAndUpgradeRefuseWrongSecrets(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	if err := kf.ResetPassphrase("AGSY-not-a-real-key", "next"); err == nil {
		t.Error("ResetPassphrase accepted a malformed recovery key")
	}

	kf.KDF.Time = 1
	if _, err := kf.UpgradeKDF("wrong passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("got %v, want ErrWrongPassphrase", err)
	}
}

func TestRecoveryKEKRejectsAWrongLengthKey(t *testing.T) {
	if _, err := recoveryKEK(make([]byte, 8)); err == nil {
		t.Error("a short recovery key was accepted")
	}
}

func TestDataKeyGuardsAgainstBeingUnset(t *testing.T) {
	var unset *DataKey
	if _, err := unset.IdentityPrivate(); err == nil {
		t.Error("an uninitialised data key produced an identity key")
	}
	if _, err := unset.IdentifierKey(); err == nil {
		t.Error("an uninitialised data key produced an identifier key")
	}
	// Closing one that was never opened must not panic.
	unset.Close()
}

func TestDataKeyDerivesDistinctSubkeys(t *testing.T) {
	// A compromise of one purpose must not hand over the other, and neither
	// can be mistaken for the other. This matters more now than it did under a
	// symmetric design: the identifier key is the one secret a pushing device
	// keeps on disk, so it must not lead back to the identity key (spec §3.3).
	dk := NewDataKey()
	identity, err := dk.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	identifier, err := dk.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(identity.Bytes(), identifier) {
		t.Error("the identity and identifier keys are the same")
	}
	if bytes.Equal(identity.Bytes(), dk.raw) {
		t.Error("the identity key is the data key itself")
	}
	if bytes.Equal(identifier, dk.raw) {
		t.Error("the identifier key is the data key itself")
	}
}

// TestTheIdentityKeyIsTheSameOnEveryDevice. Two machines unlock the same
// envelope and must arrive at the same keypair, or one could not read what the
// other pushed.
func TestTheIdentityKeyIsTheSameOnEveryDevice(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	viaPassphrase, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	viaRecovery, err := kf.UnlockWithRecoveryKey(recovery)
	if err != nil {
		t.Fatal(err)
	}

	first, err := viaPassphrase.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	second, err := viaRecovery.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("the two ways in produced different identity keys")
	}
	if !bytes.Equal(first.Bytes(), kf.IdentityPublic) {
		t.Error("the derived public key is not the one the keyfile advertises")
	}
}

// TestDataKeyCloseputsTheKeyBeyondUse covers a real defect found in review:
// Close zeroed the material but left a slice of the right length, so derive's
// guard still passed and every closed key produced the *same* content key -
// HKDF of thirty-two zero bytes, which anyone can compute. A use-after-close
// would have encrypted the user's sessions under a public constant and
// reported success, the one silent failure this package must not have.
func TestDataKeyCloseputsTheKeyBeyondUse(t *testing.T) {
	dk := NewDataKey()
	dk.Close()

	if len(dk.raw) != 0 {
		t.Error("key material survived Close")
	}
	if _, err := dk.IdentityPrivate(); err == nil {
		t.Error("a closed key still derived an identity key")
	}
	if _, err := dk.IdentifierKey(); err == nil {
		t.Error("a closed key still derived an identifier key")
	}

	// And crucially, two closed keys must not agree on anything.
	other := NewDataKey()
	other.Close()
	a, errA := dk.IdentifierKey()
	b, errB := other.IdentifierKey()
	if errA == nil && errB == nil && bytes.Equal(a, b) {
		t.Error("two closed keys derived the same content key")
	}
}

func TestNilKeyfileIsRefused(t *testing.T) {
	var kf *Keyfile
	if _, err := kf.UnlockWithPassphrase(testPassphrase); err == nil {
		t.Error("a nil keyfile unlocked")
	}
	if _, err := kf.UnlockWithRecoveryKey("AGSY-XXXX"); err == nil {
		t.Error("a nil keyfile unlocked")
	}
}

func TestKeyfileWithoutAnyWrappingIsRefused(t *testing.T) {
	// Nothing to open means the file is not a keyfile, however well-formed.
	kf := &Keyfile{Version: keyfileVersion}
	if _, err := kf.UnlockWithPassphrase(testPassphrase); err == nil {
		t.Error("an empty envelope unlocked")
	}
}

func TestWrappingRefusesAKeyfileWithUnusableParameters(t *testing.T) {
	// Wrapping derives a key from the stored parameters, so parameters that
	// fail validation must stop the wrap rather than produce something weak.
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	dataKey, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()

	broken := &Keyfile{Version: keyfileVersion, KDF: KDFParams{Name: "not-argon2"}}
	if err := broken.wrapWithPassphrase(dataKey, testPassphrase); err == nil {
		t.Error("wrapping succeeded with an unsupported derivation")
	}
}

func TestWrappingRefusesAShortRecoveryKey(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	dataKey, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()

	if err := kf.wrapWithRecoveryKey(dataKey, []byte("too short")); err == nil {
		t.Error("wrapping succeeded with a short recovery key")
	}
}

func TestUnlockWithAWellFormedButUnusableRecoveryKey(t *testing.T) {
	// The checksum passes but the value decodes to the wrong length, so the
	// failure has to come from the key derivation rather than from the parse.
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	short := FormatRecoveryKey(make([]byte, 8))
	if _, err := kf.UnlockWithRecoveryKey(short); err == nil {
		t.Error("a short recovery key unlocked the envelope")
	}
}

func TestDeriveRefusesAZeroedKey(t *testing.T) {
	// A closed key must not quietly derive usable subkeys.
	dk := NewDataKey()
	dk.raw = dk.raw[:8]
	if _, err := dk.IdentityPrivate(); err == nil {
		t.Error("a truncated data key derived an identity key")
	}
}

func TestDefaultKDFParamsAreStrongAndUnique(t *testing.T) {
	first := DefaultKDFParams()
	if err := first.validate(); err != nil {
		t.Errorf("the defaults do not pass our own validation: %v", err)
	}

	second := DefaultKDFParams()
	// A shared salt across users would let one precomputation attack many.
	if bytes.Equal(first.Salt, second.Salt) {
		t.Error("two calls produced the same salt")
	}
}
