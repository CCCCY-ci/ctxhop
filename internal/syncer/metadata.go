package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/environment"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const (
	metadataVersion           = 1
	maxMetadataBytes          = 1 << 20
	maxEncryptedMetadataBytes = maxMetadataBytes + 1024
)

var (
	// ErrInvalidMetadata reports an invalid or unsupported metadata envelope.
	ErrInvalidMetadata = errors.New("syncer: invalid session metadata")

	// ErrNoRemoteMetadata reports a session prefix without readable metadata
	// objects.
	ErrNoRemoteMetadata = errors.New("syncer: remote session has no metadata")

	// ErrDuplicateMetadata reports multiple list entries for one device's
	// mutable metadata object.
	ErrDuplicateMetadata = errors.New("syncer: duplicate remote metadata")

	// ErrRemoteMetadataTooLarge reports metadata that exceeds the bounded
	// encrypted object size before decryption.
	ErrRemoteMetadataTooLarge = errors.New("syncer: remote metadata is too large")
)

// Metadata is the format-neutral plaintext carried by one device's mutable
// metadata object. Payload is an opaque compact JSON value for higher layers.
type Metadata struct {
	RecordCount uint64
	HeadDigest  [32]byte
	Payload     []byte
}

// NewMetadata validates and copies one metadata payload.
func NewMetadata(recordCount uint64, headDigest [32]byte, payload []byte) (Metadata, error) {
	metadata := Metadata{
		RecordCount: recordCount,
		HeadDigest:  headDigest,
		Payload:     append([]byte(nil), payload...),
	}
	if err := metadata.Validate(); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

// Validate checks the metadata envelope and its opaque JSON payload.
func (m Metadata) Validate() error {
	if m.RecordCount == 0 && m.HeadDigest != EmptyDigest() {
		return fmt.Errorf("%w: empty prefix has a non-empty digest", ErrInvalidMetadata)
	}
	if len(m.Payload) == 0 {
		return fmt.Errorf("%w: payload is empty", ErrInvalidMetadata)
	}
	if len(m.Payload) > maxMetadataBytes {
		return fmt.Errorf("%w: payload exceeds %d bytes", ErrInvalidMetadata, maxMetadataBytes)
	}
	if !json.Valid(m.Payload) {
		return fmt.Errorf("%w: payload is not JSON", ErrInvalidMetadata)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, m.Payload); err != nil {
		return fmt.Errorf("%w: compact payload: %v", ErrInvalidMetadata, err)
	}
	if !bytes.Equal(compact.Bytes(), m.Payload) {
		return fmt.Errorf("%w: payload is not compact", ErrInvalidMetadata)
	}
	return nil
}

// MarshalBinary encodes a deterministic plaintext metadata envelope.
func (m Metadata) MarshalBinary() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	wire := metadataWire{
		Version:     metadataVersion,
		RecordCount: m.RecordCount,
		HeadDigest:  fmt.Sprintf("%x", m.HeadDigest),
		Payload:     append(json.RawMessage(nil), m.Payload...),
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(wire); err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("%w: encoded envelope exceeds %d bytes", ErrInvalidMetadata, maxMetadataBytes)
	}
	return data, nil
}

// ParseMetadata strictly decodes metadata received from an untrusted source.
func ParseMetadata(data []byte) (Metadata, error) {
	if len(data) == 0 {
		return Metadata{}, fmt.Errorf("%w: empty envelope", ErrInvalidMetadata)
	}
	if len(data) > maxMetadataBytes {
		return Metadata{}, fmt.Errorf("%w: encoded envelope exceeds %d bytes", ErrInvalidMetadata, maxMetadataBytes)
	}

	var wire metadataWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Metadata{}, fmt.Errorf("%w: decode envelope: %v", ErrInvalidMetadata, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Metadata{}, fmt.Errorf("%w: envelope contains trailing JSON", ErrInvalidMetadata)
	} else if !errors.Is(err, io.EOF) {
		return Metadata{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidMetadata, err)
	}
	if wire.Version != metadataVersion {
		return Metadata{}, fmt.Errorf("%w: unsupported metadata version %d", ErrInvalidMetadata, wire.Version)
	}
	digest, err := parseDigest(wire.HeadDigest)
	if err != nil {
		return Metadata{}, fmt.Errorf("%w: head digest: %v", ErrInvalidMetadata, err)
	}
	return NewMetadata(wire.RecordCount, digest, wire.Payload)
}

type metadataWire struct {
	Version     int             `json:"version"`
	RecordCount uint64          `json:"recordCount"`
	HeadDigest  string          `json:"headDigest"`
	Payload     json.RawMessage `json:"payload"`
}

// SealMetadata encodes, compresses and encrypts metadata for its exact object
// key. The compression wrapper is optional for small metadata payloads.
func SealMetadata(recipient *ecdh.PublicKey, objectKey string, metadata Metadata) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal metadata: %w", err)
	}
	plaintext, err := metadata.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("syncer: encode metadata: %w", err)
	}
	compressed, err := compressPayload(plaintext, maxMetadataBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress metadata: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt metadata: %w", err)
	}
	return sealed, nil
}

