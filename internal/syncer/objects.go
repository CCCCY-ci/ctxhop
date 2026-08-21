package syncer

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"strconv"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const (
	objectPrefix          = "v1/projects"
	shardNameWidth        = 6
	maxShardNumber        = 999999
	metaObjectName        = "meta"
	environmentObjectName = "env"
	workspaceObjectName   = "workspace"
	gitStateObjectName    = "git"
	gitTransferObjectName = "git-transfer"
)

// ObjectLayout identifies the remote namespace for one session and one device.
// The identifiers must be keyed, opaque values from the crypto layer.
type ObjectLayout struct {
	projectID string
	sessionID string
	deviceID  string
}

// NewObjectLayout validates the opaque identifiers used in remote object keys.
func NewObjectLayout(projectID, sessionID, deviceID string) (ObjectLayout, error) {
	for name, value := range map[string]string{
		"project": projectID,
		"session": sessionID,
		"device":  deviceID,
	} {
		if err := validateIdentifier(value); err != nil {
			return ObjectLayout{}, fmt.Errorf("syncer: invalid %s identifier: %w", name, err)
		}
	}
	return ObjectLayout{projectID: projectID, sessionID: sessionID, deviceID: deviceID}, nil
}

// SessionPrefix returns the key prefix containing all device branches of the
// session.
func (l ObjectLayout) SessionPrefix() (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	return objectPrefix + "/" + l.projectID + "/sessions/" + l.sessionID, nil
}

// DevicePrefix returns the only prefix this device may write.
func (l ObjectLayout) DevicePrefix() (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/" + l.deviceID, nil
}

// MetadataKey returns this device's mutable metadata object key.
func (l ObjectLayout) MetadataKey() (string, error) {
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + metaObjectName)
}

// EnvironmentKey returns the optional dependency manifest for this device branch.
func (l ObjectLayout) EnvironmentKey() (string, error) {
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + environmentObjectName)
}

// WorkspaceKey returns the optional Git/workspace snapshot for this device branch.
func (l ObjectLayout) WorkspaceKey() (string, error) {
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + workspaceObjectName)
}

// ShardKey returns the immutable key for one device-local shard sequence.
func (l ObjectLayout) GitStateKey() (string, error) {
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + gitStateObjectName)
}

func (l ObjectLayout) GitTransferKey() (string, error) {
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + gitTransferObjectName)
}
func (l ObjectLayout) ShardKey(number uint64) (string, error) {
	if number == 0 || number > maxShardNumber {
		return "", fmt.Errorf("syncer: shard sequence must be between 1 and %d", maxShardNumber)
	}
	prefix, err := l.DevicePrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + fmt.Sprintf("%0*d", shardNameWidth, number))
}

// SealShard encodes and encrypts a shard for storage at objectKey.
//
// The exact key is authenticated by the crypto layer. The returned bytes are
// safe to pass to remote.Remote, but the plaintext must never be passed there.
func SealShard(recipient *ecdh.PublicKey, objectKey string, shard Shard) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal shard: %w", err)
	}
	plaintext, err := shard.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("syncer: encode shard for %q: %w", objectKey, err)
	}
	compressed, err := compressPayload(plaintext, maxShardBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress shard: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt shard: %w", err)
	}
	return sealed, nil
}

// OpenShard decrypts and validates a shard read from objectKey.
func OpenShard(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (Shard, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return Shard{}, fmt.Errorf("syncer: open shard: %w", err)
	}
	plaintext, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return Shard{}, fmt.Errorf("syncer: decrypt shard: %w", err)
	}
	expanded, err := decompressPayload(plaintext, maxShardBytes)
	if err != nil {
		return Shard{}, fmt.Errorf("syncer: decompress shard: %w", err)
	}
	shard, err := ParseShard(expanded)
	if err != nil {
		return Shard{}, fmt.Errorf("syncer: parse decrypted shard: %w", err)
	}
	return shard, nil
}

func (l ObjectLayout) validate() error {
	if err := validateIdentifier(l.projectID); err != nil {
		return fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	if err := validateIdentifier(l.sessionID); err != nil {
		return fmt.Errorf("syncer: invalid session identifier: %w", err)
	}
	if err := validateIdentifier(l.deviceID); err != nil {
		return fmt.Errorf("syncer: invalid device identifier: %w", err)
	}
	return nil
}

func validateIdentifier(value string) error {
	if value == "" {
		return errors.New("identifier is empty")
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return errors.New("identifier must contain only lowercase ASCII letters and digits")
	}
	return nil
}

func checkedKey(key string) (string, error) {
	if err := remote.ValidateKey(key); err != nil {
		return "", fmt.Errorf("syncer: generated object key is invalid: %w", err)
	}
	return key, nil
}

// ParseShardNumber converts a six-digit shard object name to its sequence.
// It is kept separate from ParseShard so remote listing code can reject
// metadata and foreign objects before attempting decryption.
func ParseShardNumber(name string) (uint64, error) {
	if len(name) != shardNameWidth {
		return 0, errors.New("syncer: invalid shard object name")
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return 0, errors.New("syncer: invalid shard object name")
		}
	}
	number, err := strconv.ParseUint(name, 10, 64)
	if err != nil || number == 0 || number > maxShardNumber {
		return 0, errors.New("syncer: invalid shard object name")
	}
	return number, nil
}
