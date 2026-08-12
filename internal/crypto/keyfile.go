package crypto

import (
	"bytes"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
)

// keyfileVersion is the format of the stored envelope.
const keyfileVersion = 1

// keyfilePath is where the envelope lives. It is fixed and unencrypted: it
// holds no secret, only the salt and two wrapped copies of the data key.
const keyfilePath = "v1/keyfile"

// ErrWrongPassphrase reports that the passphrase did not open the envelope.
var ErrWrongPassphrase = errors.New("crypto: passphrase does not unlock this storage")

// ErrWrongRecoveryKey reports that the recovery key did not open the envelope.
var ErrWrongRecoveryKey = errors.New("crypto: recovery key does not unlock this storage")

// Keyfile is the envelope holding the data key.
//
// Both wrappings protect the same key, so a new device needs only one of them
// and nothing else - in particular, no existing device has to be online. That
// is what makes "continue on another machine while the first one is off" work
// at all (spec §3.2).
type Keyfile struct {
	Version int       `json:"version"`
	KDF     KDFParams `json:"kdf"`
	// IdentityPublic is stored in the clear because it is public. It is how a
	// second device learns which key to encrypt to, and unlocking verifies that
	// it really is the public half of the wrapped secret (spec §3.4).
	IdentityPublic       []byte `json:"identityPublic"`
	WrappedByPassphrase  []byte `json:"wrappedByPassphrase"`
	WrappedByRecoveryKey []byte `json:"wrappedByRecoveryKey"`
}

// KeyfilePath is the object key the envelope is stored under.
func KeyfilePath() string { return keyfilePath }

// NewKeyfile generates a data key and wraps it under both a passphrase and a
// freshly generated recovery key.
//
// The recovery key is returned in written form because this is the only moment
// it exists in a shape a person can keep. Nothing stores it, and it cannot be
// recovered afterwards.
func NewKeyfile(passphrase string) (*Keyfile, string, error) {
	params := DefaultKDFParams()

	dataKey := NewDataKey()
	defer dataKey.Close()

	recoveryRaw, recoveryText := NewRecoveryKey()
	defer zero(recoveryRaw)

	public, err := dataKey.IdentityPublic()
	if err != nil {
		return nil, "", err
	}

	kf := &Keyfile{Version: keyfileVersion, KDF: params, IdentityPublic: public.Bytes()}
	if err := kf.wrapWithPassphrase(dataKey, passphrase); err != nil {
		return nil, "", err
	}
	if err := kf.wrapWithRecoveryKey(dataKey, recoveryRaw); err != nil {
		return nil, "", err
	}
	return kf, recoveryText, nil
}

// UnlockWithPassphrase opens the envelope.
func (k *Keyfile) UnlockWithPassphrase(passphrase string) (*DataKey, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	if len(k.WrappedByPassphrase) == 0 {
		// Otherwise this spends a full derivation and then reports the
		// passphrase as wrong, so the user retypes one that can never work.
		return nil, errors.New("crypto: this storage has no passphrase wrapping; unlock with your recovery key")
	}

	kek, err := passphraseKEK(passphrase, k.KDF)
	if err != nil {
		return nil, err
	}
	defer zero(kek)

	raw, err := unwrap(kek, wrapPathPassphrase, k.WrappedByPassphrase)
	if err != nil {
		return nil, translateUnwrapError(err, ErrWrongPassphrase)
	}
	return k.verified(&DataKey{raw: raw})
}

// translateUnwrapError decides what a failed unwrap means to the user.
//
// Only an authentication failure is reported as a wrong secret, because only
// that one is genuinely ambiguous: under an AEAD, "you typed the wrong
// passphrase" and "somebody altered this" are indistinguishable. Everything
// else - a truncated wrapping, a format that is not ours, a version from the
// future - was decided before any key was involved and is a fact about the
// bytes.
//
// The distinction decides where the user goes next. Told the passphrase is
// wrong, they retype it, then re-run init over the keyfile - and that orphans
// every session they have already pushed (spec §13.9).
func translateUnwrapError(err error, wrongSecret error) error {
	if errors.Is(err, errWrongKEK) {
		return wrongSecret
	}
	return err
}

