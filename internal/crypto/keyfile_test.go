package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

const testPassphrase = "correct horse battery staple"

func TestNewKeyfileUnlocksBothWays(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatalf("NewKeyfile: %v", err)
	}

	byPass, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("UnlockWithPassphrase: %v", err)
	}
	byRecovery, err := kf.UnlockWithRecoveryKey(recovery)
	if err != nil {
		t.Fatalf("UnlockWithRecoveryKey: %v", err)
	}

	// Both wrappings must open the *same* key, or half the data would be
	// unreadable through one of the two doors.
	if !bytes.Equal(byPass.raw, byRecovery.raw) {
		t.Error("the two wrappings hold different keys")
	}
}

func TestKeyfileRejectsWrongSecrets(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := kf.UnlockWithPassphrase("wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("got %v, want ErrWrongPassphrase", err)
	}

	// A different, well-formed recovery key.
	_, otherText := NewRecoveryKey()
	if _, err := kf.UnlockWithRecoveryKey(otherText); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Errorf("got %v, want ErrWrongRecoveryKey", err)
	}

	// The right one still works after the failures.
	if _, err := kf.UnlockWithRecoveryKey(recovery); err != nil {
		t.Errorf("the correct recovery key stopped working: %v", err)
	}
}

// TestChangePassphraseKeepsExistingCiphertextReadable is the reason the design
// is an envelope. Deriving content keys from the passphrase would mean changing
// a password re-encrypts the user's entire history.
func TestChangePassphraseKeepsExistingCiphertextReadable(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	before, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	public, err := before.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Encrypt(public, "v1/a", []byte("written under the old passphrase"))
	if err != nil {
		t.Fatal(err)
	}

	const next = "a completely different passphrase"
	if err := kf.ChangePassphrase(testPassphrase, next); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}

	after, err := kf.UnlockWithPassphrase(next)
	if err != nil {
		t.Fatalf("the new passphrase does not unlock: %v", err)
	}
	private, err := after.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decrypt(private, "v1/a", sealed)
	if err != nil {
		t.Fatalf("data written before the change became unreadable: %v", err)
	}
	if string(got) != "written under the old passphrase" {
		t.Errorf("got %q", got)
	}

	// The old passphrase must stop working, and the recovery key must not.
	if _, err := kf.UnlockWithPassphrase(testPassphrase); !errors.Is(err, ErrWrongPassphrase) {
		t.Error("the old passphrase still unlocks")
	}
	if _, err := kf.UnlockWithRecoveryKey(recovery); err != nil {
		t.Errorf("changing the passphrase broke the recovery key: %v", err)
	}
}

func TestChangePassphraseUsesAFreshSalt(t *testing.T) {
	// Reusing the salt would let anyone holding both versions of the file test
	// a guess against either one.
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	oldSalt := bytes.Clone(kf.KDF.Salt)

	if err := kf.ChangePassphrase(testPassphrase, "next passphrase"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldSalt, kf.KDF.Salt) {
		t.Error("the salt was reused")
	}
}

