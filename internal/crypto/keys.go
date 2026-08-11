package crypto

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// KDFParams are the Argon2id settings used to turn a passphrase into a
// key-encrypting key.
//
// They are stored alongside the wrapped key rather than compiled in, so they
// can be raised later without invalidating anything: unlock with the old
// settings, re-wrap with the new ones (spec §4.1, §9).
type KDFParams struct {
	Name      string `json:"name"`
	Salt      []byte `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memoryKiB"`
	Threads   uint8  `json:"threads"`
}

// DefaultKDFParams are tuned so unlocking is imperceptible while remaining
// expensive to attack: about 47ms on a current laptop.
func DefaultKDFParams() KDFParams {
	salt := make([]byte, 16)
	rand.Read(salt)
	return KDFParams{
		Name:      "argon2id",
		Salt:      salt,
		Time:      3,
		MemoryKiB: 64 * 1024,
		Threads:   4,
	}
}

// validate rejects parameters that would silently weaken the derivation.
//
// These arrive from a file that anyone holding the bucket can rewrite. An
// attacker who could set the cost to nothing would turn an offline attack on
// the passphrase from expensive into instant.
func (p KDFParams) validate() error {
	switch {
	case p.Name != "argon2id":
		return fmt.Errorf("crypto: unsupported key derivation %q", p.Name)
	case len(p.Salt) < 16:
		return fmt.Errorf("crypto: salt is %d bytes, refusing anything under 16", len(p.Salt))
	case p.Time < 1:
		return fmt.Errorf("crypto: time cost %d is too low", p.Time)
	case p.MemoryKiB < 16*1024:
		return fmt.Errorf("crypto: memory cost %d KiB is too low", p.MemoryKiB)
	case p.Threads < 1:
		return fmt.Errorf("crypto: thread count %d is invalid", p.Threads)
	}
	return nil
}

// passphraseKEK derives the key that wraps the data key.
//
// Argon2id rather than PBKDF2: the passphrase is the only thing between someone
// who obtains the ciphertext and every session the user has ever had, and
// PBKDF2 is orders of magnitude cheaper to attack on a GPU (spec §4).
func passphraseKEK(passphrase string, p KDFParams) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if passphrase == "" {
		return nil, fmt.Errorf("crypto: passphrase is required")
	}
	return argon2.IDKey([]byte(passphrase), p.Salt, p.Time, p.MemoryKiB, p.Threads, keyLen), nil
}

// recoveryKEK derives the key that wraps the data key from a recovery key.
//
// No memory hardening here: a recovery key is already a full-entropy random
// value, so there is no dictionary to attack. Slowing it down would only slow
// down recovery (spec §5.3).
func recoveryKEK(recovery []byte) ([]byte, error) {
	if len(recovery) != recoveryKeyLen {
		return nil, fmt.Errorf("crypto: recovery key must be %d bytes", recoveryKeyLen)
	}
	return hkdf.Expand(sha256.New, recovery, "agentsync/recovery-kek", keyLen)
}

// DataKey is the key everything else hangs off.
//
// It is generated once, at random, and never changes. The passphrase and the
// recovery key each wrap it separately, which is what makes changing a
// passphrase a re-wrap rather than a re-encryption of everything ever uploaded
// (spec §3.1).
type DataKey struct {
	raw []byte
}

// NewDataKey generates a fresh data key.
func NewDataKey() *DataKey {
	raw := make([]byte, keyLen)
	rand.Read(raw)
	return &DataKey{raw: raw}
}

// ContentKey derives the key objects are encrypted under.
func (d *DataKey) ContentKey() ([]byte, error) {
	return d.derive("agentsync/content")
}

// IdentifierKey derives the key project, session and device identifiers are
// computed with.
//
// Separate from the content key so that a compromise of one does not hand over
// the other, and so that neither purpose can be confused for the other.
func (d *DataKey) IdentifierKey() ([]byte, error) {
	return d.derive("agentsync/identifier")
}

func (d *DataKey) derive(info string) ([]byte, error) {
	if d == nil || len(d.raw) != keyLen {
		return nil, fmt.Errorf("crypto: data key is not initialised")
	}
	key, err := hkdf.Expand(sha256.New, d.raw, info, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive %s: %w", info, err)
	}
	return key, nil
}

// Close zeroes the key material. Best effort, as ever in a garbage-collected
// language, but it shortens the window in which a heap dump contains the key.
func (d *DataKey) Close() {
	if d != nil {
		zero(d.raw)
	}
}
