package crypto

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

const (
	domainFingerprintBytes = 16
	domainFingerprintLabel = "ctxhop/domain-fingerprint/v1"
)

// DomainFingerprint derives a short, non-secret identifier for one configured
// storage namespace and its pinned public encryption identity. It is intended
// for human confirmation, not authorization: possession of a matching value
// does not grant access to the domain.
func DomainFingerprint(namespace string, identityPublic []byte) (string, error) {
	namespace = strings.TrimSpace(namespace)
	switch {
	case namespace == "":
		return "", errors.New("crypto: domain namespace is required")
	case strings.ContainsRune(namespace, 0):
		return "", errors.New("crypto: domain namespace contains a NUL byte")
	case len(identityPublic) == 0:
		return "", fmt.Errorf("crypto: domain public identity is required")
	}

	hash := sha256.New()
	_, _ = hash.Write([]byte(domainFingerprintLabel))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(namespace))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(identityPublic)
	return strings.ToLower(encodeCrockford(hash.Sum(nil)[:domainFingerprintBytes])), nil
}
