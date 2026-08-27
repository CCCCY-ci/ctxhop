package sessionhub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	ctxcrypto "github.com/CCCCY-ci/ctxhop/internal/crypto"
)

// DefaultHubLogicalID is the reserved logical identity used by installations
// that have not explicitly selected a SessionHub.
const DefaultHubLogicalID = "default"

const (
	hubIdentifierDomain           = "hub-v2"
	projectIdentifierDomain       = "project-v2"
	sessionIdentifierDomain       = "session-v2"
	nativeSessionIdentifierDomain = "native-session-v2"
	replicaIdentifierDomain       = "replica-v2"
	contributionIdentifierDomain  = "contribution-v2"
	environmentIdentifierDomain   = "environment-v2"
)

// DeriveHubKey derives the keyed remote key for a logical Hub identity.
func DeriveHubKey(identifierKey []byte, logicalID string) (string, error) {
	if err := validateIdentityPart(logicalID); err != nil {
		return "", fmt.Errorf("%w: hub logical identity", ErrInvalidIdentity)
	}
	return derive(identifierKey, hubIdentifierDomain, logicalID)
}

// DeriveProjectKey derives the keyed remote key for a Project within a Hub.
func DeriveProjectKey(identifierKey []byte, hubKey, canonicalProjectIdentity string) (string, error) {
	if err := validateOpaqueID(hubKey); err != nil {
		return "", fmt.Errorf("%w: project hub key", ErrInvalidIdentity)
	}
	if err := validateIdentityPart(canonicalProjectIdentity); err != nil {
		return "", fmt.Errorf("%w: project identity", ErrInvalidIdentity)
	}
	return derive(identifierKey, projectIdentifierDomain, hubKey, canonicalProjectIdentity)
}

// DeriveSessionKey derives the keyed remote key for one logical Session.
// logicalID is independent of every Agent's native session ID.
func DeriveSessionKey(identifierKey []byte, projectKey, logicalID string) (string, error) {
	if err := validateOpaqueID(projectKey); err != nil {
		return "", fmt.Errorf("%w: session project key", ErrInvalidIdentity)
	}
	if err := validateIdentityPart(logicalID); err != nil {
		return "", fmt.Errorf("%w: session logical identity", ErrInvalidIdentity)
	}
	return derive(identifierKey, sessionIdentifierDomain, projectKey, logicalID)
}

// DeriveLegacySessionKey returns the stable logical Session key used by the
// v1 compatibility projection. The v1 session group is the only input to this
// mapping; titles, timestamps, paths and Agent names are deliberately absent
// so a read-only projection cannot accidentally turn display metadata into a
// merge decision.
func DeriveLegacySessionKey(identifierKey []byte, projectKey, legacySessionID string) (string, error) {
	if err := validateOpaqueID(legacySessionID); err != nil {
		return "", fmt.Errorf("%w: legacy session id", ErrInvalidIdentity)
	}
	return DeriveSessionKey(identifierKey, projectKey, "legacy-v1:"+legacySessionID)
}

// DeriveNativeSessionKey derives the opaque source identity for one Agent's
// plaintext native session ID.
func DeriveNativeSessionKey(identifierKey []byte, agent, nativeSessionID string) (string, error) {
	if err := validateAgent(agent); err != nil {
		return "", fmt.Errorf("%w: native session agent", ErrInvalidIdentity)
	}
	if err := validateNativeSessionID(nativeSessionID); err != nil {
		return "", fmt.Errorf("%w: native session id", ErrInvalidIdentity)
	}
	return derive(identifierKey, nativeSessionIdentifierDomain, agent, nativeSessionID)
}

// DeriveReplicaKey derives the opaque key for one Agent/device/generation
// NativeReplica.
func DeriveReplicaKey(identifierKey []byte, sessionKey, agent, nativeSessionKey, deviceID string, generation uint64) (string, error) {
	if err := validateOpaqueID(sessionKey); err != nil {
		return "", fmt.Errorf("%w: replica session key", ErrInvalidIdentity)
	}
	if err := validateAgent(agent); err != nil {
		return "", fmt.Errorf("%w: replica agent", ErrInvalidIdentity)
	}
	if err := validateOpaqueID(nativeSessionKey); err != nil {
		return "", fmt.Errorf("%w: replica native session key", ErrInvalidIdentity)
	}
	if err := validateOpaqueID(deviceID); err != nil {
		return "", fmt.Errorf("%w: replica device id", ErrInvalidIdentity)
	}
	if generation == 0 {
		return "", fmt.Errorf("%w: replica generation", ErrInvalidIdentity)
	}
	return derive(identifierKey, replicaIdentifierDomain, sessionKey, agent, nativeSessionKey, deviceID, strconv.FormatUint(generation, 10))
}

// DeriveContributionKey derives a deterministic key from the immutable
// contribution identity digest. Timestamp and retry-specific data are not
// inputs to this function.
func DeriveContributionKey(identifierKey []byte, sessionKey string, envelopeDigest [32]byte) (string, error) {
	if err := validateOpaqueID(sessionKey); err != nil {
		return "", fmt.Errorf("%w: contribution session key", ErrInvalidIdentity)
	}
	return derive(identifierKey, contributionIdentifierDomain, sessionKey, hex.EncodeToString(envelopeDigest[:]))
}

// DeriveEnvironmentKey derives the Hub-scoped key for one filtered component
// fingerprint.
func DeriveEnvironmentKey(identifierKey []byte, hubKey, fingerprint string) (string, error) {
	if err := validateOpaqueID(hubKey); err != nil {
		return "", fmt.Errorf("%w: environment hub key", ErrInvalidIdentity)
	}
	if err := validateFingerprint(fingerprint); err != nil {
		return "", fmt.Errorf("%w: environment fingerprint", ErrInvalidIdentity)
	}
	return derive(identifierKey, environmentIdentifierDomain, hubKey, fingerprint)
}

func derive(identifierKey []byte, domain string, parts ...string) (string, error) {
	key, err := ctxcrypto.DeriveIdentifier(identifierKey, domain, parts...)
	if err != nil {
		return "", fmt.Errorf("%w: derive opaque key", ErrInvalidIdentity)
	}
	return key, nil
}

// DigestBytes is a small helper for callers that need the same 32-byte digest
// shape used by Contribution identity derivation.
func DigestBytes(data []byte) [32]byte {
	return sha256.Sum256(data)
}
