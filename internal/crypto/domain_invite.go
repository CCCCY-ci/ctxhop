package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

const domainInviteProofLabel = "ctxhop/domain-invite/v1"

// ErrInvalidDomainInviteProof reports an invitation proof that cannot be
// verified with the configured domain key.
var ErrInvalidDomainInviteProof = errors.New("crypto: invalid domain invite proof")

// DomainInviteProof authenticates a canonical invitation payload with the
// domain's identifier key. The proof is portable, but the key never leaves the
// local configuration or the unlocked keyfile.
func DomainInviteProof(identifierKey, payload []byte) (string, error) {
	if len(identifierKey) == 0 {
		return "", errors.New("crypto: domain invite key is required")
	}
	if len(payload) == 0 {
		return "", errors.New("crypto: domain invite payload is required")
	}

	mac := hmac.New(sha256.New, identifierKey)
	_, _ = mac.Write([]byte(domainInviteProofLabel))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// VerifyDomainInviteProof verifies a proof without revealing whether the
// payload or the supplied key was the part that differed.
func VerifyDomainInviteProof(identifierKey, payload []byte, proof string) error {
	expected, err := DomainInviteProof(identifierKey, payload)
	if err != nil {
		return err
	}
	supplied, err := base64.RawURLEncoding.DecodeString(proof)
	if err != nil || len(supplied) != sha256.Size {
		return fmt.Errorf("%w: malformed encoding", ErrInvalidDomainInviteProof)
	}
	wanted, err := base64.RawURLEncoding.DecodeString(expected)
	if err != nil || !hmac.Equal(supplied, wanted) {
		return ErrInvalidDomainInviteProof
	}
	return nil
}
