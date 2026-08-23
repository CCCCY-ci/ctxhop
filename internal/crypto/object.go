// Package crypto turns the bytes CtxHop stores into ciphertext nothing
// outside the user's machines can read.
//
// Its failure mode is the opposite of the adapter's. There, a wrong guess
// corrupts an agent's data silently. Here, mistakes are loud: authentication
// simply fails. The one quiet failure is believing something was encrypted when
// it was not, which is why the tests assert directly that no recognisable
// plaintext survives.
//
// Encryption is asymmetric because pushing and pulling are asymmetric acts. A
// push runs from the agent's SessionEnd hook, with nobody there to type a
// passphrase, so it must work from what is on disk alone. A pull is a person
// deciding to continue a session on this machine, so it can ask. Encrypting to
// a public key means the unattended half needs no secret at all: a stolen
// laptop can append to the user's storage and cannot read a word of it
// (spec §3.3).
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// The wire format of an encrypted object:
//
//	magic(4) | version(1) | ephemeral public key(32) | nonce(12) | ciphertext+tag
const (
	objectMagic   = "ASx1"
	objectVersion = 1

	keyLen       = 32
	nonceLen     = 12
	publicKeyLen = 32
	headerLen    = len(objectMagic) + 1 + publicKeyLen + nonceLen
)

// ErrCorrupt reports ciphertext that failed authentication, was truncated, or
// was presented at the wrong location.
var ErrCorrupt = errors.New("crypto: object failed authentication")

// ErrUnsupportedVersion reports an object written by a newer release.
var ErrUnsupportedVersion = errors.New("crypto: object format is newer than this version understands")

// Encrypt seals plaintext for storage at path, readable only by the holder of
// the matching private key.
//
// path is bound into both the key derivation and the additional authenticated
// data. Binding it means a shard only decrypts at the location it was written
// to: without that, an attacker - or a sync bug - could move one session's
// shard into another session and it would decrypt perfectly (spec §7.1).
func Encrypt(recipient *ecdh.PublicKey, path string, plaintext []byte) ([]byte, error) {
	if recipient == nil {
		return nil, errors.New("crypto: no recipient key")
	}
	if path == "" {
		// An empty path would leave the object unbound to any location.
		return nil, errors.New("crypto: object path is required")
	}

	// A fresh ephemeral key per object, so every object has its own content key
	// and no two objects ever share one.
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ephemeral key: %w", err)
	}

	shared, err := exchange(ephemeral, recipient)
	if err != nil {
		return nil, err
	}
	defer zero(shared)

	cek, err := contentKey(shared, ephemeral.PublicKey(), recipient, path)
	if err != nil {
		return nil, err
	}
	defer zero(cek)

	aead, err := newAEAD(cek)
	if err != nil {
		return nil, err
	}

	// Random rather than derived, even though the content key is already unique
	// per object. It costs twelve bytes and it means a future change that
	// accidentally reused an ephemeral key would still not repeat a nonce under
	// one key, which for GCM is the difference between weaker and broken
	// (spec §7.2).
	//
	// crypto/rand.Read never returns an error; it crashes the program if the
	// system source fails, so there is no branch here to get wrong.
	nonce := make([]byte, nonceLen)
	rand.Read(nonce)

	out := make([]byte, 0, headerLen+len(plaintext)+aead.Overhead())
	out = append(out, objectMagic...)
	out = append(out, objectVersion)
	out = append(out, ephemeral.PublicKey().Bytes()...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(path)), nil
}

