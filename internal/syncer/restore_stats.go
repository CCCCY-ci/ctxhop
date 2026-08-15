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
	"time"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

const restoreStatsVersion = 1

var (
	// ErrInvalidRestoreStats reports damaged or internally inconsistent local
	// restore statistics.
	ErrInvalidRestoreStats = errors.New("syncer: invalid restore statistics")

	// ErrUnsupportedRestoreStats reports statistics written by a newer version.
	ErrUnsupportedRestoreStats = errors.New("syncer: restore statistics are newer than this version")
)

// RestoreStats is the content-free local measurement of successful
// cross-device restores.
//
// It deliberately contains no project, session, device, path, or backend
// information. The state never leaves the local configuration directory.
type RestoreStats struct {
	CrossDeviceRestores uint64
	LastRestoredAt      time.Time
}

// Validate checks the timestamp representation used by the local state file.
func (s RestoreStats) Validate() error {
	if s.LastRestoredAt.IsZero() {
		return nil
	}
	encoded := s.LastRestoredAt.UTC().Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, encoded)
	if err != nil || parsed.IsZero() {
		if err == nil {
			err = errors.New("timestamp is zero")
		}
		return fmt.Errorf("%w: last restored time: %v", ErrInvalidRestoreStats, err)
	}
	return nil
}

// RestoreStatsStore persists aggregate restore statistics below one local
// configuration root.
type RestoreStatsStore struct {
	root string
}

// NewRestoreStatsStore validates a local statistics root.
func NewRestoreStatsStore(root string) (RestoreStatsStore, error) {
	if strings.TrimSpace(root) == "" {
		return RestoreStatsStore{}, errors.New("syncer: restore statistics root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return RestoreStatsStore{}, fmt.Errorf("syncer: resolve restore statistics root: %w", err)
	}
	return RestoreStatsStore{root: abs}, nil
}

// Load returns local statistics. An absent file means that no restore has been
// recorded yet and does not create any local state.
func (s RestoreStatsStore) Load(ctx context.Context) (RestoreStats, error) {
	if ctx == nil {
		return RestoreStats{}, errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return RestoreStats{}, err
	}
	if err := ctx.Err(); err != nil {
		return RestoreStats{}, fmt.Errorf("syncer: load restore statistics: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RestoreStats{}, nil
	}
	if err != nil {
		return RestoreStats{}, fmt.Errorf("syncer: read restore statistics: %w", statePathSafe(err))
	}
	if err := ctx.Err(); err != nil {
		return RestoreStats{}, fmt.Errorf("syncer: load restore statistics: %w", err)
	}

	var wire restoreStatsWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return RestoreStats{}, fmt.Errorf("%w: decode state: %v", ErrInvalidRestoreStats, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return RestoreStats{}, fmt.Errorf("%w: state contains trailing JSON", ErrInvalidRestoreStats)
	} else if !errors.Is(err, io.EOF) {
		return RestoreStats{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidRestoreStats, err)
	}
	switch {
	case wire.Version > restoreStatsVersion:
		return RestoreStats{}, fmt.Errorf("%w: version %d", ErrUnsupportedRestoreStats, wire.Version)
	case wire.Version != restoreStatsVersion:
		return RestoreStats{}, fmt.Errorf("%w: version %d", ErrInvalidRestoreStats, wire.Version)
	}

	stats := RestoreStats{CrossDeviceRestores: wire.CrossDeviceRestores}
	if wire.LastRestoredAt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, wire.LastRestoredAt)
		if err != nil || parsed.IsZero() {
			if err == nil {
				err = errors.New("timestamp is zero")
			}
			return RestoreStats{}, fmt.Errorf("%w: last restored time: %v", ErrInvalidRestoreStats, err)
		}
		stats.LastRestoredAt = parsed.UTC()
	}
	if err := stats.Validate(); err != nil {
		return RestoreStats{}, err
	}
	return stats, nil
}

// RecordRestore increments the aggregate only when the selected version has
// at least one source device other than localDeviceID.
//
// sourceDeviceIDs is the complete source-device set for the selected restore
// version. Device IDs are validated but never persisted.
func (s RestoreStatsStore) RecordRestore(ctx context.Context, localDeviceID string, sourceDeviceIDs []string, now time.Time) (RestoreStats, error) {
	if ctx == nil {
		return RestoreStats{}, errors.New("syncer: context is required")
	}
	if err := ctx.Err(); err != nil {
		return RestoreStats{}, fmt.Errorf("syncer: record restore statistics: %w", err)
	}
	if err := validateIdentifier(localDeviceID); err != nil {
		return RestoreStats{}, fmt.Errorf("%w: local device ID: %v", ErrInvalidRestoreStats, err)
	}
	if len(sourceDeviceIDs) == 0 {
		return RestoreStats{}, fmt.Errorf("%w: restore source devices are empty", ErrInvalidRestoreStats)
	}
	foreign := false
	seen := make(map[string]struct{}, len(sourceDeviceIDs))
	for i, deviceID := range sourceDeviceIDs {
		if err := validateIdentifier(deviceID); err != nil {
			return RestoreStats{}, fmt.Errorf("%w: source device %d: %v", ErrInvalidRestoreStats, i+1, err)
		}
		if _, exists := seen[deviceID]; exists {
			continue
		}
		seen[deviceID] = struct{}{}
		if deviceID != localDeviceID {
			foreign = true
		}
	}
	if !foreign {
		return RestoreStats{}, nil
	}
	if now.IsZero() {
		return RestoreStats{}, fmt.Errorf("%w: restore time is zero", ErrInvalidRestoreStats)
	}

	stats, err := s.Load(ctx)
	if err != nil {
		return RestoreStats{}, err
	}
	if stats.CrossDeviceRestores == ^uint64(0) {
		return RestoreStats{}, fmt.Errorf("%w: restore count overflow", ErrInvalidRestoreStats)
	}
	stats.CrossDeviceRestores++
	stats.LastRestoredAt = now.UTC()
	if err := stats.Validate(); err != nil {
		return RestoreStats{}, err
	}
	if err := s.save(ctx, stats); err != nil {
		return RestoreStats{}, err
	}
	return stats, nil
}

func (s RestoreStatsStore) save(ctx context.Context, stats RestoreStats) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if err := stats.Validate(); err != nil {
		return err
	}
	path, err := s.filePath()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: save restore statistics: %w", err)
	}

	wire := restoreStatsWire{
		Version:             restoreStatsVersion,
		CrossDeviceRestores: stats.CrossDeviceRestores,
	}
	if !stats.LastRestoredAt.IsZero() {
		wire.LastRestoredAt = stats.LastRestoredAt.UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("syncer: encode restore statistics: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("syncer: create restore statistics directory: %w", statePathSafe(err))
	}
	if err := atomicfile.Write(path, func(w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write restore statistics: %w", err)
		}
		if written != len(data) {
			return fmt.Errorf("write restore statistics: got %d bytes, expected %d", written, len(data))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: save restore statistics: %w", statePathSafe(err))
	}
	return nil
}

type restoreStatsWire struct {
	Version             int    `json:"version"`
	CrossDeviceRestores uint64 `json:"crossDeviceRestores"`
	LastRestoredAt      string `json:"lastRestoredAt,omitempty"`
}

func (s RestoreStatsStore) filePath() (string, error) {
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("syncer: restore statistics root is required")
	}
	return filepath.Join(s.root, "state", "v1", "stats.json"), nil
}