// TestARefusedChangeLeavesTheKeyfileIntact covers a real defect this test
// found: the parameters were replaced before the new wrapping was known to
// succeed, so a rejected change left a salt that no longer matched the wrapped
// key - the old passphrase stopped working even though nothing was supposed to
// have changed, locking the user out of everything but their recovery key.
func TestARefusedChangeLeavesTheKeyfileIntact(t *testing.T) {
	attempts := map[string]func(kf *Keyfile, recovery string) error{
		"change to an empty passphrase": func(kf *Keyfile, _ string) error {
			return kf.ChangePassphrase(testPassphrase, "")
		},
		"reset to an empty passphrase": func(kf *Keyfile, recovery string) error {
			return kf.ResetPassphrase(recovery, "")
		},
		"upgrade with the wrong passphrase": func(kf *Keyfile, _ string) error {
			_, err := kf.UpgradeKDF("not the passphrase")
			return err
		},
	}

	for name, attempt := range attempts {
		t.Run(name, func(t *testing.T) {
			kf, recovery, err := NewKeyfile(testPassphrase)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, "upgrade") {
				// A keyfile that is weak *and* self-consistent, so the only
				// thing under test is the refusal itself.
				weakenConsistently(t, kf)
			}
			saltBefore := bytes.Clone(kf.KDF.Salt)
			wrappedBefore := bytes.Clone(kf.WrappedByPassphrase)

			if err := attempt(kf, recovery); err == nil {
				t.Fatal("expected the attempt to be refused")
			}

			if !bytes.Equal(saltBefore, kf.KDF.Salt) {
				t.Error("a refused attempt replaced the salt")
			}
			if !bytes.Equal(wrappedBefore, kf.WrappedByPassphrase) {
				t.Error("a refused attempt replaced the wrapped key")
			}
			if _, err := kf.UnlockWithPassphrase(testPassphrase); err != nil {
				t.Errorf("the original passphrase stopped working: %v", err)
			}
		})
	}
}

// weakenConsistently rewraps a keyfile under weaker settings, the way one
// written by an older release would look.
func weakenConsistently(t *testing.T, kf *Keyfile) {
	t.Helper()

	dataKey, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()

	weak := kf.KDF
	weak.Time = 1
	weak.MemoryKiB = 16 * 1024
	if err := kf.rewrap(dataKey, testPassphrase, weak); err != nil {
		t.Fatal(err)
	}
}

func TestChangePassphraseRefusesTheWrongCurrent(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if err := kf.ChangePassphrase("not it", "next"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("got %v, want ErrWrongPassphrase", err)
	}
	// And nothing changed.
	if _, err := kf.UnlockWithPassphrase(testPassphrase); err != nil {
		t.Errorf("a refused change damaged the keyfile: %v", err)
	}
}

func TestResetPassphraseWithTheRecoveryKey(t *testing.T) {
	// The user who forgot their passphrase entirely.
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	before, err := kf.UnlockWithRecoveryKey(recovery)
	if err != nil {
		t.Fatal(err)
	}
	beforePublic, err := before.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}

	if err := kf.ResetPassphrase(recovery, "a brand new passphrase"); err != nil {
		t.Fatalf("ResetPassphrase: %v", err)
	}

	after, err := kf.UnlockWithPassphrase("a brand new passphrase")
	if err != nil {
		t.Fatalf("the reset passphrase does not unlock: %v", err)
	}
	afterPublic, err := after.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforePublic.Bytes(), afterPublic.Bytes()) {
		t.Error("resetting the passphrase changed the identity key")
	}
}

func TestUpgradeKDF(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	// A keyfile as an older release would have written it.
	weakenConsistently(t, kf)

	changed, err := kf.UpgradeKDF(testPassphrase)
	if err != nil {
		t.Fatalf("UpgradeKDF: %v", err)
	}
	if !changed {
		t.Fatal("weaker settings were not upgraded")
	}

	// Still unlocks, now under stronger settings.
	if _, err := kf.UnlockWithPassphrase(testPassphrase); err != nil {
		t.Fatalf("upgrading broke the keyfile: %v", err)
	}

	// And a second call is a no-op, so callers do not rewrite the file forever.
	changed, err = kf.UpgradeKDF(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("an already-current keyfile was upgraded again")
	}
}

func TestKeyfileRoundTripsThroughStorage(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	data, err := MarshalKeyfile(kf)
	if err != nil {
		t.Fatalf("MarshalKeyfile: %v", err)
	}

	// The envelope is stored unencrypted, so it must not contain either secret.
	if bytes.Contains(data, []byte(testPassphrase)) {
		t.Error("the keyfile contains the passphrase")
	}
	if bytes.Contains(data, []byte(strings.ReplaceAll(recovery, "-", ""))) {
		t.Error("the keyfile contains the recovery key")
	}

	back, err := ParseKeyfile(data)
	if err != nil {
		t.Fatalf("ParseKeyfile: %v", err)
	}
	if _, err := back.UnlockWithPassphrase(testPassphrase); err != nil {
		t.Errorf("a keyfile did not survive storage: %v", err)
	}
}

