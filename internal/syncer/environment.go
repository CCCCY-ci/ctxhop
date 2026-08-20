package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/environment"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const (
	environmentMetadataVersion   = 1
	maxEnvironmentMetadataBytes  = 64 << 10
	maxEncryptedEnvironmentBytes = maxEnvironmentMetadataBytes + 1024
)

var (
	ErrInvalidEnvironmentMetadata = errors.New("syncer: invalid environment metadata")
	ErrRemoteEnvironmentTooLarge  = errors.New("syncer: remote environment metadata is too large")
)

// EnvironmentMetadata is the optional, encrypted dependency manifest for one
// device branch. It contains references only; it never contains config files,
// tokens, or commands.
type EnvironmentMetadata struct {
	References []environment.Reference
}

type environmentMetadataWire struct {
	Version    int                     `json:"version"`
	References []environment.Reference `json:"references"`
}

func NewEnvironmentMetadata(references []environment.Reference) (EnvironmentMetadata, error) {
	normalized := environment.Normalize(references)
	metadata := EnvironmentMetadata{References: append([]environment.Reference(nil), normalized...)}
	if err := metadata.Validate(); err != nil {
		return EnvironmentMetadata{}, err
	}
	return metadata, nil
}

func (m EnvironmentMetadata) Validate() error {
	if len(m.References) > environment.MaxReferences {
		return fmt.Errorf("%w: too many references", ErrInvalidEnvironmentMetadata)
	}
	for _, reference := range m.References {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("%w: dependency: %v", ErrInvalidEnvironmentMetadata, err)
		}
	}
	return nil
}

func (m EnvironmentMetadata) MarshalBinary() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	wire := environmentMetadataWire{
		Version:    environmentMetadataVersion,
		References: append([]environment.Reference(nil), m.References...),
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("syncer: encode environment metadata: %w", err)
	}
	if len(payload) > maxEnvironmentMetadataBytes {
		return nil, fmt.Errorf("%w: payload is too large", ErrInvalidEnvironmentMetadata)
	}
	return payload, nil
}

func ParseEnvironmentMetadata(payload []byte) (EnvironmentMetadata, error) {
	if len(payload) == 0 || len(payload) > maxEnvironmentMetadataBytes {
		return EnvironmentMetadata{}, fmt.Errorf("%w: payload size is invalid", ErrInvalidEnvironmentMetadata)
	}
	var wire environmentMetadataWire
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return EnvironmentMetadata{}, fmt.Errorf("%w: decode: %v", ErrInvalidEnvironmentMetadata, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return EnvironmentMetadata{}, fmt.Errorf("%w: trailing JSON", ErrInvalidEnvironmentMetadata)
	} else if !errors.Is(err, io.EOF) {
		return EnvironmentMetadata{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidEnvironmentMetadata, err)
	}
	if wire.Version != environmentMetadataVersion {
		return EnvironmentMetadata{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidEnvironmentMetadata, wire.Version)
	}
	return NewEnvironmentMetadata(wire.References)
}

func SealEnvironment(recipient *ecdh.PublicKey, objectKey string, metadata EnvironmentMetadata) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal environment metadata: %w", err)
	}
	plaintext, err := metadata.MarshalBinary()
	if err != nil {
		return nil, err
	}
	compressed, err := compressPayload(plaintext, maxEnvironmentMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress environment metadata: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt environment metadata: %w", err)
	}
	return sealed, nil
}

func OpenEnvironment(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (EnvironmentMetadata, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: open environment metadata: %w", err)
	}
	if len(sealed) > maxEncryptedEnvironmentBytes {
		return EnvironmentMetadata{}, ErrRemoteEnvironmentTooLarge
	}
	compressed, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: decrypt environment metadata: %w", err)
	}
	plaintext, err := decompressPayload(compressed, maxEnvironmentMetadataBytes)
	if err != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: decompress environment metadata: %w", err)
	}
	metadata, err := ParseEnvironmentMetadata(plaintext)
	if err != nil {
		return EnvironmentMetadata{}, err
	}
	return metadata, nil
}

func PutEnvironmentReferences(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, references []environment.Reference) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	metadata, err := NewEnvironmentMetadata(references)
	if err != nil {
		return err
	}
	key, err := layout.EnvironmentKey()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish environment metadata: %w", err)
	}
	sealed, err := SealEnvironment(recipient, key, metadata)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish environment metadata: %w", err)
	}
	return nil
}

func readEnvironmentReferences(ctx context.Context, store remote.Remote, metadataKey string, identities []*ecdh.PrivateKey) ([]environment.Reference, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(metadataKey, "/"+metaObjectName) {
		return nil, errors.New("syncer: invalid metadata key for environment object")
	}
	environmentKey := strings.TrimSuffix(metadataKey, "/"+metaObjectName) + "/" + environmentObjectName
	if err := remote.ValidateKey(environmentKey); err != nil {
		return nil, fmt.Errorf("syncer: invalid environment object key: %w", err)
	}
	reader, err := store.Get(ctx, environmentKey)
	if err != nil {
		return nil, err
	}
	sealed, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedEnvironmentBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("syncer: read environment metadata: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("syncer: close environment metadata: %w", closeErr)
	}
	if len(sealed) > maxEncryptedEnvironmentBytes {
		return nil, ErrRemoteEnvironmentTooLarge
	}
	var lastErr error
	for _, identity := range identities {
		metadata, openErr := OpenEnvironment(identity, environmentKey, sealed)
		if openErr == nil {
			return metadata.References, nil
		}
		lastErr = openErr
	}
	return nil, fmt.Errorf("syncer: open environment metadata: %w", lastErr)
}