// UnlockWithRecoveryKey opens the envelope with the written recovery key.
func (k *Keyfile) UnlockWithRecoveryKey(recoveryText string) (*DataKey, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	if len(k.WrappedByRecoveryKey) == 0 {
		return nil, errors.New("crypto: this storage has no recovery-key wrapping; unlock with your passphrase")
	}

	// A mistyped key is reported as a mistyped key, before anything is tried
	// against the envelope.
	recoveryRaw, err := ParseRecoveryKey(recoveryText)
	if err != nil {
		return nil, err
	}
	defer zero(recoveryRaw)

	kek, err := recoveryKEK(recoveryRaw)
	if err != nil {
		return nil, err
	}
	defer zero(kek)

	raw, err := unwrap(kek, wrapPathRecovery, k.WrappedByRecoveryKey)
	if err != nil {
		return nil, translateUnwrapError(err, ErrWrongRecoveryKey)
	}
	return k.verified(&DataKey{raw: raw})
}

// ErrPublicKeyMismatch reports a keyfile whose public key is not the public
// half of the secret it wraps.
var ErrPublicKeyMismatch = errors.New("crypto: the storage keyfile advertises a public key that does not belong to it; do not push to this storage")

// verified checks the advertised public key against the unwrapped secret.
//
// This is the moment a device decides which key to encrypt everything to, and
// the check is what makes that decision safe. Someone able to rewrite the
// keyfile cannot read anything - but if they replaced the public key, a device
// that pinned it would encrypt every future session to *their* key and lose the
// ability to read its own pushes. Verifying here means the pin is only ever
// taken from a value the passphrase itself vouches for (spec §3.4).
func (k *Keyfile) verified(dataKey *DataKey) (*DataKey, error) {
	if len(k.IdentityPublic) == 0 {
		// Written by a build that predates the asymmetric format. Nothing
		// shipped, so this is a corrupt file rather than an old one.
		dataKey.Close()
		return nil, fmt.Errorf("%w: it advertises no public key", ErrPublicKeyMismatch)
	}

	public, err := dataKey.IdentityPublic()
	if err != nil {
		dataKey.Close()
		return nil, err
	}
	if !bytes.Equal(public.Bytes(), k.IdentityPublic) {
		dataKey.Close()
		return nil, ErrPublicKeyMismatch
	}
	return dataKey, nil
}

// IdentityPublicKey parses the advertised public key, for a caller pinning it
// after a successful unlock.
func (k *Keyfile) IdentityPublicKey() (*ecdh.PublicKey, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	public, err := ecdh.X25519().NewPublicKey(k.IdentityPublic)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPublicKeyMismatch, err)
	}
	return public, nil
}

// ChangePassphrase re-wraps the data key under a new passphrase.
//
// The data key itself does not change, so every byte already uploaded stays
// readable. Re-deriving the content key from the passphrase instead would have
// meant re-encrypting the user's entire history to change a password.
func (k *Keyfile) ChangePassphrase(current, next string) error {
	dataKey, err := k.UnlockWithPassphrase(current)
	if err != nil {
		return err
	}
	defer dataKey.Close()

	// A new salt too: reusing the old one would let anyone holding both
	// versions of the file confirm a guess against either.
	params := DefaultKDFParams()
	return k.rewrap(dataKey, next, params)
}

// ResetPassphrase sets a new passphrase using the recovery key, for the user
// who has forgotten the old one.
func (k *Keyfile) ResetPassphrase(recoveryText, next string) error {
	dataKey, err := k.UnlockWithRecoveryKey(recoveryText)
	if err != nil {
		return err
	}
	defer dataKey.Close()

	params := DefaultKDFParams()
	return k.rewrap(dataKey, next, params)
}