func TestParseKeyfileRejectsRubbish(t *testing.T) {
	for _, data := range []string{
		"",
		"not json",
		`{"version": 1}`,
		`{"version": 99, "wrappedByPassphrase": "AA=="}`,
	} {
		if _, err := ParseKeyfile([]byte(data)); err == nil {
			t.Errorf("ParseKeyfile(%q) succeeded", data)
		}
	}
}

// TestAnEnvelopeFromANewerReleaseIsNotCalledAWrongSecret guards the remedy the
// user is pointed at. Reporting a newer format as "wrong passphrase" leads
// straight to re-running init over v1/keyfile, which orphans every session
// already uploaded.
func TestAnEnvelopeFromANewerReleaseIsNotCalledAWrongSecret(t *testing.T) {
	for name, damage := range map[string]func(*Keyfile){
		"passphrase wrapping": func(k *Keyfile) { k.WrappedByPassphrase[len(objectMagic)] = objectVersion + 1 },
		"recovery wrapping":   func(k *Keyfile) { k.WrappedByRecoveryKey[len(objectMagic)] = objectVersion + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			kf, recovery, err := NewKeyfile(testPassphrase)
			if err != nil {
				t.Fatal(err)
			}
			damage(kf)

			_, passErr := kf.UnlockWithPassphrase(testPassphrase)
			_, recErr := kf.UnlockWithRecoveryKey(recovery)

			err = passErr
			if !errors.Is(err, ErrUnsupportedVersion) {
				err = recErr
			}
			if !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("neither path reported the version: %v / %v", passErr, recErr)
			}
			if errors.Is(err, ErrWrongPassphrase) || errors.Is(err, ErrWrongRecoveryKey) {
				t.Error("a newer format was reported as a wrong secret")
			}
			if !strings.Contains(err.Error(), "upgrade") {
				t.Errorf("the message should name the remedy: %v", err)
			}
		})
	}
}

// TestAMissingWrappingSaysSoRatherThanBlamingTheSecret keeps the user from
// retyping a passphrase that could never have worked.
func TestAMissingWrappingSaysSoRatherThanBlamingTheSecret(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	noPassphrase := *kf
	noPassphrase.WrappedByPassphrase = nil
	_, err = noPassphrase.UnlockWithPassphrase(testPassphrase)
	if err == nil || errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("got %v, want an explanation of the missing wrapping", err)
	}
	if !strings.Contains(err.Error(), "recovery key") {
		t.Errorf("the message should name the other way in: %v", err)
	}

	noRecovery := *kf
	noRecovery.WrappedByRecoveryKey = nil
	_, err = noRecovery.UnlockWithRecoveryKey(recovery)
	if err == nil || errors.Is(err, ErrWrongRecoveryKey) {
		t.Errorf("got %v, want an explanation of the missing wrapping", err)
	}
}

func TestUpgradeKDFSurvivesANilKeyfile(t *testing.T) {
	// Every other method on *Keyfile does, so a caller on a "no keyfile yet"
	// path must not be the one to find out this one does not.
	var kf *Keyfile
	if _, err := kf.UpgradeKDF(testPassphrase); err == nil {
		t.Error("a nil keyfile was upgraded")
	}
}

// TestMarshalRefusesAnEnvelopeHoldingNoKey covers the object whose loss locks
// the user out of everything: the caller writes this straight over the existing
// one, so refusing is the only safe answer (BR-12).
func TestMarshalRefusesAnEnvelopeHoldingNoKey(t *testing.T) {
	for name, kf := range map[string]*Keyfile{
		"nil":           nil,
		"no wrappings":  {Version: keyfileVersion},
		"future format": {Version: keyfileVersion + 1, WrappedByPassphrase: []byte("x")},
	} {
		t.Run(name, func(t *testing.T) {
			if data, err := MarshalKeyfile(kf); err == nil {
				t.Errorf("serialized an unusable envelope: %s", data)
			}
		})
	}
}