// OpenMetadata decrypts, decompresses and validates metadata read from its
// exact object key. Payloads written before compression are accepted as-is.
func OpenMetadata(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (Metadata, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return Metadata{}, fmt.Errorf("syncer: open metadata: %w", err)
	}
	compressed, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return Metadata{}, fmt.Errorf("syncer: decrypt metadata: %w", err)
	}
	plaintext, err := decompressPayload(compressed, maxMetadataBytes)
	if err != nil {
		return Metadata{}, fmt.Errorf("syncer: decompress metadata: %w", err)
	}
	metadata, err := ParseMetadata(plaintext)
	if err != nil {
		return Metadata{}, fmt.Errorf("syncer: parse metadata: %w", err)
	}
	return metadata, nil
}

// PutMetadata encrypts and publishes metadata to the local device branch.
func PutMetadata(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, metadata Metadata) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	if err := metadata.Validate(); err != nil {
		return err
	}
	key, err := layout.MetadataKey()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish metadata: %w", err)
	}
	sealed, err := SealMetadata(recipient, key, metadata)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish metadata: %w", err)
	}
	return nil
}

// MetadataRef is one validated metadata object associated with a device.
type MetadataRef struct {
	DeviceID              string
	Metadata              Metadata
	Environment           []environment.Reference
	EnvironmentComponents []environment.Component
}

// FetchMetadata lists, reads, decrypts, and validates every device metadata
// object under one remote session prefix.
func FetchMetadata(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey) ([]MetadataRef, error) {
	return FetchMetadataWithIdentities(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity})
}

// FetchMetadataWithIdentities reads metadata encrypted under any retained
// content-key generation.
func FetchMetadataWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey) ([]MetadataRef, error) {
	return FetchMetadataWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, nil)
}

// FetchMetadataWithIdentitiesAndDevices reads metadata and optionally ignores
// branches whose device ID is not in the current membership set.
func FetchMetadataWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]MetadataRef, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	layout, err := NewSessionLayout(projectID, sessionID)
	if err != nil {
		return nil, err
	}
	prefix, err := layout.Prefix()
	if err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list remote metadata: %w", err)
	}
	refs, err := collectMetadataRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, ErrNoRemoteMetadata
	}

	devices := make([]string, 0, len(refs))
	for device := range refs {
		devices = append(devices, device)
	}
	sort.Strings(devices)
	out := make([]MetadataRef, 0, len(devices))
	for _, device := range devices {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("syncer: read remote metadata: %w", err)
		}
		if allowed != nil {
			if _, ok := allowed[device]; !ok {
				continue
			}
		}
		key := refs[device]
		sealed, err := readRemoteMetadata(ctx, store, key)
		if err != nil {
			return nil, fmt.Errorf("syncer: read remote metadata: %w", err)
		}
		metadata, err := openMetadataWithIdentities(identities, key, sealed)
		if err != nil {
			return nil, fmt.Errorf("syncer: open remote metadata: %w", err)
		}
		environmentReferences := []environment.Reference(nil)
		environmentComponents := []environment.Component(nil)
		if environmentMetadata, environmentErr := readEnvironmentMetadata(ctx, store, key, identities); environmentErr == nil {
			environmentReferences = environmentMetadata.References
			environmentComponents = environment.ComponentSummaries(environmentMetadata.Components)
		} else if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("syncer: read remote environment metadata: %w", contextErr)
		}
		out = append(out, MetadataRef{DeviceID: device, Metadata: metadata, Environment: environmentReferences, EnvironmentComponents: environmentComponents})
	}
	if len(out) == 0 {
		return nil, ErrNoRemoteMetadata
	}
	return out, nil
}

func collectMetadataRefs(prefix string, objects []remote.ObjectInfo) (map[string]string, error) {
	refs := make(map[string]string)
	for _, object := range objects {
		device, ok := parseMetadataObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		if _, exists := refs[device]; exists {
			return nil, fmt.Errorf("%w for one device", ErrDuplicateMetadata)
		}
		refs[device] = object.Key
	}
	return refs, nil
}

func parseMetadataObjectKey(prefix, key string) (string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")
	if len(parts) != 2 || parts[1] != metaObjectName || validateIdentifier(parts[0]) != nil {
		return "", false
	}
	expected, err := checkedKey(prefix + "/" + parts[0] + "/" + metaObjectName)
	if err != nil || expected != key {
		return "", false
	}
	return parts[0], true
}

func readRemoteMetadata(ctx context.Context, store remote.Remote, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get remote metadata: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedMetadataBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("read remote metadata: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("read remote metadata: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close remote metadata: %w", closeErr)
	}
	if len(data) > maxEncryptedMetadataBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrRemoteMetadataTooLarge, maxEncryptedMetadataBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read remote metadata: %w", err)
	}
	return data, nil
}
