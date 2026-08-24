package syncflow

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrInvalidPullRequest reports a malformed local device or observed-tip
	// input before any remote operation begins.
	ErrInvalidPullRequest = errors.New("syncflow: invalid pull request")

	// ErrLocalDeviceStateMismatch reports a remote tip that claims the local
	// device has progress the local cursor cannot account for.
	ErrLocalDeviceStateMismatch = errors.New("syncflow: local device state does not match its cursor")
)

// RemoteTip is the opaque, content-free part of one device's session
// metadata. It is sufficient to decide whether encrypted shard bodies might
// need to be read.
type RemoteTip struct {
	DeviceID    string
	RecordCount uint64
	HeadDigest  [32]byte
}

// PullOptions describes the local source of truth and the last foreign tips a
// caller has already inspected. Observed contains no session records and may
// be persisted as local scheduling state by a higher layer.
type PullOptions struct {
	LocalDeviceID string
	LocalCursor   syncer.PushCursor
	Observed      []RemoteTip
}

// PullPlan is the result of a metadata-only pull check. Foreign contains only
// new or changed non-local tips; it never contains the local device branch.
// The plan does not read or write session files and does not read shard bodies.
type PullPlan struct {
	LocalDeviceID string
	LocalCursor   syncer.PushCursor
	LocalTip      *RemoteTip
	Foreign       []RemoteTip
}

// HasForeignChanges reports whether a caller has a reason to perform an
// explicit body read or show a restore choice to the user.
func (p PullPlan) HasForeignChanges() bool {
	return len(p.Foreign) != 0
}

// ForeignDeviceIDs returns the changed foreign device IDs in deterministic
// order.
func (p PullPlan) ForeignDeviceIDs() []string {
	ids := make([]string, len(p.Foreign))
	for i, tip := range p.Foreign {
		ids[i] = tip.DeviceID
	}
	return ids
}

// PlanPull evaluates remote metadata without reading any session shard.
//
// The local cursor is authoritative for the local device branch. A stale
// local metadata object is tolerated, but a same-length divergent tip or a
// tip ahead of the cursor is rejected rather than treated as a foreign branch.
// This prevents an accidental device-ID collision or lost local state from
// causing an automatic restore.
func PlanPull(refs []syncer.MetadataRef, options PullOptions) (PullPlan, error) {
	if err := validatePullOptions(options); err != nil {
		return PullPlan{}, err
	}
	observed, err := observedTipMap(options.Observed, options.LocalDeviceID)
	if err != nil {
		return PullPlan{}, err
	}

	seen := make(map[string]struct{}, len(refs))
	foreign := make([]RemoteTip, 0, len(refs))
	var localTip *RemoteTip
	for _, ref := range refs {
		if err := config.ValidateDeviceID(ref.DeviceID); err != nil {
			return PullPlan{}, fmt.Errorf("%w: remote device ID: %v", ErrInvalidPullRequest, err)
		}
		if _, exists := seen[ref.DeviceID]; exists {
			return PullPlan{}, fmt.Errorf("%w: duplicate remote device ID", ErrInvalidPullRequest)
		}
		seen[ref.DeviceID] = struct{}{}
		if err := ref.Metadata.Validate(); err != nil {
			return PullPlan{}, fmt.Errorf("%w: metadata for device: %v", ErrInvalidPullRequest, err)
		}

		tip := RemoteTip{
			DeviceID:    ref.DeviceID,
			RecordCount: ref.Metadata.RecordCount,
			HeadDigest:  ref.Metadata.HeadDigest,
		}
		if ref.DeviceID == options.LocalDeviceID {
			if tip.RecordCount > options.LocalCursor.RecordCount ||
				(tip.RecordCount == options.LocalCursor.RecordCount && tip.HeadDigest != options.LocalCursor.HeadDigest) {
				return PullPlan{}, fmt.Errorf("%w: remote local tip is ahead or divergent", ErrLocalDeviceStateMismatch)
			}
			localTip = &tip
			continue
		}

		if previous, exists := observed[tip.DeviceID]; exists {
			if previous == tip || tip.RecordCount < previous.RecordCount {
				continue
			}
		}
		foreign = append(foreign, tip)
	}
	sort.Slice(foreign, func(i, j int) bool { return foreign[i].DeviceID < foreign[j].DeviceID })

	return PullPlan{
		LocalDeviceID: options.LocalDeviceID,
		LocalCursor:   options.LocalCursor,
		LocalTip:      localTip,
		Foreign:       foreign,
	}, nil
}

// FetchPullPlan performs the automatic pull check. It reads only encrypted
// metadata; a session body is read later by an explicit restore operation.
// Missing metadata is a safe no-op because the caller has no authenticated
// remote tip from which to select or restore a session.
func FetchPullPlan(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey, options PullOptions) (PullPlan, error) {
	return FetchPullPlanWithIdentities(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity}, options)
}

// FetchPullPlanWithIdentities performs the metadata-only pull check with a
// retained key history.
func FetchPullPlanWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, options PullOptions) (PullPlan, error) {
	if ctx == nil {
		return PullPlan{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return PullPlan{}, fmt.Errorf("syncflow: check pull metadata: %w", err)
	}
	if err := validatePullOptions(options); err != nil {
		return PullPlan{}, err
	}
	refs, err := syncer.FetchMetadataWithIdentities(ctx, store, projectID, sessionID, identities)
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return PullPlan{LocalDeviceID: options.LocalDeviceID, LocalCursor: options.LocalCursor}, nil
	}
	if err != nil {
		return PullPlan{}, fmt.Errorf("syncflow: fetch pull metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return PullPlan{}, fmt.Errorf("syncflow: plan pull: %w", err)
	}
	return PlanPull(refs, options)
}

func validatePullOptions(options PullOptions) error {
	if err := config.ValidateDeviceID(options.LocalDeviceID); err != nil {
		return fmt.Errorf("%w: local device ID: %v", ErrInvalidPullRequest, err)
	}
	if err := options.LocalCursor.Validate(); err != nil {
		return fmt.Errorf("%w: local cursor: %v", ErrInvalidPullRequest, err)
	}
	return nil
}

func observedTipMap(tips []RemoteTip, localDeviceID string) (map[string]RemoteTip, error) {
	seen := make(map[string]RemoteTip, len(tips))
	for _, tip := range tips {
		if err := validateRemoteTip(tip); err != nil {
			return nil, fmt.Errorf("%w: observed tip: %v", ErrInvalidPullRequest, err)
		}
		if tip.DeviceID == localDeviceID {
			return nil, fmt.Errorf("%w: local device cannot be an observed foreign tip", ErrInvalidPullRequest)
		}
		if _, exists := seen[tip.DeviceID]; exists {
			return nil, fmt.Errorf("%w: duplicate observed device ID", ErrInvalidPullRequest)
		}
		seen[tip.DeviceID] = tip
	}
	return seen, nil
}

func validateRemoteTip(tip RemoteTip) error {
	if err := config.ValidateDeviceID(tip.DeviceID); err != nil {
		return err
	}
	if tip.RecordCount == 0 && tip.HeadDigest != syncer.EmptyDigest() {
		return errors.New("empty tip has a non-empty digest")
	}
	return nil
}