func TestKeyfileRejectsWeakenedKDFParameters(t *testing.T) {
	// The keyfile sits in storage anyone holding the bucket can rewrite. An
	// attacker who could set the cost to nothing would turn an offline attack
	// on the passphrase from expensive into instant.
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	for name, weaken := range map[string]func(*KDFParams){
		"no memory cost": func(p *KDFParams) { p.MemoryKiB = 8 },
		"no time cost":   func(p *KDFParams) { p.Time = 0 },
		"short salt":     func(p *KDFParams) { p.Salt = []byte("short") },
		"other kdf":      func(p *KDFParams) { p.Name = "md5" },
		"no threads":     func(p *KDFParams) { p.Threads = 0 },

		// The other direction. argon2.IDKey answers these by allocating the
		// memory and doing the work, so an unbounded value is an OOM or a hang
		// at unlock - a denial of the user's own data, from a file anyone
		// holding the bucket can rewrite.
		"absurd memory":  func(p *KDFParams) { p.MemoryKiB = 4294967295 },
		"absurd time":    func(p *KDFParams) { p.Time = 4294967295 },
		"absurd threads": func(p *KDFParams) { p.Threads = 255 },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := *kf
			tampered.KDF = kf.KDF
			weaken(&tampered.KDF)

			if _, err := tampered.UnlockWithPassphrase(testPassphrase); err == nil {
				t.Error("weakened parameters were accepted")
			}
		})
	}
}

// TestASwappedPublicKeyIsRefused is the attack the asymmetric design opens up
// and has to close. Whoever holds the bucket cannot read anything - but if they
// replaced the advertised public key, the next device to join would pin theirs,
// and every session pushed afterwards would be readable by them and unreadable
// by its owner. Unlocking therefore checks the advertised key against the
// secret it wraps, so a pin is only ever taken from a value the passphrase
// vouches for (spec §3.4).
func TestASwappedPublicKeyIsRefused(t *testing.T) {
	kf, recovery, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	attacker := NewDataKey()
	defer attacker.Close()
	theirs, err := attacker.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	kf.IdentityPublic = theirs.Bytes()

	if _, err := kf.UnlockWithPassphrase(testPassphrase); !errors.Is(err, ErrPublicKeyMismatch) {
		t.Errorf("got %v, want ErrPublicKeyMismatch", err)
	}
	if _, err := kf.UnlockWithRecoveryKey(recovery); !errors.Is(err, ErrPublicKeyMismatch) {
		t.Errorf("got %v, want ErrPublicKeyMismatch", err)
	}

	// The message has to tell the user not to push, because pushing is what
	// would hand their sessions over.
	_, err = kf.UnlockWithPassphrase(testPassphrase)
	if !strings.Contains(err.Error(), "do not push") {
		t.Errorf("the message does not warn against pushing: %v", err)
	}
}

func TestAKeyfileAdvertisingNoPublicKeyIsRefused(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	kf.IdentityPublic = nil

	if _, err := kf.UnlockWithPassphrase(testPassphrase); !errors.Is(err, ErrPublicKeyMismatch) {
		t.Errorf("got %v, want ErrPublicKeyMismatch", err)
	}
}

func TestIdentityPublicKeyParsesWhatItAdvertises(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}

	public, err := kf.IdentityPublicKey()
	if err != nil {
		t.Fatalf("IdentityPublicKey: %v", err)
	}

	// It must be usable for exactly one thing: encrypting to this storage.
	sealed, err := Encrypt(public, "v1/a", []byte("pushed with a pinned key"))
	if err != nil {
		t.Fatal(err)
	}
	dataKey, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(private, "v1/a", sealed)
	if err != nil {
		t.Fatalf("what the pinned key sealed could not be opened: %v", err)
	}
	if string(got) != "pushed with a pinned key" {
		t.Errorf("got %q", got)
	}

	// And rubbish in that field is reported as such rather than panicking.
	kf.IdentityPublic = []byte("not a curve point")
	if _, err := kf.IdentityPublicKey(); err == nil {
		t.Error("an unusable public key was accepted")
	}
}

