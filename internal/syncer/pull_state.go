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
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

const (
	pullTipStateVersion = 1
	maxPullTips         = 4096
)

var (
	// ErrInvalidPullTipState reports damaged or internally inconsistent local
	// observed-tip state.
	ErrInvalidPullTipState = errors.New("syncer: invalid pull tip state")

	// ErrUnsupportedPullTipState reports a state file written by a newer
	// version.
	ErrUnsupportedPullTipState = errors.New("syncer: pull tip state is newer than this version")

	// ErrDuplicatePullTip reports repeated device entries in one state file.
	ErrDuplicatePullTip = errors.New("syncer: duplicate pull tip")
)

// PullTip is the content-free remote progress retained for a foreign device.
type PullTip struct {
	DeviceID    string
	RecordCount uint64
	HeadDigest  [32]byte
}

// NewPullTip validates one observed remote tip.
func NewPullTip(deviceID string, recordCount uint64, headDigest [32]byte) (PullTip, error) {
	tip := PullTip{DeviceID: deviceID, RecordCount: recordCount, HeadDigest: headDigest}
	if err := tip.Validate(); err != nil {
		return PullTip{}, err
	}
	return tip, nil
}

// Validate checks the opaque identity and empty-prefix digest rule.
func (t PullTip) Validate() error {
	if err := validateIdentifier(t.DeviceID); err != nil {
		return fmt.Errorf("%w: device ID: %v", ErrInvalidPullTipState, err)
	}
	if t.RecordCount == 0 && t.HeadDigest != EmptyDigest() {
		return fmt.Errorf("%w: empty tip has a non-empty digest", ErrInvalidPullTipState)
	}
	return nil
}

// PullTipStore persists observed foreign tips for one local device/session.
// The state is advisory: it suppresses repeated body reads, while remote
// metadata remains authoritative when a tip changes.
type PullTipStore struct {
	root   string
	layout ObjectLayout
}

// NewPullTipStore validates a local state root and remote identity tuple.
func NewPullTipStore(root string, layout ObjectLayout) (PullTipStore, error) {
	if strings.TrimSpace(root) == "" {
		return PullTipStore{}, errors.New("syncer: pull tip state root is required")
	}
	if err := layout.validate(); err != nil {
		return PullTipStore{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return PullTipStore{}, fmt.Errorf("syncer: resolve pull tip state root: %w", err)
	}
	return PullTipStore{root: abs, layout: layout}, nil
}

// Load returns observed tips in deterministic device-ID order. An absent file
// means that no foreign tip has been observed yet.
func (s PullTipStore) Load(ctx context.Context) ([]PullTip, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("syncer: load pull tip state: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("syncer: read pull tip state: %w", statePathSafe(err))
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("syncer: load pull tip state: %w", err)
	}

	var wire pullTipStateWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("%w: decode state: %v", ErrInvalidPullTipState, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%w: state contains trailing JSON", ErrInvalidPullTipState)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data: %v", ErrInvalidPullTipState, err)
	}
	if wire.Version > pullTipStateVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedPullTipState, wire.Version)
	}
	if wire.Version != pullTipStateVersion {
		return nil, fmt.Errorf("%w: version %d", ErrInvalidPullTipState, wire.Version)
	}
	if len(wire.Tips) > maxPullTips {
		return nil, fmt.Errorf("%w: too many tips", ErrInvalidPullTipState)
	}

	tips := make([]PullTip, 0, len(wire.Tips))
	seen := make(map[string]struct{}, len(wire.Tips))
	for i, item := range wire.Tips {
		digest, err := parseDigest(item.HeadDigest)
		if err != nil {
			return nil, fmt.Errorf("%w: tip %d digest: %v", ErrInvalidPullTipState, i+1, err)
		}
		tip, err := NewPullTip(item.DeviceID, item.RecordCount, digest)
		if err != nil {
			return nil, fmt.Errorf("%w: tip %d: %v", ErrInvalidPullTipState, i+1, err)
		}
		if _, exists := seen[tip.DeviceID]; exists {
			return nil, fmt.Errorf("%w: device ID appears more than once", ErrDuplicatePullTip)
		}
		seen[tip.DeviceID] = struct{}{}
		tips = append(tips, tip)
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].DeviceID < tips[j].DeviceID })
	return tips, nil
}

// Save atomically replaces observed tips after validation and deterministic
// sorting. The caller's slice is not modified.
func (s PullTipStore) Save(ctx context.Context, tips []PullTip) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	path, err := s.filePath()
	if err != nil {
		return err
	}
	if len(tips) > maxPullTips {
		return fmt.Errorf("%w: too many tips", ErrInvalidPullTipState)
	}
	copyTips := append([]PullTip(nil), tips...)
	sort.Slice(copyTips, func(i, j int) bool { return copyTips[i].DeviceID < copyTips[j].DeviceID })
	for i, tip := range copyTips {
		if err := tip.Validate(); err != nil {
			return fmt.Errorf("%w: tip %d: %v", ErrInvalidPullTipState, i+1, err)
		}
		if i > 0 && copyTips[i-1].DeviceID == tip.DeviceID {
			return fmt.Errorf("%w: device ID appears more than once", ErrDuplicatePullTip)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: save pull tip state: %w", err)
	}

	wire := pullTipStateWire{Version: pullTipStateVersion, Tips: make([]pullTipWire, len(copyTips))}
	for i, tip := range copyTips {
		wire.Tips[i] = pullTipWire{
			DeviceID:    tip.DeviceID,
			RecordCount: tip.RecordCount,
			HeadDigest:  hexDigest(tip.HeadDigest),
		}
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("syncer: encode pull tip state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("syncer: create pull tip state directory: %w", statePathSafe(err))
	}
	if err := atomicfile.Write(path, func(w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if err != nil {
			return fmt.Errorf("write pull tip state: %w", err)
		}
		if written != len(data) {
			return fmt.Errorf("write pull tip state: got %d bytes, expected %d", written, len(data))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("syncer: save pull tip state: %w", statePathSafe(err))
	}
	return nil
}

type pullTipStateWire struct {
	Version int           `json:"version"`
	Tips    []pullTipWire `json:"tips"`
}

type pullTipWire struct {
	DeviceID    string `json:"deviceId"`
	RecordCount uint64 `json:"recordCount"`
	HeadDigest  string `json:"headDigest"`
}

func (s PullTipStore) filePath() (string, error) {
	if strings.TrimSpace(s.root) == "" {
		return "", errors.New("syncer: pull tip state root is required")
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
		"pull.json",
	), nil
}
