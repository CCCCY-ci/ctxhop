package crypto

import (
	"bytes"
	"crypto/ecdh"
	"errors"
	"strings"
	"testing"
)

// testIdentity returns a keypair as it is really derived, from a data key, so
// the tests exercise the same derivation the product uses.
func testIdentity(t testing.TB) (*ecdh.PrivateKey, *ecdh.PublicKey) {
	t.Helper()
	dk := NewDataKey()
	private, err := dk.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	return private, private.PublicKey()
}

func TestEncryptRoundTrip(t *testing.T) {
	private, public := testIdentity(t)

	for name, tc := range map[string]struct {
		path      string
		plaintext []byte
	}{
		"a shard":        {"v1/projects/p/sessions/s/dev/000001", []byte(`{"type":"user"}`)},
		"empty content":  {"v1/meta", []byte{}},
		"binary content": {"v1/meta", []byte{0x00, 0xff, 0xfe, 0x01}},
		"large content":  {"v1/big", bytes.Repeat([]byte("x"), 1<<20)},
		"unicode path":   {"v1/项目/会话", []byte("content")},
	} {
		t.Run(name, func(t *testing.T) {
			sealed, err := Encrypt(public, tc.path, tc.plaintext)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := Decrypt(private, tc.path, sealed)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if !bytes.Equal(got, tc.plaintext) {
				t.Errorf("got %q, want %q", got, tc.plaintext)
			}
		})
	}
}

