package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// The wire format of a wrapped key:
//
//	magic(4) | version(1) | nonce(12) | ciphertext+tag
//
// A magic distinct from an object's means a wrap can never be presented as an
// object, or an object as a wrap, however the two are shuffled in storage.
const (
	wrapMagic     = "ASk1"
	wrapVersion   = 1
	wrapHeaderLen = len(wrapMagic) + 1 + nonceLen
)

// wrap seals key material under a key-encrypting key.
//
// This is the one place symmetric encryption remains. Objects are encrypted to
// a public key so that an unattended push needs no secret, but the envelope
// itself is the opposite problem: it turns something the user knows - a
// passphrase, a recovery key - into the key that opens it, and there is no
// public half of a passphrase (spec §3.2).
//
// label is authenticated, so the passphrase wrapping and the recovery-key
// wrapping cannot be swapped for one another.
func wrap(kek []byte, label string, plaintext []byte) ([]byte, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}

	// See Encrypt: crypto/rand.Read cannot fail without crashing.
	nonce := make([]byte, nonceLen)
	rand.Read(nonce)

	out := make([]byte, 0, wrapHeaderLen+len(plaintext)+aead.Overhead())
	out = append(out, wrapMagic...)
	out = append(out, wrapVersion)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(label)), nil
}

// errWrongKEK reports that the sealed key did not authenticate under this
// key-encrypting key.
//
// It is deliberately separate from the structural failures below. Whether the
// secret was wrong or the ciphertext was tampered with is genuinely
// indistinguishable under an AEAD, so this one error covers both. Everything
// else in unwrap is decided before a key is involved at all, and is therefore a
// fact about the bytes rather than a guess about the user (spec §13.9).
var errWrongKEK = errors.New("crypto: wrapped key did not authenticate")

// ErrDamagedKeyfile reports a keyfile that is not a keyfile any more.
var ErrDamagedKeyfile = errors.New("crypto: the storage keyfile is damaged; existing sessions can only be reached by restoring it")

// unwrap opens key material sealed by wrap.
func unwrap(kek []byte, label string, sealed []byte) ([]byte, error) {
	if len(sealed) < wrapHeaderLen {
		return nil, fmt.Errorf("%w: too short to be a wrapped key", ErrDamagedKeyfile)
	}
	if string(sealed[:len(wrapMagic)]) != wrapMagic {
		return nil, fmt.Errorf("%w: it does not begin like one", ErrDamagedKeyfile)
	}

	// As for objects, only a higher version is "too new"; the two directions
	// call for different remedies (spec §9).
	switch v := sealed[len(wrapMagic)]; {
	case v > wrapVersion:
		return nil, fmt.Errorf("%w: keyfile version %d, upgrade agentsync to read it", ErrUnsupportedVersion, v)
	case v != wrapVersion:
		return nil, fmt.Errorf("%w: unknown wrapping version %d", ErrDamagedKeyfile, v)
	}

	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}

	nonce := sealed[len(wrapMagic)+1 : wrapHeaderLen]
	plaintext, err := aead.Open(nil, nonce, sealed[wrapHeaderLen:], []byte(label))
	if err != nil {
		return nil, errWrongKEK
	}
	return plaintext, nil
}
