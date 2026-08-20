package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/environment"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

// ProjectMetadataRef groups the authenticated metadata objects found under
// one opaque remote session identifier.
//
// The session identifier is deliberately kept separate from MetadataRef:
// MetadataRef describes one device branch, while this type is the result of a
// project-level listing. No shard object is read by FetchProjectMetadata.
type ProjectMetadataRef struct {
	SessionID string
	Devices   []MetadataRef
}

// FetchProjectMetadata lists and decrypts metadata for every session in one
// project. It never reads immutable shard bodies, and it never infers a
// session from a shard-only prefix.
//
// The remote listing is collected before any object body is read. This makes
// the set of session/device metadata objects stable for the duration of this
// call and lets the reader reject duplicate metadata entries deterministically.
func FetchProjectMetadata(ctx context.Context, store remote.Remote, projectID string, identity *ecdh.PrivateKey) ([]ProjectMetadataRef, error) {
	return FetchProjectMetadataWithIdentities(ctx, store, projectID, []*ecdh.PrivateKey{identity})
}

// FetchProjectMetadataWithIdentities reads project metadata under any retained
// content-key generation.
func FetchProjectMetadataWithIdentities(ctx context.Context, store remote.Remote, projectID string, identities []*ecdh.PrivateKey) ([]ProjectMetadataRef, error) {
	return FetchProjectMetadataWithIdentitiesAndDevices(ctx, store, projectID, identities, nil)
}

// FetchProjectMetadataWithIdentitiesAndDevices reads project metadata while
// optionally filtering revoked device branches.
func FetchProjectMetadataWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]ProjectMetadataRef, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	if err := validateIdentifier(projectID); err != nil {
		return nil, fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("syncer: list project metadata: %w", err)
	}

	prefix := objectPrefix + "/" + projectID + "/sessions"
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list project metadata: %w", err)
	}
	refs, err := collectProjectMetadataRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, ErrNoRemoteMetadata
	}

	sessionIDs := make([]string, 0, len(refs))
	for sessionID := range refs {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)

	out := make([]ProjectMetadataRef, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		devices := refs[sessionID]
		deviceIDs := make([]string, 0, len(devices))
		for deviceID := range devices {
			deviceIDs = append(deviceIDs, deviceID)
		}
		sort.Strings(deviceIDs)

		metadata := make([]MetadataRef, 0, len(deviceIDs))
		for _, deviceID := range deviceIDs {
			if allowed != nil {
				if _, ok := allowed[deviceID]; !ok {
					continue
				}
			}
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("syncer: read project metadata: %w", err)
			}
			sealed, err := readRemoteMetadata(ctx, store, devices[deviceID])
			if err != nil {
				return nil, fmt.Errorf("syncer: read project metadata: %w", err)
			}
			opened, err := openMetadataWithIdentities(identities, devices[deviceID], sealed)
			if err != nil {
				return nil, fmt.Errorf("syncer: open project metadata: %w", err)
			}
			environmentReferences := []environment.Reference(nil)
			environmentComponents := []environment.Component(nil)
			if environmentMetadata, environmentErr := readEnvironmentMetadata(ctx, store, devices[deviceID], identities); environmentErr == nil {
				environmentReferences = environmentMetadata.References
				environmentComponents = environment.ComponentSummaries(environmentMetadata.Components)
			} else if contextErr := ctx.Err(); contextErr != nil {
				return nil, fmt.Errorf("syncer: read project environment metadata: %w", contextErr)
			}
			metadata = append(metadata, MetadataRef{DeviceID: deviceID, Metadata: opened, Environment: environmentReferences, EnvironmentComponents: environmentComponents})
		}
		if len(metadata) != 0 {
			out = append(out, ProjectMetadataRef{SessionID: sessionID, Devices: metadata})
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRemoteMetadata
	}
	return out, nil
}

func collectProjectMetadataRefs(prefix string, objects []remote.ObjectInfo) (map[string]map[string]string, error) {
	refs := make(map[string]map[string]string)
	for _, object := range objects {
		sessionID, deviceID, ok := parseProjectMetadataObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		devices := refs[sessionID]
		if devices == nil {
			devices = make(map[string]string)
			refs[sessionID] = devices
		}
		if _, exists := devices[deviceID]; exists {
			return nil, fmt.Errorf("%w for one project session and device", ErrDuplicateMetadata)
		}
		devices[deviceID] = object.Key
	}
	return refs, nil
}

func parseProjectMetadataObjectKey(prefix, key string) (string, string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")
	if len(parts) != 3 || parts[2] != metaObjectName {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[1]) != nil {
		return "", "", false
	}
	expected, err := checkedKey(prefix + "/" + parts[0] + "/" + parts[1] + "/" + metaObjectName)
	if err != nil || expected != key {
		return "", "", false
	}
	return parts[0], parts[1], true
}