// TestPushNeedsNoSecret is the reason the format is asymmetric. The unattended
// half of the product runs from the agent's SessionEnd hook with nobody to type
// a passphrase, so it must be able to encrypt from what is on disk - and what
// is on disk must not be able to read anything back (spec §3.3).
func TestPushNeedsNoSecret(t *testing.T) {
	private, public := testIdentity(t)
	const secret = "the session body nobody else may read"

	// Everything a pushing device has: the pinned public key, and no more.
	pinned, err := ecdh.X25519().NewPublicKey(public.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Encrypt(pinned, "v1/a", []byte(secret))
	if err != nil {
		t.Fatalf("a device holding only the public key could not encrypt: %v", err)
	}

	// That device cannot read its own push back: the only key it holds is
	// public, and the ciphertext keeps no copy of the plaintext.
	if bytes.Contains(sealed, []byte(secret)) {
		t.Error("the ciphertext contains the plaintext")
	}
	if bytes.Contains(sealed, []byte("session")) {
		t.Error("the ciphertext contains recognisable plaintext")
	}

	// Nor does the public key appear in a form that would let it stand in for
	// the private one - the header carries the *ephemeral* key, not the pinned
	// identity key.
	header := sealed[len(objectMagic)+1 : len(objectMagic)+1+publicKeyLen]
	if bytes.Equal(header, public.Bytes()) {
		t.Error("the object header carries the identity key instead of an ephemeral one")
	}

	// And the holder of the private key can.
	got, err := Decrypt(private, "v1/a", sealed)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != secret {
		t.Errorf("got %q", got)
	}
}

// TestAnotherIdentityCannotRead: a second user's storage, or an attacker who
// substituted their own key, must not be able to open these objects.
func TestAnotherIdentityCannotRead(t *testing.T) {
	_, public := testIdentity(t)
	stranger, _ := testIdentity(t)

	sealed, err := Encrypt(public, "v1/a", []byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(stranger, "v1/a", sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want ErrCorrupt", err)
	}
}

func TestEncryptLeavesNoPlaintext(t *testing.T) {
	_, public := testIdentity(t)
	const secret = "REMEMBER-THIS-DISTINCTIVE-STRING"

	sealed, err := Encrypt(public, "v1/a", []byte("prefix "+secret+" suffix"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(secret)) {
		t.Fatal("the plaintext survived into the ciphertext")
	}
	// Nor in any obvious transformation of it.
	if bytes.Contains(bytes.ToLower(sealed), []byte(strings.ToLower(secret))) {
		t.Error("the plaintext survived in another case")
	}
}

// TestEveryObjectGetsItsOwnKey. A fresh ephemeral key per object means no two
// objects share a content key, so a nonce repeated by chance between two
// objects is harmless (spec §7.2).
func TestEveryObjectGetsItsOwnKey(t *testing.T) {
	_, public := testIdentity(t)

	ephemerals := map[string]bool{}
	nonces := map[string]bool{}
	for i := 0; i < 50; i++ {
		sealed, err := Encrypt(public, "v1/same/path", []byte("same plaintext every time"))
		if err != nil {
			t.Fatal(err)
		}
		start := len(objectMagic) + 1
		ephemeral := string(sealed[start : start+publicKeyLen])
		nonce := string(sealed[start+publicKeyLen : headerLen])

		if ephemerals[ephemeral] {
			t.Fatal("an ephemeral key repeated")
		}
		if nonces[nonce] {
			t.Fatal("a nonce repeated")
		}
		ephemerals[ephemeral] = true
		nonces[nonce] = true
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	private, public := testIdentity(t)
	const path = "v1/a"

	sealed, err := Encrypt(public, path, []byte("content worth protecting"))
	if err != nil {
		t.Fatal(err)
	}

	// Every byte, including the magic, the version, the ephemeral key and the
	// nonce. Flipping any of them must fail rather than yield other plaintext.
	for i := range sealed {
		damaged := bytes.Clone(sealed)
		damaged[i] ^= 0x01

		got, err := Decrypt(private, path, damaged)
		if err == nil {
			t.Fatalf("a flipped bit at %d decrypted to %q", i, got)
		}
	}
}

func TestDecryptRejectsTruncation(t *testing.T) {
	private, public := testIdentity(t)
	sealed, err := Encrypt(public, "v1/a", []byte("some content worth keeping"))
	if err != nil {
		t.Fatal(err)
	}

	for cut := 0; cut < len(sealed); cut++ {
		if _, err := Decrypt(private, "v1/a", sealed[:cut]); err == nil {
			t.Fatalf("a %d-byte prefix decrypted", cut)
		}
	}
}

// TestDecryptRefusesTheWrongPath is what stops a shard being moved between
// sessions - by an attacker, or by a bug in the sync layer (spec §7.1).
func TestDecryptRefusesTheWrongPath(t *testing.T) {
	private, public := testIdentity(t)
	sealed, err := Encrypt(public, "v1/projects/a/sessions/s/dev/000001", []byte("shard"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = Decrypt(private, "v1/projects/b/sessions/s/dev/000001", sealed)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("a shard decrypted at another session's path: %v", err)
	}
}

func TestDecryptRefusesANewerVersion(t *testing.T) {
	private, public := testIdentity(t)
	sealed, err := Encrypt(public, "v1/a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(objectMagic)] = objectVersion + 1

	if _, err := Decrypt(private, "v1/a", sealed); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("got %v, want ErrUnsupportedVersion", err)
	}
}

// TestDecryptDoesNotCallAnOlderVersionNewer keeps the two directions apart. A
// user told to upgrade over a version byte of zero would be chasing a release
// that does not exist, and the format needs the room: a future version 2 has to
// be able to read version 1 (spec §9).
func TestDecryptDoesNotCallAnOlderVersionNewer(t *testing.T) {
	private, public := testIdentity(t)
	sealed, err := Encrypt(public, "v1/a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(objectMagic)] = objectVersion - 1

	_, err = Decrypt(private, "v1/a", sealed)
	if err == nil {
		t.Fatal("an unknown version decrypted")
	}
	if errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("an older version was reported as newer: %v", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want ErrCorrupt", err)
	}
}

func TestDecryptRejectsForeignData(t *testing.T) {
	private, _ := testIdentity(t)
	for _, junk := range [][]byte{
		nil,
		[]byte("not an object at all"),
		bytes.Repeat([]byte{0}, headerLen+16),
	} {
		if _, err := Decrypt(private, "v1/a", junk); err == nil {
			t.Errorf("%q decrypted", junk)
		}
	}
}

// TestAnUnusableEphemeralKeyIsCorruptionNotAKeyProblem. The header is
// attacker-controlled bytes; a point that is not on the curve says the object
// is damaged, not that the user's key is wrong.
func TestAnUnusableEphemeralKeyIsCorruptionNotAKeyProblem(t *testing.T) {
	private, public := testIdentity(t)
	sealed, err := Encrypt(public, "v1/a", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	start := len(objectMagic) + 1
	for i := start; i < start+publicKeyLen; i++ {
		sealed[i] = 0
	}

	if _, err := Decrypt(private, "v1/a", sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("got %v, want ErrCorrupt", err)
	}
}

func TestEncryptValidatesItsInputs(t *testing.T) {
	private, public := testIdentity(t)

	if _, err := Encrypt(public, "", []byte("x")); err == nil {
		t.Error("an object with no path was encrypted")
	}
	if _, err := Encrypt(nil, "v1/a", []byte("x")); err == nil {
		t.Error("an object was encrypted to no recipient")
	}
	if _, err := Decrypt(nil, "v1/a", []byte("x")); err == nil {
		t.Error("an object was decrypted with no key")
	}
	if _, err := Decrypt(private, "", []byte("x")); err == nil {
		t.Error("an object with no path was decrypted")
	}
}

func FuzzDecrypt(f *testing.F) {
	private, public := testIdentity(f)

	sealed, err := Encrypt(public, "v1/a", []byte("seed"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add("v1/a", sealed)
	f.Add("v1/a", []byte{})
	f.Add("", []byte("x"))

	f.Fuzz(func(t *testing.T, path string, sealed []byte) {
		// The only requirement is that no input panics and none produces
		// plaintext it should not. A caller acting on unauthenticated bytes is
		// the failure this package exists to prevent.
		got, err := Decrypt(private, path, sealed)
		if err == nil && got == nil {
			t.Error("Decrypt succeeded with no plaintext")
		}
	})
}
