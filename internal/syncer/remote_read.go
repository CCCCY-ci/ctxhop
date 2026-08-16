package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/remote"
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
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if identity == nil {
		return nil, errors.New("syncer: identity key is required")
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
		parts := refs[device]
		numbers := make([]uint64, 0, len(parts))
		for number := range parts {
			numbers = append(numbers, number)
		}
		sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })

		shards := make([]ShardPart, 0, len(numbers))
		for _, number := range numbers {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("syncer: read remote session: %w", err)
			}
			key := parts[number]
			sealed, err := readRemoteShard(ctx, store, key)
			if err != nil {
				return nil, fmt.Errorf("syncer: read remote branch: %w", err)
			}
			shard, err := OpenShard(identity, key, sealed)
			if err != nil {
				return nil, fmt.Errorf("syncer: open remote branch: %w", err)
			}
			shards = append(shards, ShardPart{Number: number, Shard: shard})
		}

		branch, err := AssembleBranch(device, shards)
		if err != nil {
			return nil, fmt.Errorf("syncer: assemble remote branch: %w", err)
		}
		branches = append(branches, branch)
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
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if identity == nil {
		return nil, errors.New("syncer: identity key is required")
	}

	metadata, err := FetchMetadata(ctx, store, projectID, sessionID, identity)
	if err != nil {
		return nil, errors.Join(ErrIncompleteRemoteSession, fmt.Errorf("syncer: fetch session metadata: %w", err))
	}
	branches, err := FetchBranches(ctx, store, projectID, sessionID, identity)
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
	}

	for device := range expected {
		if _, exists := actual[device]; !exists {
			return nil, fmt.Errorf("%w: device %q has metadata but no visible shard branch", ErrIncompleteRemoteSession, device)
		}
	}

	return branches, nil
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
