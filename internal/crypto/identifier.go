package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"
)

// identifierLen is how many bytes of the digest become an identifier. 128 bits
// makes a collision impossible in practice while keeping keys short.
const identifierLen = 16

// Domain separators. Without them, the same input under two different meanings
// could produce the same identifier - a session named like a project would
// collide with it, and one could be made to stand in for the other.
const (
	domainProject = "project"
	domainSession = "session"
	domainDevice  = "device"
)

// ProjectID derives the identifier a project is stored under.
//
// The remote path must not reveal what the user works on: an object listing
// alone would otherwise show how many projects someone has and what they are
// called, without the storage provider decrypting anything (PRD §8.3).
//
// The input must be stable across machines, because every device has to derive
// the same identifier for the same project. That is why it is the normalised
// git remote and never a local path.
func ProjectID(idKey []byte, canonicalRemote string) (string, error) {
	if strings.TrimSpace(canonicalRemote) == "" {
		return "", fmt.Errorf("crypto: project identity is required")
	}
	return derive(idKey, domainProject, canonicalRemote)
}

// SessionID derives the identifier a session is stored under, scoped to its
// project so the same native id in two projects stays distinct.
func SessionID(idKey []byte, projectID, nativeID string) (string, error) {
	if projectID == "" || nativeID == "" {
		return "", fmt.Errorf("crypto: project and session identity are required")
	}
	return derive(idKey, domainSession, projectID, nativeID)
}

// DeviceID derives the identifier this machine writes under.
func DeviceID(idKey []byte, localIdentity string) (string, error) {
	if strings.TrimSpace(localIdentity) == "" {
		return "", fmt.Errorf("crypto: device identity is required")
	}
	return derive(idKey, domainDevice, localIdentity)
}

// derive computes a keyed, domain-separated identifier.
//
// Parts are joined with a byte that cannot appear in any of them, so no
// arrangement of inputs can be made to produce the digest of a different
// arrangement.
func derive(idKey []byte, domain string, parts ...string) (string, error) {
	if len(idKey) != keyLen {
		return "", fmt.Errorf("crypto: identifier key must be %d bytes", keyLen)
	}

	mac := hmac.New(sha256.New, idKey)
	mac.Write([]byte(domain))
	for _, part := range parts {
		mac.Write([]byte{0})
		mac.Write([]byte(part))
	}

	return strings.ToLower(encodeCrockford(mac.Sum(nil)[:identifierLen])), nil
}
