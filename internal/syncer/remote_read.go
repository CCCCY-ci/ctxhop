package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

const maxEncryptedShardBytes = maxShardBytes + 1024

var (
	// ErrNoRemoteBranches reports a session prefix that contains no readable
	// shard objects.
	ErrNoRemoteBranches = errors.New("syncer: remote session has no shard branches")

	// ErrIncompleteRemoteSession reports a remote session whose authenticated
	// metadata and visible shard branches do not describe the same complete
	// stream. Callers must retry rather than restore the visible prefix.
	ErrIncompleteRemoteSession = errors.New("syncer: remote session is incomplete")

	// ErrDuplicateShard reports two list entries claiming the same device-local
	// shard sequence.
	ErrDuplicateShard = errors.New("syncer: duplicate remote shard")

	// ErrRemoteObjectTooLarge reports an object that exceeds the syncer's
	// encrypted shard bound before decryption.
	ErrRemoteObjectTooLarge = errors.New("syncer: remote shard is too large")
)

// LegacyReplica is a complete, read-only compatibility view of one v1
// device branch. Metadata and the assembled canonical branch are returned
// together so migration and v2 resume code cannot accidentally pair a body
// from one device with metadata from another device.
//
// The value is a reader result, not a v2 object. It has no v2 Replica ID and
// does not authorize a caller to rewrite the v1 namespace. Callers that need
// a v2 publication must derive a new v2 identity and publish new immutable
// objects.
type LegacyReplica struct {
	LegacySessionID string
	DeviceID        string
	Metadata        Metadata
	Branch          Branch
}

// SessionLayout identifies the remote prefix shared by every device branch of
// one session.
type SessionLayout struct {
	projectID string
	sessionID string
}

// NewSessionLayout validates the identifiers used by a session prefix.
func NewSessionLayout(projectID, sessionID string) (SessionLayout, error) {
	if err := validateIdentifier(projectID); err != nil {
		return SessionLayout{}, fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	if err := validateIdentifier(sessionID); err != nil {
		return SessionLayout{}, fmt.Errorf("syncer: invalid session identifier: %w", err)
	}
	return SessionLayout{projectID: projectID, sessionID: sessionID}, nil
}

// Prefix returns the exact remote prefix containing all device branches.
func (l SessionLayout) Prefix() (string, error) {
	if err := validateIdentifier(l.projectID); err != nil {
		return "", fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	if err := validateIdentifier(l.sessionID); err != nil {
		return "", fmt.Errorf("syncer: invalid session identifier: %w", err)
	}
	return objectPrefix + "/" + l.projectID + "/sessions/" + l.sessionID, nil
}

// FetchBranches lists, decrypts, validates, and assembles every complete
// device branch visible under one remote session prefix.
//
// A remotely listed gap is never treated as an absent suffix. The operation
// fails with ErrIncompleteBranch so callers can retry after eventual
// consistency settles instead of restoring a silently truncated session.
func FetchBranches(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey) ([]Branch, error) {
	return FetchBranchesWithIdentities(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity})
}

// FetchBranchesWithIdentities reads branches encrypted under any retained
// content-key generation.
func FetchBranchesWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey) ([]Branch, error) {
	return FetchBranchesWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, nil)
}

// FetchBranchesWithIdentitiesAndDevices reads branches and optionally filters
// out branches from revoked devices.
func FetchBranchesWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]Branch, error) {
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
		return nil, fmt.Errorf("syncer: list remote session: %w", err)
	}
	refs, err := collectShardRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, ErrNoRemoteBranches
	}

	devices := make([]string, 0, len(refs))
	for device := range refs {
		devices = append(devices, device)
	}
	sort.Strings(devices)

	branches := make([]Branch, 0, len(devices))
	for _, device := range devices {
		if allowed != nil {
			if _, ok := allowed[device]; !ok {
				continue
			}
		}
		parts := refs[device]
		numbers := make([]uint64, 0, len(parts))
		for number := range parts {
			numbers = append(numbers, number)
		}
		sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })

		builder := newBranchBuilder(device)
		for _, number := range numbers {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("syncer: read remote session: %w", err)
			}
			key := parts[number]
			sealed, err := readRemoteShard(ctx, store, key)
			if err != nil {
				return nil, fmt.Errorf("syncer: read remote branch: %w", err)
			}
			shard, err := openShardWithIdentities(identities, key, sealed)
			if err != nil {
				return nil, fmt.Errorf("syncer: open remote branch: %w", err)
			}
			if err := builder.append(number, shard); err != nil {
				return nil, fmt.Errorf("syncer: assemble remote branch: %w", err)
			}
		}

		branch, err := builder.finish()
		if err != nil {
			return nil, fmt.Errorf("syncer: assemble remote branch: %w", err)
		}
		branches = append(branches, branch)
	}
	if len(branches) == 0 {
		return nil, ErrNoRemoteBranches
	}
	return branches, nil
}

// FetchCompleteBranches reads and validates the authenticated metadata and
// every visible shard branch for a session.
//
// A List call can be eventually consistent. FetchBranches can prove that the
// shards it sees are contiguous, but contiguity alone cannot prove that the
// final shard is visible: a stale listing can end at a perfectly valid shard.
// The per-device metadata tip supplies that missing upper bound. If the
// metadata record count or digest does not match the assembled branch, the
// session is incomplete and must not be restored.
func FetchCompleteBranches(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey) ([]Branch, error) {
	return FetchCompleteBranchesWithIdentities(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity})
}

