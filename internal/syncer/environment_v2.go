package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

const (
	maxEnvironmentComponentPlainBytes   = environment.MaxComponentContentBytes + (8 << 10)
	maxEnvironmentComponentObjectBytes  = maxEnvironmentComponentPlainBytes + 1024
	maxEnvironmentAttachmentPlainBytes  = 64 << 10
	maxEnvironmentAttachmentObjectBytes = maxEnvironmentAttachmentPlainBytes + 1024
	environmentComponentVersion         = 1
)

var (
	// ErrEnvironmentComponentConflict means an immutable component key already
	// contains a different filtered descriptor or body.
	ErrEnvironmentComponentConflict = errors.New("syncer: v2 environment component conflicts")

	// ErrEnvironmentAttachmentConflict means an immutable attachment key already
	// contains a different contribution-bound environment snapshot.
	ErrEnvironmentAttachmentConflict = errors.New("syncer: v2 environment attachment conflicts")

	// ErrEnvironmentObjectIncomplete means that one of the required metadata or
	// body objects is absent or does not agree with its companion object.
	ErrEnvironmentObjectIncomplete = errors.New("syncer: v2 environment object is incomplete")
)

type environmentComponentDescriptor struct {
	Version   int                   `json:"version"`
	Component environment.Component `json:"component"`
}

// PutEnvironmentComponent publishes one filtered component into the
// Hub-scoped content-addressed pool. The component descriptor and body are
// immutable, device-owned objects; retries with retained identities verify
// equality instead of overwriting either object.
func PutEnvironmentComponent(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout EnvironmentHubLayout, componentKey, deviceID string, content environment.ComponentContent) error {
	return PutEnvironmentComponentWithIdentities(ctx, store, recipient, layout, componentKey, deviceID, content, nil)
}

