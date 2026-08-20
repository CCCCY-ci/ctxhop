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
	legacyEnvironmentMetadataVersion = 1
	environmentMetadataVersion       = 2
	maxEnvironmentMetadataBytes      = 64 << 10
	maxEncryptedEnvironmentBytes     = maxEnvironmentMetadataBytes + 1024
)

var (
	ErrInvalidEnvironmentMetadata = errors.New("syncer: invalid environment metadata")
	ErrRemoteEnvironmentTooLarge  = errors.New("syncer: remote environment metadata is too large")
)

// EnvironmentMetadata is the optional, encrypted dependency manifest for one
// device branch. Component bodies are limited to filtered, non-sensitive text;
// it never contains tokens, credentials, commands, or project files.
type EnvironmentMetadata struct {
	References []environment.Reference
	Components []environment.ComponentContent
}

type environmentMetadataWire struct {
	Version    int                            `json:"version"`
	References []environment.Reference        `json:"references"`
	Components []environment.ComponentContent `json:"components,omitempty"`
}

func NewEnvironmentMetadata(references []environment.Reference) (EnvironmentMetadata, error) {
	return NewEnvironmentMetadataWithComponents(references, nil)
}

func NewEnvironmentMetadataWithComponents(references []environment.Reference, components []environment.ComponentContent) (EnvironmentMetadata, error) {
	normalized := environment.Normalize(references)
	normalizedComponents := environment.NormalizeComponentContents(components)
	metadata := EnvironmentMetadata{
		References: append([]environment.Reference(nil), normalized...),
		Components: append([]environment.ComponentContent(nil), normalizedComponents...),
	}
	if err := metadata.Validate(); err != nil {
		return EnvironmentMetadata{}, err
	}
	return metadata, nil
}

func (m EnvironmentMetadata) Validate() error {
	if len(m.References) > environment.MaxReferences {
		return fmt.Errorf("%w: too many references", ErrInvalidEnvironmentMetadata)
	}
	observedReferences := make(map[string]struct{})
	for _, reference := range m.References {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("%w: dependency: %v", ErrInvalidEnvironmentMetadata, err)
		}
		observedReferences[reference.Kind+"\x00"+reference.Name] = struct{}{}
	}
	if len(m.Components) > environment.MaxComponents {
		return fmt.Errorf("%w: too many components", ErrInvalidEnvironmentMetadata)
	}
	seen := make(map[string]string, len(m.Components))
	totalContent := 0
	for _, component := range m.Components {
		if err := component.Validate(); err != nil {
			return fmt.Errorf("%w: component: %v", ErrInvalidEnvironmentMetadata, err)
		}
		componentReference := component.Component.Kind + "\x00" + component.Component.Name
		if _, observed := observedReferences[componentReference]; !observed {
			return fmt.Errorf("%w: component is not referenced by this session", ErrInvalidEnvironmentMetadata)
		}
		key := component.Component.Kind + "\x00" + component.Component.Name + "\x00" + component.Component.Scope + "\x00" + component.Component.ProjectID
		if previous, exists := seen[key]; exists {
			if previous != component.Component.Fingerprint {
				return fmt.Errorf("%w: conflicting component bodies", ErrInvalidEnvironmentMetadata)
			}
			return fmt.Errorf("%w: duplicate component", ErrInvalidEnvironmentMetadata)
		}
		seen[key] = component.Component.Fingerprint
		totalContent += len(component.Content)
		if totalContent > environment.MaxTotalComponentContentBytes {
			return fmt.Errorf("%w: component content is too large", ErrInvalidEnvironmentMetadata)
		}
	}
	return nil
}

func (m EnvironmentMetadata) MarshalBinary() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	version := legacyEnvironmentMetadataVersion
	if len(m.Components) != 0 {
		version = environmentMetadataVersion
	}
	wire := environmentMetadataWire{
		Version:    version,
		References: append([]environment.Reference(nil), m.References...),
		Components: append([]environment.ComponentContent(nil), m.Components...),
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
	if wire.Version != legacyEnvironmentMetadataVersion && wire.Version != environmentMetadataVersion {
		return EnvironmentMetadata{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidEnvironmentMetadata, wire.Version)
	}
	if wire.Version == legacyEnvironmentMetadataVersion && len(wire.Components) != 0 {
		return EnvironmentMetadata{}, fmt.Errorf("%w: legacy envelope contains components", ErrInvalidEnvironmentMetadata)
	}
	return NewEnvironmentMetadataWithComponents(wire.References, wire.Components)
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
	return PutEnvironmentManifest(ctx, store, recipient, layout, references, nil)
}

func PutEnvironmentManifest(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, references []environment.Reference, components []environment.ComponentContent) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	metadata, err := NewEnvironmentMetadataWithComponents(references, components)
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

func readEnvironmentMetadata(ctx context.Context, store remote.Remote, metadataKey string, identities []*ecdh.PrivateKey) (EnvironmentMetadata, error) {
	if ctx == nil {
		return EnvironmentMetadata{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return EnvironmentMetadata{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return EnvironmentMetadata{}, err
	}
	if !strings.HasSuffix(metadataKey, "/"+metaObjectName) {
		return EnvironmentMetadata{}, errors.New("syncer: invalid metadata key for environment object")
	}
	environmentKey := strings.TrimSuffix(metadataKey, "/"+metaObjectName) + "/" + environmentObjectName
	if err := remote.ValidateKey(environmentKey); err != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: invalid environment object key: %w", err)
	}
	reader, err := store.Get(ctx, environmentKey)
	if err != nil {
		return EnvironmentMetadata{}, err
	}
	sealed, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedEnvironmentBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: read environment metadata: %w", readErr)
	}
	if closeErr != nil {
		return EnvironmentMetadata{}, fmt.Errorf("syncer: close environment metadata: %w", closeErr)
	}
	if len(sealed) > maxEncryptedEnvironmentBytes {
		return EnvironmentMetadata{}, ErrRemoteEnvironmentTooLarge
	}
	var lastErr error
	for _, identity := range identities {
		metadata, openErr := OpenEnvironment(identity, environmentKey, sealed)
		if openErr == nil {
			return metadata, nil
		}
		lastErr = openErr
	}
	return EnvironmentMetadata{}, fmt.Errorf("syncer: open environment metadata: %w", lastErr)
}

func readEnvironmentReferences(ctx context.Context, store remote.Remote, metadataKey string, identities []*ecdh.PrivateKey) ([]environment.Reference, error) {
	metadata, err := readEnvironmentMetadata(ctx, store, metadataKey, identities)
	if err != nil {
		return nil, err
	}
	return metadata.References, nil
}
