package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// recoveryKeyLen is 256 bits, matching the strength of the key it unwraps.
const recoveryKeyLen = 32

// crockford is Base32 without I, L, O or U: the characters people confuse with
// 1, 1, 0 and V when copying by hand. A recovery key exists to be written on
// paper, so the alphabet is chosen for transcription rather than density.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// recoveryPrefix makes a written-down key recognisable months later.
const recoveryPrefix = "AGSY"

// checksumChars is how much of the digest is appended. Twenty bits catches any
// realistic transcription slip while adding only one group to copy.
const checksumChars = 4

// ErrRecoveryKeyChecksum reports a recovery key that does not check out, which
// almost always means it was mistyped rather than that it is the wrong key.
var ErrRecoveryKeyChecksum = errors.New("crypto: recovery key checksum does not match; check for a mistyped character")

// NewRecoveryKey generates a recovery key and its written form.
func NewRecoveryKey() ([]byte, string) {
	raw := make([]byte, recoveryKeyLen)
	rand.Read(raw)
	return raw, FormatRecoveryKey(raw)
}

// FormatRecoveryKey renders a recovery key for a human to copy.
func FormatRecoveryKey(raw []byte) string {
	body := encodeCrockford(raw)
	full := body + checksumOf(body)

	groups := []string{recoveryPrefix}
	for i := 0; i < len(full); i += 4 {
		end := min(i+4, len(full))
		groups = append(groups, full[i:end])
	}
	return strings.Join(groups, "-")
}

// ParseRecoveryKey reads a recovery key back.
//
// Input is normalised before anything else: case is ignored, separators are
// ignored, and the characters Crockford treats as aliases are folded. Somebody
// reading their own handwriting should not be told their key is wrong because
// they wrote a lowercase l for a 1.
func ParseRecoveryKey(s string) ([]byte, error) {
	cleaned := normalizeCrockford(s)
	cleaned = strings.TrimPrefix(cleaned, recoveryPrefix)

	if len(cleaned) < checksumChars {
		return nil, fmt.Errorf("crypto: recovery key is too short")
	}
	body, checksum := cleaned[:len(cleaned)-checksumChars], cleaned[len(cleaned)-checksumChars:]

	// Verified before decoding so a mistyped character is reported as exactly
	// that, rather than surfacing later as "wrong passphrase" - which would
	// send the user looking in the wrong place entirely.
	if checksumOf(body) != checksum {
		return nil, ErrRecoveryKeyChecksum
	}

	raw, err := decodeCrockford(body)
	if err != nil {
		return nil, err
	}
	if len(raw) != recoveryKeyLen {
		return nil, fmt.Errorf("crypto: recovery key decodes to %d bytes, expected %d", len(raw), recoveryKeyLen)
	}
	return raw, nil
}

func checksumOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return encodeCrockford(sum[:])[:checksumChars]
}

// normalizeCrockford uppercases, drops separators, and folds the aliases
// Crockford defines.
func normalizeCrockford(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ', '\t', '\n', '\r', '_':
			// Separators are for reading, not for meaning.
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// encodeCrockford writes bytes as base32 without padding.
func encodeCrockford(data []byte) string {
	var b strings.Builder
	var buf, bits uint

	for _, c := range data {
		buf = buf<<8 | uint(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			b.WriteByte(crockford[(buf>>bits)&31])
		}
	}
	if bits > 0 {
		b.WriteByte(crockford[(buf<<(5-bits))&31])
	}
	return b.String()
}

// decodeCrockford reverses encodeCrockford. Input must already be normalised.
func decodeCrockford(s string) ([]byte, error) {
	var out []byte
	var buf, bits uint

	for i := 0; i < len(s); i++ {
		v := strings.IndexByte(crockford, s[i])
		if v < 0 {
			return nil, fmt.Errorf("crypto: recovery key contains %q, which is not part of the alphabet", s[i])
		}
		buf = buf<<5 | uint(v)
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(buf>>bits))
		}
	}
	return out, nil
}