// FetchCompleteBranchesWithIdentities validates metadata and branches using
// every retained content-key generation.
func FetchCompleteBranchesWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey) ([]Branch, error) {
	return FetchCompleteBranchesWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, nil)
}

// FetchCompleteBranchesWithIdentitiesAndDevices validates metadata and branches
// after filtering the current membership set.
func FetchCompleteBranchesWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]Branch, error) {
	replicas, err := FetchCompleteLegacyReplicasWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, allowed)
	if err != nil {
		return nil, err
	}
	branches := make([]Branch, 0, len(replicas))
	for _, replica := range replicas {
		branches = append(branches, replica.Branch)
	}
	return branches, nil
}

// FetchCompleteLegacyReplicas reads and verifies one complete v1 Replica for
// every authorized device branch under a legacy session. It reads metadata and
// shard bodies exactly once per call, and it rejects stale listings, gaps,
// duplicate objects, digest mismatches, and metadata/body disagreement.
func FetchCompleteLegacyReplicas(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey) ([]LegacyReplica, error) {
	return FetchCompleteLegacyReplicasWithIdentities(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity})
}

// FetchCompleteLegacyReplicasWithIdentities reads complete v1 Replicas using
// any retained content-key generation.
func FetchCompleteLegacyReplicasWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey) ([]LegacyReplica, error) {
	return FetchCompleteLegacyReplicasWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, nil)
}

// FetchCompleteLegacyReplicasWithIdentitiesAndDevices is the device-filtered
// compatibility reader used by migration and restore paths.
func FetchCompleteLegacyReplicasWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]LegacyReplica, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}

	metadata, err := FetchMetadataWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, allowed)
	if err != nil {
		return nil, errors.Join(ErrIncompleteRemoteSession, fmt.Errorf("syncer: fetch session metadata: %w", err))
	}
	branches, err := FetchBranchesWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, allowed)
	if err != nil {
		return nil, errors.Join(ErrIncompleteRemoteSession, err)
	}

	expected := make(map[string]Metadata, len(metadata))
	for _, ref := range metadata {
		if _, exists := expected[ref.DeviceID]; exists {
			return nil, fmt.Errorf("%w: duplicate metadata for device %q", ErrIncompleteRemoteSession, ref.DeviceID)
		}
		expected[ref.DeviceID] = ref.Metadata
	}

	actual := make(map[string]Branch, len(branches))
	metadataByDevice := make(map[string]Metadata, len(metadata))
	for _, branch := range branches {
		if _, exists := actual[branch.DeviceID]; exists {
			return nil, fmt.Errorf("%w: duplicate branch for device %q", ErrIncompleteRemoteSession, branch.DeviceID)
		}
		metadata, exists := expected[branch.DeviceID]
		if !exists {
			return nil, fmt.Errorf("%w: device %q has shards but no authenticated metadata", ErrIncompleteRemoteSession, branch.DeviceID)
		}
		if uint64(len(branch.Records)) != metadata.RecordCount || branch.HeadDigest != metadata.HeadDigest {
			return nil, fmt.Errorf("%w: device %q metadata expects %d records with digest %x, visible branch has %d with digest %x", ErrIncompleteRemoteSession, branch.DeviceID, metadata.RecordCount, metadata.HeadDigest, len(branch.Records), branch.HeadDigest)
		}
		actual[branch.DeviceID] = branch
		metadataByDevice[branch.DeviceID] = metadata
	}

	for device := range expected {
		if _, exists := actual[device]; !exists {
			return nil, fmt.Errorf("%w: device %q has metadata but no visible shard branch", ErrIncompleteRemoteSession, device)
		}
	}

	replicas := make([]LegacyReplica, 0, len(branches))
	for _, branch := range branches {
		replicas = append(replicas, LegacyReplica{
			LegacySessionID: sessionID,
			DeviceID:        branch.DeviceID,
			Metadata:        metadataByDevice[branch.DeviceID],
			Branch:          branch,
		})
	}
	return replicas, nil
}

type shardRefs map[string]map[uint64]string

func collectShardRefs(prefix string, objects []remote.ObjectInfo) (shardRefs, error) {
	refs := make(shardRefs)
	for _, object := range objects {
		device, number, ok := parseShardObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		byNumber := refs[device]
		if byNumber == nil {
			byNumber = make(map[uint64]string)
			refs[device] = byNumber
		}
		if existing, exists := byNumber[number]; exists && existing != object.Key {
			return nil, fmt.Errorf("%w for one device sequence", ErrDuplicateShard)
		}
		if _, exists := byNumber[number]; exists {
			return nil, fmt.Errorf("%w for one device sequence", ErrDuplicateShard)
		}
		byNumber[number] = object.Key
	}
	return refs, nil
}

func parseShardObjectKey(prefix, key string) (string, uint64, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return "", 0, false
	}
	remainder := strings.TrimPrefix(key, prefix+"/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || validateIdentifier(parts[0]) != nil {
		return "", 0, false
	}
	number, err := ParseShardNumber(parts[1])
	if err != nil {
		return "", 0, false
	}
	expected, err := checkedKey(prefix + "/" + parts[0] + "/" + fmt.Sprintf("%0*d", shardNameWidth, number))
	if err != nil || expected != key {
		return "", 0, false
	}
	return parts[0], number, true
}

func readRemoteShard(ctx context.Context, store remote.Remote, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("get remote shard: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedShardBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("read remote shard: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("read remote shard: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close remote shard: %w", closeErr)
	}
	if len(data) > maxEncryptedShardBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrRemoteObjectTooLarge, maxEncryptedShardBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read remote shard: %w", err)
	}
	return data, nil
}
