// Package crypto turns the bytes AgentSync stores into ciphertext nothing
// outside the user's machines can read.
//
// Its failure mode is the opposite of the adapter's. There, a wrong guess
// corrupts an agent's data silently. Here, mistakes are loud: authentication
// simply fails. The one quiet failure is believing something was encrypted when
// it was not, which is why the tests assert directly that no recognisable
// plaintext survives.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// The wire format of an encrypted object:
//
//	magic(4) | version(1) | nonce(12) | ciphertext+tag
const (
	objectMagic   = "ASx1"
	objectVersion = 1

	keyLen    = 32
	nonceLen  = 12
	headerLen = len(objectMagic) + 1 + nonceLen
)

// ErrCorrupt reports ciphertext that failed authentication, was truncated, or
// was presented at the wrong location.
var ErrCorrupt = errors.New("crypto: object failed authentication")

// ErrUnsupportedVersion reports an object written by a newer release.
var ErrUnsupportedVersion = errors.New("crypto: object format is newer than this version understands")

// Encrypt seals plaintext for storage at path.
//
// path is both an input to the object's key and the additional authenticated
// data. Binding it means a shard only decrypts at the location it was written
// to: without that, an attacker - or a sync bug - could move one session's
// shard into another session and it would decrypt perfectly (spec §7.1).
func Encrypt(contentKey []byte, path string, plaintext []byte) ([]byte, error) {
	aead, err := objectAEAD(contentKey, path)
	if err != nil {
		return nil, err
	}

	// Random rather than deterministic, even though shards are immutable.
	// Metadata objects are rewritten, and a repeated nonce under one key is
	// catastrophic for GCM rather than merely weak. Per-object keys make an
	// accidental collision across objects harmless (spec §7.2).
	// crypto/rand.Read never returns an error; it crashes the program if the
	// system source fails, so there is no branch here to get wrong.
	nonce := make([]byte, nonceLen)
	rand.Read(nonce)

	out := make([]byte, 0, headerLen+len(plaintext)+aead.Overhead())
	out = append(out, objectMagic...)
	out = append(out, objectVersion)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(path)), nil
}

// Decrypt opens an object stored at path.
//
// Any modification to the ciphertext, the tag, the nonce, the header, or the
// path makes this fail. It never returns partial plaintext: a caller that acted
// on half a record would be acting on something nobody authenticated.
func Decrypt(contentKey []byte, path string, sealed []byte) ([]byte, error) {
	if len(sealed) < headerLen {
		return nil, fmt.Errorf("%w: too short to be an object", ErrCorrupt)
	}
	if string(sealed[:len(objectMagic)]) != objectMagic {
		return nil, fmt.Errorf("%w: not an AgentSync object", ErrCorrupt)
	}
	if v := sealed[len(objectMagic)]; v != objectVersion {
		// Refuse rather than guess. A newer format may mean anything.
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedVersion, v)
	}

	aead, err := objectAEAD(contentKey, path)
	if err != nil {
		return nil, err
	}

	nonce := sealed[len(objectMagic)+1 : headerLen]
	plaintext, err := aead.Open(nil, nonce, sealed[headerLen:], []byte(path))
	if err != nil {
		// The underlying error says nothing useful and could differ between
		// implementations; what matters is that it did not authenticate.
		return nil, ErrCorrupt
	}
	return plaintext, nil
}

// objectAEAD derives this object's own key and returns a cipher for it.
//
// A key per object means no two objects share a key, so a nonce repeated by
// chance between two objects is harmless.
func objectAEAD(contentKey []byte, path string) (cipher.AEAD, error) {
	if len(contentKey) != keyLen {
		return nil, fmt.Errorf("crypto: content key must be %d bytes", keyLen)
	}
	if path == "" {
		// An empty path would give every such object the same key and no
		// binding to a location.
		return nil, errors.New("crypto: object path is required")
	}

	objectKey, err := hkdf.Expand(sha256.New, contentKey, "agentsync/object\x00"+path, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive object key: %w", err)
	}
	defer zero(objectKey)

	block, err := aes.NewCipher(objectKey)
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