// PutEnvironmentComponentWithIdentities is the authenticated retry form of
// PutEnvironmentComponent.
func PutEnvironmentComponentWithIdentities(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout EnvironmentHubLayout, componentKey, deviceID string, content environment.ComponentContent, identities []*ecdh.PrivateKey) error {
	if err := validateEnvironmentWriteArgs(ctx, store, recipient); err != nil {
		return err
	}
	if err := validateIdentities(identities); err != nil {
		return err
	}
	if err := content.Validate(); err != nil {
		return fmt.Errorf("syncer: environment component: %w", err)
	}
	descriptorKey, err := layout.ComponentDescriptorKey(componentKey, deviceID)
	if err != nil {
		return err
	}
	bodyKey, err := layout.ComponentBodyKey(componentKey, deviceID)
	if err != nil {
		return err
	}
	descriptorPayload, err := json.Marshal(environmentComponentDescriptor{
		Version:   environmentComponentVersion,
		Component: content.Component,
	})
	if err != nil {
		return fmt.Errorf("syncer: encode environment component descriptor: %w", err)
	}
	bodyPayload, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("syncer: encode environment component body: %w", err)
	}
	descriptorSealed, err := sealEnvironmentV2Payload(recipient, descriptorKey, descriptorPayload, maxEnvironmentComponentPlainBytes, maxEnvironmentComponentObjectBytes)
	if err != nil {
		return fmt.Errorf("syncer: seal environment component descriptor: %w", err)
	}
	bodySealed, err := sealEnvironmentV2Payload(recipient, bodyKey, bodyPayload, maxEnvironmentComponentPlainBytes, maxEnvironmentComponentObjectBytes)
	if err != nil {
		return fmt.Errorf("syncer: seal environment component body: %w", err)
	}
	if err := putReplicaImmutable(ctx, store, descriptorKey, descriptorSealed, maxEnvironmentComponentObjectBytes, identities, func(existing []byte) error {
		parsed, err := openEnvironmentComponentDescriptor(identities, descriptorKey, existing)
		if err != nil {
			return err
		}
		if parsed.Version != environmentComponentVersion || parsed.Component != content.Component {
			return ErrEnvironmentComponentConflict
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: publish environment component descriptor: %w", err)
	}
	if err := putReplicaImmutable(ctx, store, bodyKey, bodySealed, maxEnvironmentComponentObjectBytes, identities, func(existing []byte) error {
		parsed, err := openEnvironmentComponentBody(identities, bodyKey, existing)
		if err != nil {
			return err
		}
		if parsed.Component != content.Component || !bytes.Equal(parsed.Content, content.Content) {
			return ErrEnvironmentComponentConflict
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: publish environment component body: %w", err)
	}
	return nil
}

// FetchEnvironmentComponent reads and authenticates both objects of one
// filtered component. A descriptor without its immutable body is never
// returned as usable environment data.
func FetchEnvironmentComponent(ctx context.Context, store remote.Remote, layout EnvironmentHubLayout, componentKey, deviceID string, identities []*ecdh.PrivateKey) (environment.ComponentContent, error) {
	if ctx == nil {
		return environment.ComponentContent{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return environment.ComponentContent{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return environment.ComponentContent{}, err
	}
	descriptorKey, err := layout.ComponentDescriptorKey(componentKey, deviceID)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	bodyKey, err := layout.ComponentBodyKey(componentKey, deviceID)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	descriptor, err := fetchEnvironmentComponentDescriptor(ctx, store, descriptorKey, identities)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	body, err := fetchEnvironmentComponentBody(ctx, store, bodyKey, identities)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	if descriptor.Version != environmentComponentVersion || descriptor.Component != body.Component {
		return environment.ComponentContent{}, fmt.Errorf("%w: descriptor and body differ", ErrEnvironmentObjectIncomplete)
	}
	return body, nil
}

// PutEnvironmentAttachment publishes the contribution-bound environment
// attachment. Metadata and the first immutable body are intentionally both
// present so metadata-only listing can discover a branch while reads still
// require a complete authenticated object pair.
func PutEnvironmentAttachment(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, environmentKey, deviceID string, attachment sessionhub.EnvironmentAttachment) error {
	return PutEnvironmentAttachmentWithIdentities(ctx, store, recipient, layout, environmentKey, deviceID, attachment, nil)
}

// PutEnvironmentAttachmentWithIdentities is the authenticated retry form of
// PutEnvironmentAttachment.
func PutEnvironmentAttachmentWithIdentities(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, environmentKey, deviceID string, attachment sessionhub.EnvironmentAttachment, identities []*ecdh.PrivateKey) error {
	if err := validateEnvironmentWriteArgs(ctx, store, recipient); err != nil {
		return err
	}
	if err := validateIdentities(identities); err != nil {
		return err
	}
	if err := attachment.Validate(); err != nil {
		return fmt.Errorf("syncer: environment attachment: %w", err)
	}
	descriptorKey, err := layout.EnvironmentAttachmentDescriptorKey(environmentKey, deviceID)
	if err != nil {
		return err
	}
	bodyKey, err := layout.EnvironmentAttachmentBodyKey(environmentKey, deviceID)
	if err != nil {
		return err
	}
	payload, err := attachment.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode environment attachment: %w", err)
	}
	descriptorSealed, err := sealEnvironmentV2Payload(recipient, descriptorKey, payload, maxEnvironmentAttachmentPlainBytes, maxEnvironmentAttachmentObjectBytes)
	if err != nil {
		return fmt.Errorf("syncer: seal environment attachment metadata: %w", err)
	}
	bodySealed, err := sealEnvironmentV2Payload(recipient, bodyKey, payload, maxEnvironmentAttachmentPlainBytes, maxEnvironmentAttachmentObjectBytes)
	if err != nil {
		return fmt.Errorf("syncer: seal environment attachment body: %w", err)
	}
	verify := func(key string, existing []byte) error {
		opened, err := openEnvironmentAttachment(identities, key, existing)
		if err != nil {
			return err
		}
		canonical, err := opened.MarshalBinary()
		if err != nil {
			return err
		}
		if !bytes.Equal(canonical, payload) {
			return ErrEnvironmentAttachmentConflict
		}
		return nil
	}
	if err := putReplicaImmutable(ctx, store, descriptorKey, descriptorSealed, maxEnvironmentAttachmentObjectBytes, identities, func(existing []byte) error {
		return verify(descriptorKey, existing)
	}); err != nil {
		return fmt.Errorf("syncer: publish environment attachment metadata: %w", err)
	}
	if err := putReplicaImmutable(ctx, store, bodyKey, bodySealed, maxEnvironmentAttachmentObjectBytes, identities, func(existing []byte) error {
		return verify(bodyKey, existing)
	}); err != nil {
		return fmt.Errorf("syncer: publish environment attachment body: %w", err)
	}
	return nil
}

// FetchEnvironmentAttachment reads both immutable attachment objects and
// rejects a partially published or conflicting pair.
func FetchEnvironmentAttachment(ctx context.Context, store remote.Remote, layout SessionHubLayout, environmentKey, deviceID string, identities []*ecdh.PrivateKey) (sessionhub.EnvironmentAttachment, error) {
	if ctx == nil {
		return sessionhub.EnvironmentAttachment{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return sessionhub.EnvironmentAttachment{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	descriptorKey, err := layout.EnvironmentAttachmentDescriptorKey(environmentKey, deviceID)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	bodyKey, err := layout.EnvironmentAttachmentBodyKey(environmentKey, deviceID)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	descriptor, err := fetchEnvironmentAttachmentObject(ctx, store, descriptorKey, identities)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	body, err := fetchEnvironmentAttachmentObject(ctx, store, bodyKey, identities)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	descriptorBytes, err := descriptor.MarshalBinary()
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	bodyBytes, err := body.MarshalBinary()
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	if !bytes.Equal(descriptorBytes, bodyBytes) {
		return sessionhub.EnvironmentAttachment{}, fmt.Errorf("%w: metadata and body differ", ErrEnvironmentObjectIncomplete)
	}
	return descriptor, nil
}

func validateEnvironmentWriteArgs(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	return ctx.Err()
}

func sealEnvironmentV2Payload(recipient *ecdh.PublicKey, objectKey string, payload []byte, maxPlain, maxObject int) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > maxPlain {
		return nil, fmt.Errorf("%w: plaintext size is invalid", ErrEnvironmentObjectIncomplete)
	}
	compressed, err := compressPayload(payload, maxPlain)
	if err != nil {
		return nil, err
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, err
	}
	if len(sealed) > maxObject {
		return nil, fmt.Errorf("%w: encrypted object exceeds %d bytes", ErrEnvironmentObjectIncomplete, maxObject)
	}
	return sealed, nil
}

func openEnvironmentV2Payload(identities []*ecdh.PrivateKey, objectKey string, sealed []byte, maxPlain int) ([]byte, error) {
	if len(sealed) > maxPlain+1024 {
		return nil, fmt.Errorf("%w: encrypted object is too large", ErrEnvironmentObjectIncomplete)
	}
	compressed, err := openReplicaPayloadWithIdentities(identities, objectKey, sealed)
	if err != nil {
		return nil, err
	}
	return decompressPayload(compressed, maxPlain)
}

func fetchEnvironmentV2Payload(ctx context.Context, store remote.Remote, objectKey string, identities []*ecdh.PrivateKey, maxPlain, maxObject int) ([]byte, error) {
	sealed, err := readReplicaObject(ctx, store, objectKey, int64(maxObject))
	if errors.Is(err, remote.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrEnvironmentObjectIncomplete, err)
	}
	if err != nil {
		return nil, err
	}
	return openEnvironmentV2Payload(identities, objectKey, sealed, maxPlain)
}

func openEnvironmentComponentDescriptor(identities []*ecdh.PrivateKey, key string, sealed []byte) (environmentComponentDescriptor, error) {
	payload, err := openEnvironmentV2Payload(identities, key, sealed, maxEnvironmentComponentPlainBytes)
	if err != nil {
		return environmentComponentDescriptor{}, err
	}
	var descriptor environmentComponentDescriptor
	if err := decodeEnvironmentJSON(payload, &descriptor); err != nil {
		return environmentComponentDescriptor{}, err
	}
	if descriptor.Version != environmentComponentVersion || descriptor.Component.Validate() != nil {
		return environmentComponentDescriptor{}, fmt.Errorf("%w: component descriptor is invalid", ErrEnvironmentObjectIncomplete)
	}
	return descriptor, nil
}

func openEnvironmentComponentBody(identities []*ecdh.PrivateKey, key string, sealed []byte) (environment.ComponentContent, error) {
	payload, err := openEnvironmentV2Payload(identities, key, sealed, maxEnvironmentComponentPlainBytes)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	var content environment.ComponentContent
	if err := decodeEnvironmentJSON(payload, &content); err != nil {
		return environment.ComponentContent{}, err
	}
	if err := content.Validate(); err != nil {
		return environment.ComponentContent{}, fmt.Errorf("%w: component body is invalid", ErrEnvironmentObjectIncomplete)
	}
	return content, nil
}

func fetchEnvironmentComponentDescriptor(ctx context.Context, store remote.Remote, key string, identities []*ecdh.PrivateKey) (environmentComponentDescriptor, error) {
	sealed, err := readReplicaObject(ctx, store, key, maxEnvironmentComponentObjectBytes)
	if err != nil {
		return environmentComponentDescriptor{}, err
	}
	return openEnvironmentComponentDescriptor(identities, key, sealed)
}

func fetchEnvironmentComponentBody(ctx context.Context, store remote.Remote, key string, identities []*ecdh.PrivateKey) (environment.ComponentContent, error) {
	sealed, err := readReplicaObject(ctx, store, key, maxEnvironmentComponentObjectBytes)
	if err != nil {
		return environment.ComponentContent{}, err
	}
	return openEnvironmentComponentBody(identities, key, sealed)
}

func openEnvironmentAttachment(identities []*ecdh.PrivateKey, key string, sealed []byte) (sessionhub.EnvironmentAttachment, error) {
	payload, err := openEnvironmentV2Payload(identities, key, sealed, maxEnvironmentAttachmentPlainBytes)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	var attachment sessionhub.EnvironmentAttachment
	if err := decodeEnvironmentJSON(payload, &attachment); err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	if err := attachment.Validate(); err != nil {
		return sessionhub.EnvironmentAttachment{}, fmt.Errorf("%w: attachment is invalid", ErrEnvironmentObjectIncomplete)
	}
	return attachment, nil
}

func fetchEnvironmentAttachmentObject(ctx context.Context, store remote.Remote, key string, identities []*ecdh.PrivateKey) (sessionhub.EnvironmentAttachment, error) {
	sealed, err := readReplicaObject(ctx, store, key, maxEnvironmentAttachmentObjectBytes)
	if err != nil {
		return sessionhub.EnvironmentAttachment{}, err
	}
	return openEnvironmentAttachment(identities, key, sealed)
}

func decodeEnvironmentJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("%w: decode environment object: %v", ErrEnvironmentObjectIncomplete, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("%w: trailing environment object", ErrEnvironmentObjectIncomplete)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing environment data: %v", ErrEnvironmentObjectIncomplete, err)
	}
	return nil
}