// rewrap replaces the passphrase wrapping and its parameters together, or
// leaves both exactly as they were.
//
// Assigning the new parameters before the wrapping succeeds would leave a
// keyfile whose salt no longer matches its wrapped key: the old passphrase
// would stop working even though the change was refused, locking the user out
// of everything but their recovery key.
func (k *Keyfile) rewrap(dataKey *DataKey, passphrase string, params KDFParams) error {
	next := &Keyfile{Version: k.Version, KDF: params}
	if err := next.wrapWithPassphrase(dataKey, passphrase); err != nil {
		return err
	}

	k.KDF = params
	k.WrappedByPassphrase = next.WrappedByPassphrase
	return nil
}

// UpgradeKDF re-wraps under stronger settings if the stored ones are weaker
// than today's defaults.
//
// Reports whether anything changed, so a caller only writes the file back when
// it must. Existing ciphertext is untouched either way.
func (k *Keyfile) UpgradeKDF(passphrase string) (bool, error) {
	// Before touching k.KDF: every other method on *Keyfile survives a nil
	// receiver through check, and a caller on a "no keyfile yet" path should
	// get the same error here rather than a panic.
	if err := k.check(); err != nil {
		return false, err
	}

	defaults := DefaultKDFParams()
	if k.KDF.Time >= defaults.Time && k.KDF.MemoryKiB >= defaults.MemoryKiB {
		return false, nil
	}

	dataKey, err := k.UnlockWithPassphrase(passphrase)
	if err != nil {
		return false, err
	}
	defer dataKey.Close()

	if err := k.rewrap(dataKey, passphrase, defaults); err != nil {
		return false, err
	}
	return true, nil
}

// Distinct paths so the two wrappings cannot be swapped for one another: the
// path is authenticated data, so a recovery-key envelope presented as a
// passphrase envelope fails rather than being tried.
const (
	wrapPathPassphrase = "keyfile/passphrase"
	wrapPathRecovery   = "keyfile/recovery"
)

func (k *Keyfile) wrapWithPassphrase(dataKey *DataKey, passphrase string) error {
	kek, err := passphraseKEK(passphrase, k.KDF)
	if err != nil {
		return err
	}
	defer zero(kek)

	sealed, err := wrap(kek, wrapPathPassphrase, dataKey.raw)
	if err != nil {
		return fmt.Errorf("wrap data key: %w", err)
	}
	k.WrappedByPassphrase = sealed
	return nil
}

func (k *Keyfile) wrapWithRecoveryKey(dataKey *DataKey, recoveryRaw []byte) error {
	kek, err := recoveryKEK(recoveryRaw)
	if err != nil {
		return err
	}
	defer zero(kek)

	sealed, err := wrap(kek, wrapPathRecovery, dataKey.raw)
	if err != nil {
		return fmt.Errorf("wrap data key: %w", err)
	}
	k.WrappedByRecoveryKey = sealed
	return nil
}

// check rejects an envelope this build cannot safely use.
func (k *Keyfile) check() error {
	if k == nil {
		return errors.New("crypto: no keyfile")
	}
	if k.Version > keyfileVersion {
		return fmt.Errorf("%w: keyfile version %d", ErrUnsupportedVersion, k.Version)
	}
	if len(k.WrappedByPassphrase) == 0 && len(k.WrappedByRecoveryKey) == 0 {
		return errors.New("crypto: keyfile holds no wrapped key")
	}
	return nil
}

// MarshalKeyfile renders the envelope for storage.
//
// It refuses an envelope holding no wrapped key. This object is the one whose
// loss locks the user out of every session they have ever pushed, and the
// caller's next act is to write the result over the existing one, so a caller
// bug here is unrecoverable data loss. Refusing to write is always safe
// (BR-12).
func MarshalKeyfile(k *Keyfile) ([]byte, error) {
	if err := k.check(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode keyfile: %w", err)
	}
	return append(data, '\n'), nil
}

// ParseKeyfile reads an envelope from storage.
func ParseKeyfile(data []byte) (*Keyfile, error) {
	var k Keyfile
	if err := json.Unmarshal(data, &k); err != nil {
		return nil, fmt.Errorf("keyfile is not valid JSON; storage may be corrupt or not an AgentSync store: %w", err)
	}
	if err := k.check(); err != nil {
		return nil, err
	}
	return &k, nil
}
