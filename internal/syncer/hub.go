package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

// HubMetadataRef is the metadata-only view of one device-owned Hub
// descriptor. Hub descriptors are device-owned so two devices can announce
// the same logical Hub without racing on a mutable shared object.
type HubMetadataRef struct {
	Descriptor sessionhub.HubDescriptor
	DeviceID   string
}

// HubProjectMetadataRef is the metadata-only view of one Project announced
// inside a Hub. Project descriptors are device-owned for the same reason Hub
// descriptors are: several devices can announce the same logical Project
// without rewriting one shared object.
type HubProjectMetadataRef struct {
	Descriptor sessionhub.ProjectDescriptor
	DeviceID   string
}

var ErrNoProjectMetadata = errors.New("syncer: Hub has no Project metadata")

// FetchHubMetadata lists and authenticates all visible Hub descriptors. It
// never reads Projects, Sessions, Replica shards, or native session bodies.
func FetchHubMetadata(ctx context.Context, store remote.Remote, identities []*ecdh.PrivateKey) ([]HubMetadataRef, error) {
	return FetchHubMetadataWithDevices(ctx, store, identities, nil)
}

// FetchHubMetadataWithDevices filters Hub descriptor announcements by active
// device membership before decrypting them.
func FetchHubMetadataWithDevices(ctx context.Context, store remote.Remote, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]HubMetadataRef, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, v2ObjectPrefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list v2 Hubs: %w", err)
	}
	refs := make([]hubDescriptorRef, 0)
	seen := make(map[hubDescriptorRef]struct{})
	for _, object := range objects {
		hubID, deviceID, ok := parseHubDescriptorObjectKey(object.Key)
		if !ok {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[deviceID]; !ok {
				continue
			}
		}
		ref := hubDescriptorRef{HubID: hubID, DeviceID: deviceID}
		if _, exists := seen[ref]; exists {
			return nil, fmt.Errorf("syncer: duplicate v2 Hub descriptor for %q from device %q", hubID, deviceID)
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].HubID != refs[j].HubID {
			return refs[i].HubID < refs[j].HubID
		}
		return refs[i].DeviceID < refs[j].DeviceID
	})
	result := make([]HubMetadataRef, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptor, err := FetchHubDescriptorForDevice(ctx, store, ref.HubID, ref.DeviceID, identities)
		if err != nil {
			return nil, fmt.Errorf("syncer: read v2 Hub %q from device %q: %w", ref.HubID, ref.DeviceID, err)
		}
		result = append(result, HubMetadataRef{Descriptor: descriptor, DeviceID: ref.DeviceID})
	}
	if len(result) == 0 {
		return nil, ErrNoReplicaMetadata
	}
	return result, nil
}

// FetchHubDescriptorForDevice reads a Hub descriptor without requiring a
// child Project, Session, or Replica layout.
func FetchHubDescriptorForDevice(ctx context.Context, store remote.Remote, hubKey, deviceID string, identities []*ecdh.PrivateKey) (sessionhub.HubDescriptor, error) {
	if err := validateIdentifier(hubKey); err != nil {
		return sessionhub.HubDescriptor{}, fmt.Errorf("syncer: Hub key: %w", err)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return sessionhub.HubDescriptor{}, fmt.Errorf("syncer: Hub device key: %w", err)
	}
	key := v2ObjectPrefix + "/" + hubKey + "/descriptors/" + deviceID + "/" + descriptorMetaName
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Hub descriptor")
	if err != nil {
		return sessionhub.HubDescriptor{}, err
	}
	descriptor, err := sessionhub.ParseHubDescriptor(payload)
	if err != nil {
		return sessionhub.HubDescriptor{}, fmt.Errorf("syncer: parse v2 Hub descriptor: %w", err)
	}
	if descriptor.HubID != hubKey {
		return sessionhub.HubDescriptor{}, fmt.Errorf("%w: Hub descriptor belongs to another Hub", ErrReplicaIdentityMismatch)
	}
	return descriptor, nil
}

// FetchHubProjectMetadata lists and authenticates Project descriptors below a
// Hub. It never reads Session descriptors, Replica tips, or any body shard.
func FetchHubProjectMetadata(ctx context.Context, store remote.Remote, hubKey string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]HubProjectMetadataRef, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	if err := validateIdentifier(hubKey); err != nil {
		return nil, fmt.Errorf("syncer: Hub key: %w", err)
	}
	prefix := v2ObjectPrefix + "/" + hubKey + "/projects"
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list Hub Projects: %w", err)
	}
	refs := make([]hubProjectDescriptorRef, 0)
	seen := make(map[hubProjectDescriptorRef]struct{})
	for _, object := range objects {
		projectKey, deviceID, ok := parseHubProjectDescriptorObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[deviceID]; !ok {
				continue
			}
		}
		ref := hubProjectDescriptorRef{ProjectID: projectKey, DeviceID: deviceID}
		if _, exists := seen[ref]; exists {
			return nil, fmt.Errorf("syncer: duplicate Hub Project descriptor for %q from device %q", projectKey, deviceID)
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return nil, ErrNoProjectMetadata
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ProjectID != refs[j].ProjectID {
			return refs[i].ProjectID < refs[j].ProjectID
		}
		return refs[i].DeviceID < refs[j].DeviceID
	})
	result := make([]HubProjectMetadataRef, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		layout, err := NewProjectHubLayout(hubKey, ref.ProjectID)
		if err != nil {
			return nil, err
		}
		descriptor, err := FetchProjectDescriptorForDevice(ctx, store, layout, ref.DeviceID, identities)
		if err != nil {
			return nil, fmt.Errorf("syncer: read Hub Project %q from device %q: %w", ref.ProjectID, ref.DeviceID, err)
		}
		result = append(result, HubProjectMetadataRef{Descriptor: descriptor, DeviceID: ref.DeviceID})
	}
	return result, nil
}

type hubDescriptorRef struct {
	HubID    string
	DeviceID string
}

type hubProjectDescriptorRef struct {
	ProjectID string
	DeviceID  string
}

func parseHubProjectDescriptorObjectKey(prefix, key string) (string, string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")
	if len(parts) != 4 || parts[1] != "descriptors" || parts[3] != descriptorMetaName {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil {
		return "", "", false
	}
	expected := prefix + "/" + parts[0] + "/descriptors/" + parts[2] + "/" + descriptorMetaName
	checked, err := checkedKey(expected)
	if err != nil || checked != key {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func parseHubDescriptorObjectKey(key string) (string, string, bool) {
	if key == "" || !strings.HasPrefix(key, v2ObjectPrefix+"/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, v2ObjectPrefix+"/"), "/")
	if len(parts) != 4 || parts[1] != "descriptors" || parts[3] != descriptorMetaName {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil {
		return "", "", false
	}
	expected := v2ObjectPrefix + "/" + parts[0] + "/descriptors/" + parts[2] + "/" + descriptorMetaName
	checked, err := checkedKey(expected)
	if err != nil || checked != key {
		return "", "", false
	}
	return parts[0], parts[2], true
}
