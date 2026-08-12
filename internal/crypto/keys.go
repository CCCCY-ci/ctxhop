package crypto

import (
	"crypto/ecdh"
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
	// crypto/rand.Read never returns an error; it crashes the program if the
	// system source fails, so there is no branch here to get wrong.
	rand.Read(salt)
	return KDFParams{
		Name:      "argon2id",
		Salt:      salt,
		Time:      3,
		MemoryKiB: 64 * 1024,
		Threads:   4,
	}
}

// Ceilings on the stored cost. They are far above any setting a future release
// would plausibly choose (the current defaults are 3 and 64 MiB), so raising the
// defaults never trips them.
//
// They exist because argon2.IDKey answers absurd parameters by allocating the
// memory and doing the work: a rewritten memoryKiB is an unrecoverable
// allocation failure, and a rewritten time cost is an unbounded loop with no
// context to cancel it (code_style §4.2, §4.3).
const (
	maxKDFMemoryKiB = 1 << 20 // 1 GiB
	maxKDFTime      = 16
	maxKDFThreads   = 16
)

// validate rejects parameters this build will not derive a key from.
//
// These arrive from a file that anyone holding the bucket can rewrite, so they
// are bounded in both directions. Too cheap turns an offline attack on the
// passphrase from expensive into instant; too expensive turns unlocking into a
// crash or a hang, which is a denial of the user's own data.
func (p KDFParams) validate() error {
	switch {
	case p.Name != "argon2id":
		return fmt.Errorf("crypto: unsupported key derivation %q", p.Name)
	case len(p.Salt) < 16:
		return fmt.Errorf("crypto: salt is %d bytes, refusing anything under 16", len(p.Salt))
	case p.Time < 1:
		return fmt.Errorf("crypto: time cost %d is too low", p.Time)
	case p.Time > maxKDFTime:
		return fmt.Errorf("crypto: time cost %d exceeds the %d this build will run; the keyfile may have been tampered with", p.Time, maxKDFTime)
	case p.MemoryKiB < 16*1024:
		return fmt.Errorf("crypto: memory cost %d KiB is too low", p.MemoryKiB)
	case p.MemoryKiB > maxKDFMemoryKiB:
		return fmt.Errorf("crypto: memory cost %d KiB exceeds the %d KiB this build will allocate; the keyfile may have been tampered with", p.MemoryKiB, maxKDFMemoryKiB)
	case p.Threads < 1:
		return fmt.Errorf("crypto: thread count %d is invalid", p.Threads)
	case p.Threads > maxKDFThreads:
		return fmt.Errorf("crypto: thread count %d exceeds the %d this build will run; the keyfile may have been tampered with", p.Threads, maxKDFThreads)
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
	// See DefaultKDFParams: rand.Read cannot fail without crashing.
	rand.Read(raw)
	return &DataKey{raw: raw}
}

// IdentityPrivate derives the X25519 key objects are encrypted to.
//
// Deriving it from the data key rather than generating it separately is what
// keeps the envelope unchanged: the passphrase and the recovery key still wrap
// one random secret, and everything else hangs off it (spec §3.1).
func (d *DataKey) IdentityPrivate() (*ecdh.PrivateKey, error) {
	seed, err := d.derive("agentsync/identity-x25519")
	if err != nil {
		return nil, err
	}
	defer zero(seed)

	key, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("derive identity key: %w", err)
	}
	return key, nil
}

// IdentityPublic derives the public half, which is all a push needs.
//
// This is the point of the whole hierarchy. A machine that only pushes holds no
// secret capable of reading anything: it encrypts to this value and cannot
// reverse it, so a stolen laptop yields the ability to append and nothing else
// (spec §3.3).
func (d *DataKey) IdentityPublic() (*ecdh.PublicKey, error) {
	private, err := d.IdentityPrivate()
	if err != nil {
		return nil, err
	}
	return private.PublicKey(), nil
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
		return nil, fmt.Errorf("crypto: data key is not initialised, or has been closed")
	}
	key, err := hkdf.Expand(sha256.New, d.raw, info, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive %s: %w", info, err)
	}
	return key, nil
}

// Close zeroes the key material and puts the key beyond use. Best effort, as
// ever in a garbage-collected language, but it shortens the window in which a
// heap dump contains the key.
//
// Dropping the slice matters as much as zeroing it. Zeroing alone leaves a
// key of the right length holding thirty-two zero bytes, which derive would
// accept - and every closed key would then derive the same content key, one
// anybody can compute offline. A use-after-close would encrypt the user's
// sessions under a public constant and report success (spec §13.4).
func (d *DataKey) Close() {
	if d != nil {
		zero(d.raw)
		d.raw = nil
	}
}
