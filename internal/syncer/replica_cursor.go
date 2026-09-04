package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

const replicaCursorStateVersion = 1

var (
	// ErrNoReplicaCursor reports that this local writer has not published a
	// v2 Replica prefix yet.
	ErrNoReplicaCursor = errors.New("syncer: v2 Replica cursor does not exist")

	// ErrInvalidReplicaCursorState reports damaged or mismatched local v2
	// cursor contents.
	ErrInvalidReplicaCursorState = errors.New("syncer: invalid v2 Replica cursor state")

	// ErrReplicaCursorCommit reports a remote shard that succeeded but whose
	// local v2 cursor could not be persisted.
	ErrReplicaCursorCommit = errors.New("syncer: v2 Replica cursor commit failed")
)

// ReplicaCursorStore persists one device-owned v2 Replica cursor. It is kept
// separate from CursorStore because v1 and v2 namespaces must not share a
// cursor file or accidentally advance one another.
type ReplicaCursorStore struct {
	root   string
	layout ReplicaLayout
}

// NewReplicaCursorStore validates a local state root and v2 Replica identity.
func NewReplicaCursorStore(root string, layout ReplicaLayout) (ReplicaCursorStore, error) {
	if strings.TrimSpace(root) == "" {
		return ReplicaCursorStore{}, errors.New("syncer: Replica cursor state root is required")
	}
	if err := layout.validate(); err != nil {
		return ReplicaCursorStore{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ReplicaCursorStore{}, fmt.Errorf("syncer: resolve Replica cursor state root: %w", err)
	}
	return ReplicaCursorStore{root: abs, layout: layout}, nil
}

// Load reads and validates the local v2 Replica cursor.
func (s ReplicaCursorStore) Load(ctx context.Context) (PushCursor, error) {
	if ctx == nil {
		return PushCursor{}, errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return PushCursor{}, err
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: load v2 Replica cursor: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PushCursor{}, ErrNoReplicaCursor
	}
	if err != nil {
		return PushCursor{}, fmt.Errorf("syncer: read v2 Replica cursor: %w", statePathSafe(err))
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: load v2 Replica cursor: %w", err)
	}

	var wire replicaCursorWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return PushCursor{}, fmt.Errorf("%w: decode state: %w", ErrInvalidReplicaCursorState, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return PushCursor{}, fmt.Errorf("%w: state contains trailing JSON", ErrInvalidReplicaCursorState)
	} else if !errors.Is(err, io.EOF) {
		return PushCursor{}, fmt.Errorf("%w: state has trailing data: %w", ErrInvalidReplicaCursorState, err)
	}
	if wire.Version != replicaCursorStateVersion {
		if wire.Version > replicaCursorStateVersion {
			return PushCursor{}, fmt.Errorf("%w: version %d", sessionhub.ErrUnsupportedVersion, wire.Version)
		}
		return PushCursor{}, fmt.Errorf("%w: version %d", ErrInvalidReplicaCursorState, wire.Version)
	}
	if wire.ReplicaID != s.layout.replicaKey {
		return PushCursor{}, fmt.Errorf("%w: cursor belongs to another Replica", ErrInvalidReplicaCursorState)
	}
	digest, err := parseDigest(wire.HeadDigest)
	if err != nil {
		return PushCursor{}, fmt.Errorf("%w: head digest: %w", ErrInvalidReplicaCursorState, err)
	}
	cursor := PushCursor{NextShard: wire.NextShard, RecordCount: wire.RecordCount, HeadDigest: digest}
	if err := cursor.Validate(); err != nil {
		return PushCursor{}, fmt.Errorf("%w: %w", ErrInvalidReplicaCursorState, err)
	}
	return cursor, nil
}

// Save atomically commits a v2 Replica cursor after a shard is durable.
func (s ReplicaCursorStore) Save(ctx context.Context, cursor PushCursor) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if err := cursor.Validate(); err != nil {
		return err
	}
	path, err := s.filePath()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: save v2 Replica cursor: %w", err)
	}
	wire := replicaCursorWire{
		Version:     replicaCursorStateVersion,
		ReplicaID:   s.layout.replicaKey,
		NextShard:   cursor.NextShard,
		RecordCount: cursor.RecordCount,
		HeadDigest:  hexDigest(cursor.HeadDigest),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Replica cursor: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("syncer: create v2 Replica cursor directory: %w", statePathSafe(err))
	}
	if err := atomicfile.Write(path, func(w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write v2 Replica cursor: %w", err)
		}
		if written != len(data) {
			return fmt.Errorf("write v2 Replica cursor: got %d bytes, expected %d", written, len(data))
		}
		return ctx.Err()
	}); err != nil {
		return fmt.Errorf("syncer: save v2 Replica cursor: %w", statePathSafe(err))
	}
	return nil
}

// PushReplicaWithCursorStore loads the durable local cursor, publishes one
// source-native Replica and commits cursor progress after every shard. A
// missing cursor starts at the empty prefix.
func PushReplicaWithCursorStore(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, state ReplicaCursorStore, records [][]byte, options ReplicaPushOptions) (ReplicaPushResult, error) {
	if ctx == nil {
		return ReplicaPushResult{}, errors.New("syncer: context is required")
	}
	cursor, err := state.Load(ctx)
	if errors.Is(err, ErrNoReplicaCursor) {
		cursor = NewPushCursor()
	} else if err != nil {
		return ReplicaPushResult{}, err
	}
	return pushReplica(ctx, store, recipient, layout, descriptor, cursor, records, options, func(next PushCursor) error {
		return state.Save(ctx, next)
	})
}

type replicaCursorWire struct {
	Version     int    `json:"version"`
	ReplicaID   string `json:"replicaId"`
	NextShard   uint64 `json:"nextShard"`
	RecordCount uint64 `json:"recordCount"`
	HeadDigest  string `json:"headDigest"`
}

func (s ReplicaCursorStore) filePath() (string, error) {
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("syncer: Replica cursor state root is required")
	}
	if err := s.layout.validate(); err != nil {
		return "", err
	}
	return filepath.Join(
		s.root,
		"state",
		"v2",
		"hubs",
		s.layout.hubKey,
		"projects",
		s.layout.projectKey,
		"sessions",
		s.layout.sessionKey,
		"replicas",
		s.layout.replicaKey,
		s.layout.deviceID,
		"cursor.json",
	), nil
}