// Decrypt opens an object stored at path.
//
// Any modification to the ciphertext, the tag, the nonce, the ephemeral key,
// the header, or the path makes this fail. It never returns partial plaintext:
// a caller that acted on half a record would be acting on something nobody
// authenticated.
func Decrypt(identity *ecdh.PrivateKey, path string, sealed []byte) ([]byte, error) {
	if identity == nil {
		return nil, errors.New("crypto: no identity key")
	}
	if path == "" {
		return nil, errors.New("crypto: object path is required")
	}
	if len(sealed) < headerLen {
		return nil, fmt.Errorf("%w: too short to be an object", ErrCorrupt)
	}
	if string(sealed[:len(objectMagic)]) != objectMagic {
		return nil, fmt.Errorf("%w: not a CtxHop object", ErrCorrupt)
	}

	// Only a *higher* version is "too new to read"; the two directions call for
	// different remedies, and a caller that upgrades in response to a version 0
	// would be chasing the wrong thing. Splitting them here is also what leaves
	// room for a future version 2 that still reads version 1 (spec §9).
	switch v := sealed[len(objectMagic)]; {
	case v > objectVersion:
		return nil, fmt.Errorf("%w: version %d, upgrade ctxhop to read it", ErrUnsupportedVersion, v)
	case v != objectVersion:
		return nil, fmt.Errorf("%w: unknown object version %d", ErrCorrupt, v)
	}

	ephemeralStart := len(objectMagic) + 1
	ephemeral, err := ecdh.X25519().NewPublicKey(sealed[ephemeralStart : ephemeralStart+publicKeyLen])
	if err != nil {
		// A point that is not on the curve is corruption, not a key problem.
		return nil, fmt.Errorf("%w: unusable ephemeral key", ErrCorrupt)
	}

	shared, err := exchange(identity, ephemeral)
	if err != nil {
		return nil, err
	}
	defer zero(shared)

	cek, err := contentKey(shared, ephemeral, identity.PublicKey(), path)
	if err != nil {
		return nil, err
	}
	defer zero(cek)

	aead, err := newAEAD(cek)
	if err != nil {
		return nil, err
	}

	nonce := sealed[ephemeralStart+publicKeyLen : headerLen]
	plaintext, err := aead.Open(nil, nonce, sealed[headerLen:], []byte(path))
	if err != nil {
		// The underlying error says nothing useful and could differ between
		// implementations; what matters is that it did not authenticate.
		return nil, ErrCorrupt
	}
	return plaintext, nil
}

// contentKey derives one object's key from an X25519 shared secret.
//
// Both public keys go into the salt and the path into the info, so the key is
// bound to this ephemeral key, this recipient and this location. Leaving the
// recipient out would let one exchange be replayed towards a different
// recipient; leaving the path out would let a shard be moved between sessions.
//
// The two roles are passed in rather than inferred from which private key the
// caller happens to hold. Both sides must build byte-identical salt, and a
// function that guessed which key was the ephemeral one would be one refactor
// away from swapping them - at which point nothing decrypts and the reason why
// is invisible.
func contentKey(shared []byte, ephemeral, identity *ecdh.PublicKey, path string) ([]byte, error) {
	salt := make([]byte, 0, 2*publicKeyLen)
	salt = append(salt, ephemeral.Bytes()...)
	salt = append(salt, identity.Bytes()...)

	prk, err := hkdf.Extract(sha256.New, shared, salt)
	if err != nil {
		return nil, fmt.Errorf("derive object key: %w", err)
	}
	defer zero(prk)

	key, err := hkdf.Expand(sha256.New, prk, "ctxhop/object\x00"+path, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive object key: %w", err)
	}
	return key, nil
}

// exchange performs the X25519 agreement, reporting a failure as corruption:
// the only way it fails here is a peer key that is not a usable point.
func exchange(private *ecdh.PrivateKey, peer *ecdh.PublicKey) ([]byte, error) {
	shared, err := private.ECDH(peer)
	if err != nil {
		return nil, fmt.Errorf("%w: key exchange failed", ErrCorrupt)
	}
	return shared, nil
}

// newAEAD returns AES-256-GCM under one object's content key.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create aead: %w", err)
	}
	return aead, nil
}

// zero overwrites key material once it is no longer needed.
//
// Best effort only: the runtime may already have copied the value during a
// garbage collection, and Go offers no way to find those copies. It still
// shortens the window in which a heap dump or a swapped page contains the key.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
