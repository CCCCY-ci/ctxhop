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

// managedKeyfileVersion is the device-authorized envelope introduced after
// the original passphrase-only format. Keep keyfileVersion at 1 because the
// legacy migration path deliberately continues to create v1 files before a
// local device identity exists.
const managedKeyfileVersion = 2

// keyfilePath is where the envelope lives. It is fixed and unencrypted. v1
// stores public KDF metadata and two wrapped copies of the data key; managed v2
// also stores public membership/epoch metadata while its key bundle and grants
// remain encrypted.
const keyfilePath = "v1/keyfile"

// ErrWrongPassphrase reports that the passphrase did not open the envelope.
var ErrWrongPassphrase = errors.New("crypto: passphrase does not unlock this storage")

// ErrWrongRecoveryKey reports that the recovery key did not open the envelope.
var ErrWrongRecoveryKey = errors.New("crypto: recovery key does not unlock this storage")

// Keyfile is the envelope holding the content-key material. In v1 both
// wrappings protect the same data key. In managed v2 they protect a bundle of
// retained epoch keys and the stable identifier key; per-device grants provide
// the unattended authorization boundary. A new device still needs only one
// wrapping plus its own enrollment, and no existing device has to be online.
type Keyfile struct {
	Version int       `json:"version"`
	KDF     KDFParams `json:"kdf"`
	// IdentityPublic is stored in the clear because it is public. It is how a
	// second device learns which key to encrypt to, and unlocking verifies that
	// it really is the public half of the wrapped secret (spec §3.4).
	IdentityPublic       []byte `json:"identityPublic"`
	WrappedByPassphrase  []byte `json:"wrappedByPassphrase"`
	WrappedByRecoveryKey []byte `json:"wrappedByRecoveryKey"`
	// Generation and the fields below are present only in managed v2 files.
	// They are public metadata; epoch keys remain in encrypted wrappers.
	Generation uint64          `json:"generation,omitempty"`
	Members    []KeyfileMember `json:"members,omitempty"`
	Epochs     []KeyfileEpoch  `json:"epochs,omitempty"`
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
	raw, err := k.unlockPassphraseMaterial(passphrase)
	if err != nil {
		return nil, err
	}
	return k.dataKeyFromMaterial(raw)
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
	raw, err := k.unlockRecoveryMaterial(recoveryText)
	if err != nil {
		return nil, err
	}
	return k.dataKeyFromMaterial(raw)
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
	material, err := k.unlockPassphraseMaterial(current)
	if err != nil {
		return err
	}
	if k.IsManaged() {
		params := DefaultKDFParams()
		sealed, err := k.wrapMaterialWithPassphrase(material, next, params)
		zero(material)
		if err != nil {
			return err
		}
		k.KDF = params
		k.WrappedByPassphrase = sealed
		return nil
	}
	dataKey := &DataKey{raw: material}
	defer dataKey.Close()
	return k.rewrap(dataKey, next, DefaultKDFParams())
}

// ResetPassphrase sets a new passphrase using the recovery key, for the user
// who has forgotten the old one.
func (k *Keyfile) ResetPassphrase(recoveryText, next string) error {
	material, err := k.unlockRecoveryMaterial(recoveryText)
	if err != nil {
		return err
	}
	if k.IsManaged() {
		params := DefaultKDFParams()
		sealed, err := k.wrapMaterialWithPassphrase(material, next, params)
		zero(material)
		if err != nil {
			return err
		}
		k.KDF = params
		k.WrappedByPassphrase = sealed
		return nil
	}
	dataKey := &DataKey{raw: material}
	defer dataKey.Close()
	return k.rewrap(dataKey, next, DefaultKDFParams())
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
	if err := k.check(); err != nil {
		return false, err
	}
	defaults := DefaultKDFParams()
	if k.KDF.Time >= defaults.Time && k.KDF.MemoryKiB >= defaults.MemoryKiB {
		return false, nil
	}
	material, err := k.unlockPassphraseMaterial(passphrase)
	if err != nil {
		return false, err
	}
	if k.IsManaged() {
		sealed, err := k.wrapMaterialWithPassphrase(material, passphrase, defaults)
		zero(material)
		if err != nil {
			return false, err
		}
		k.KDF = defaults
		k.WrappedByPassphrase = sealed
		return true, nil
	}
	dataKey := &DataKey{raw: material}
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
	if k.Version > managedKeyfileVersion {
		return fmt.Errorf("%w: keyfile version %d", ErrUnsupportedVersion, k.Version)
	}
	if len(k.WrappedByPassphrase) == 0 && len(k.WrappedByRecoveryKey) == 0 {
		return errors.New("crypto: keyfile holds no wrapped key")
	}
	if k.Version == managedKeyfileVersion {
		if k.Generation == 0 || len(k.IdentityPublic) == 0 || len(k.Members) == 0 || len(k.Epochs) == 0 {
			return errors.New("crypto: managed keyfile has no active device authorization")
		}
		if len(k.Members) > maxManagedMembers || len(k.Epochs) > maxManagedEpochs {
			return errors.New("crypto: managed keyfile exceeds its member or epoch limit")
		}
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
		return nil, fmt.Errorf("keyfile is not valid JSON; storage may be corrupt or not a CtxHop store: %w", err)
	}
	if err := k.check(); err != nil {
		return nil, err
	}
	return &k, nil
}