// TestWrappedKeysAreNotObjects. The two formats live in one bucket, and a wrap
// presented as an object - or the reverse - must fail rather than be attempted.
func TestWrappedKeysAreNotObjects(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	dataKey, err := kf.UnlockWithPassphrase(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Decrypt(private, wrapPathPassphrase, kf.WrappedByPassphrase); err == nil {
		t.Error("a wrapped key was read as an object")
	}

	sealed, err := Encrypt(private.PublicKey(), "v1/a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrap(make([]byte, keyLen), "v1/a", sealed); err == nil {
		t.Error("an object was read as a wrapped key")
	}
}

// TestACorruptWrappingIsReportedAsCorruption, not as a wrong passphrase. A
// truncated or garbled keyfile is a real thing to find in storage - a half-
// finished upload, a sync tool's conflict copy - and telling the user their
// passphrase is wrong sends them to re-run init, which orphans everything they
// have already pushed.
func TestACorruptWrappingIsReportedAsCorruption(t *testing.T) {
	kf, _, err := NewKeyfile(testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	intact := bytes.Clone(kf.WrappedByPassphrase)

	for name, damage := range map[string]func([]byte) []byte{
		"truncated to nothing":    func(b []byte) []byte { return b[:0] },
		"truncated mid-header":    func(b []byte) []byte { return b[:3] },
		"truncated before nonce":  func(b []byte) []byte { return b[:wrapHeaderLen-1] },
		"unknown wrapping format": func(b []byte) []byte { c := bytes.Clone(b); c[len(wrapMagic)] = 0; return c },
	} {
		t.Run(name, func(t *testing.T) {
			kf.WrappedByPassphrase = damage(intact)

			_, err := kf.UnlockWithPassphrase(testPassphrase)
			if err == nil {
				t.Fatal("a damaged wrapping unlocked")
			}
			if errors.Is(err, ErrWrongPassphrase) {
				t.Errorf("damage was reported as a wrong passphrase: %v", err)
			}
		})
	}
}

func TestIdentityPublicKeyRefusesAKeyfileItCannotUse(t *testing.T) {
	// Every other method on *Keyfile survives a nil receiver; a caller on a
	// "no keyfile yet" path must not be the one to find out this one does not.
	var kf *Keyfile
	if _, err := kf.IdentityPublicKey(); err == nil {
		t.Error("a nil keyfile advertised a public key")
	}
}

func TestSealLocalRoundTrip(t *testing.T) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	const secret = "AKIAIOSFODNN7EXAMPLE/wJalrXUtnFEMI"

	sealed, err := SealLocal(key, "ctxhop/secrets", []byte(secret))
	if err != nil {
		t.Fatalf("SealLocal: %v", err)
	}
	if bytes.Contains(sealed, []byte("AKIA")) {
		t.Fatal("the credential survived into the sealed bytes")
	}

	got, err := OpenLocal(key, "ctxhop/secrets", sealed)
	if err != nil {
		t.Fatalf("OpenLocal: %v", err)
	}
	if string(got) != secret {
		t.Errorf("got %q", got)
	}

	// The label is authenticated, so a file cannot be presented as another.
	if _, err := OpenLocal(key, "ctxhop/something-else", sealed); err == nil {
		t.Error("sealed data opened under a different label")
	}
	// And a different device key cannot open it.
	other := make([]byte, keyLen)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocal(other, "ctxhop/secrets", sealed); err == nil {
		t.Error("sealed data opened under a different key")
	}
}
