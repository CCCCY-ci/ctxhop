package crypto

import (
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
	Version              int       `json:"version"`
	KDF                  KDFParams `json:"kdf"`
	WrappedByPassphrase  []byte    `json:"wrappedByPassphrase"`
	WrappedByRecoveryKey []byte    `json:"wrappedByRecoveryKey"`
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

	kf := &Keyfile{Version: keyfileVersion, KDF: params}
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

	kek, err := passphraseKEK(passphrase, k.KDF)
	if err != nil {
		return nil, err
	}
	defer zero(kek)

	raw, err := Decrypt(kek, wrapPathPassphrase, k.WrappedByPassphrase)
	if err != nil {
		// Deliberately not the underlying authentication error: the only thing
		// a user can act on is that this passphrase is not the right one.
		return nil, ErrWrongPassphrase
	}
	return &DataKey{raw: raw}, nil
}

// UnlockWithRecoveryKey opens the envelope with the written recovery key.
func (k *Keyfile) UnlockWithRecoveryKey(recoveryText string) (*DataKey, error) {
	if err := k.check(); err != nil {
		return nil, err
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

	raw, err := Decrypt(kek, wrapPathRecovery, k.WrappedByRecoveryKey)
	if err != nil {
		return nil, ErrWrongRecoveryKey
	}
	return &DataKey{raw: raw}, nil
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

	sealed, err := Encrypt(kek, wrapPathPassphrase, dataKey.raw)
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

	sealed, err := Encrypt(kek, wrapPathRecovery, dataKey.raw)
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
func MarshalKeyfile(k *Keyfile) ([]byte, error) {
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
