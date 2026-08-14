package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

const cursorStateVersion = 1

var (
	// ErrNoPushCursor reports that no local progress has been established for
	// the project, session, and device tuple.
	ErrNoPushCursor = errors.New("syncer: push cursor does not exist")

	// ErrInvalidCursorState reports damaged or unsupported cursor contents.
	ErrInvalidCursorState = errors.New("syncer: invalid push cursor state")

	// ErrUnsupportedCursorState reports a cursor written by a newer format.
	ErrUnsupportedCursorState = errors.New("syncer: push cursor state is newer than this version")
)

// CursorStore persists one device-local push cursor under an AgentSync
// configuration root.
//
// The root is kept separate from ObjectLayout because the former is a local
// filesystem location while the latter contains only opaque remote IDs.
type CursorStore struct {
	root   string
	layout ObjectLayout
}

// NewCursorStore validates a local state root and remote identity tuple.
func NewCursorStore(root string, layout ObjectLayout) (CursorStore, error) {
	if strings.TrimSpace(root) == "" {
		return CursorStore{}, errors.New("syncer: cursor state root is required")
	}
	if err := layout.validate(); err != nil {
		return CursorStore{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return CursorStore{}, fmt.Errorf("syncer: resolve cursor state root: %w", err)
	}
	return CursorStore{root: abs, layout: layout}, nil
}

// Load reads and strictly validates the local push cursor.
func (s CursorStore) Load(ctx context.Context) (PushCursor, error) {
	if ctx == nil {
		return PushCursor{}, errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return PushCursor{}, err
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: load push cursor: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return PushCursor{}, ErrNoPushCursor
	}
	if err != nil {
		return PushCursor{}, fmt.Errorf("syncer: read push cursor: %w", statePathSafe(err))
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: load push cursor: %w", err)
	}

	var wire cursorStateWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return PushCursor{}, fmt.Errorf("%w: decode state: %v", ErrInvalidCursorState, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return PushCursor{}, fmt.Errorf("%w: state contains trailing JSON", ErrInvalidCursorState)
	} else if !errors.Is(err, io.EOF) {
		return PushCursor{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidCursorState, err)
	}

	switch {
	case wire.Version > cursorStateVersion:
		return PushCursor{}, fmt.Errorf("%w: version %d", ErrUnsupportedCursorState, wire.Version)
	case wire.Version != cursorStateVersion:
		return PushCursor{}, fmt.Errorf("%w: version %d", ErrInvalidCursorState, wire.Version)
	}
	digest, err := parseDigest(wire.HeadDigest)
	if err != nil {
		return PushCursor{}, fmt.Errorf("%w: head digest: %v", ErrInvalidCursorState, err)
	}
	cursor := PushCursor{
		NextShard:   wire.NextShard,
		RecordCount: wire.RecordCount,
		HeadDigest:  digest,
	}
	if err := cursor.Validate(); err != nil {
		return PushCursor{}, fmt.Errorf("%w: %v", ErrInvalidCursorState, err)
	}
	return cursor, nil
}

// Save atomically replaces the local push cursor after validating it.
func (s CursorStore) Save(ctx context.Context, cursor PushCursor) error {
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
		return fmt.Errorf("syncer: save push cursor: %w", err)
	}

	wire := cursorStateWire{
		Version:     cursorStateVersion,
		NextShard:   cursor.NextShard,
		RecordCount: cursor.RecordCount,
		HeadDigest:  hexDigest(cursor.HeadDigest),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("syncer: encode push cursor: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("syncer: create cursor state directory: %w", statePathSafe(err))
	}
	if err := atomicfile.Write(path, func(w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write cursor state: %w", err)
		}
		if written != len(data) {
			return fmt.Errorf("write cursor state: got %d bytes, expected %d", written, len(data))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: save push cursor: %w", statePathSafe(err))
	}
	return nil
}

type cursorStateWire struct {
	Version     int    `json:"version"`
	NextShard   uint64 `json:"nextShard"`
	RecordCount uint64 `json:"recordCount"`
	HeadDigest  string `json:"headDigest"`
}

func (s CursorStore) filePath() (string, error) {
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("syncer: cursor state root is required")
	}
	if err := s.layout.validate(); err != nil {
		return "", err
	}
	return filepath.Join(
		s.root,
		"state",
		"v1",
		"projects",
		s.layout.projectID,
		"sessions",
		s.layout.sessionID,
		s.layout.deviceID,
		"cursor.json",
	), nil
}

func hexDigest(digest [32]byte) string {
	return fmt.Sprintf("%x", digest[:])
}

func statePathSafe(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", pathErr.Op, pathErr.Err)
	}
	return err
}
