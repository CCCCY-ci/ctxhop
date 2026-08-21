package syncflow

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

var (
	// ErrRestoreCompatibility reports an installation that is not safe to
	// restore without the required compatibility decision.
	ErrRestoreCompatibility = errors.New("syncflow: restore compatibility is insufficient")

	// ErrForkSelectionRequired reports a resolution with multiple incomparable
	// versions and no explicit branch selection.
	ErrForkSelectionRequired = errors.New("syncflow: fork requires explicit version selection")

	// ErrInvalidVersionSelection reports a selected version outside a
	// resolution's bounds.
	ErrInvalidVersionSelection = errors.New("syncflow: invalid restore version selection")

	// ErrInvalidRestoreResolution reports resolution metadata that cannot be
	// trusted as a complete canonical version.
	ErrInvalidRestoreResolution = errors.New("syncflow: invalid restore resolution")

	// ErrRestoreLocalization reports a selected canonical record that could not
	// be transformed into the target path space.
	ErrRestoreLocalization = errors.New("syncflow: restore path localisation failed")
)

// RestoreOptions controls the decisions that can change restore behavior.
//
// VersionIndex is nil for the safe default: a single maximal version is
// selected automatically, while a fork must be selected explicitly. A
// non-nil pointer may select version zero as well; the pointer makes the
// explicit decision distinguishable from an omitted option.
type RestoreOptions struct {
	AllowLimited bool
	VersionIndex *int
}

// RestorePlan is a non-destructive, fully localised restore result.
//
// Both record slices are owned by the plan. Callers may inspect them, but a
// later apply step should treat the plan as immutable after validation.
type RestorePlan struct {
	ResolutionKind      syncer.ResolutionKind
	CommonPrefix        uint64
	VersionIndex        int
	Devices             []string
	HeadDigest          [32]byte
	CanonicalRecords    [][]byte
	LocalizedRecords    [][]byte
	Compatibility       adapter.Compatibility
	CompatibilityReason string
}

// FetchRestorePlan reads, resolves, and localises one remote session.
//
// Preconditions are checked before listing so a stopped installation or an
// incomplete target path cannot cause remote I/O. The function never writes
// local Agent data.
func FetchRestorePlan(ctx context.Context, store remote.Remote, projectID, sessionID string, identity *ecdh.PrivateKey, space adapter.PathSpace, installation adapter.Installation, options RestoreOptions) (RestorePlan, error) {
	return FetchRestorePlanWithIdentitiesAndDevices(ctx, store, projectID, sessionID, []*ecdh.PrivateKey{identity}, nil, space, installation, options)
}

// FetchRestorePlanWithIdentities reads and resolves a session with retained
// content-key generations.
func FetchRestorePlanWithIdentities(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, space adapter.PathSpace, installation adapter.Installation, options RestoreOptions) (RestorePlan, error) {
	return FetchRestorePlanWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, nil, space, installation, options)
}

// FetchRestorePlanWithIdentitiesAndDevices restores only currently authorized
// device branches.
func FetchRestorePlanWithIdentitiesAndDevices(ctx context.Context, store remote.Remote, projectID, sessionID string, identities []*ecdh.PrivateKey, allowed map[string]struct{}, space adapter.PathSpace, installation adapter.Installation, options RestoreOptions) (RestorePlan, error) {
	if ctx == nil {
		return RestorePlan{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return RestorePlan{}, fmt.Errorf("syncflow: fetch restore plan: %w", err)
	}
	if err := validateRestoreRequest(space, installation, options); err != nil {
		return RestorePlan{}, err
	}

	branches, err := syncer.FetchCompleteBranchesWithIdentitiesAndDevices(ctx, store, projectID, sessionID, identities, allowed)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("syncflow: fetch restore branches: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return RestorePlan{}, fmt.Errorf("syncflow: resolve restore branches: %w", err)
	}

	resolution, err := syncer.ResolveBranchesOwned(branches)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("syncflow: resolve restore branches: %w", err)
	}
	branches = nil
	return planRestore(resolution, space, installation, options, false)
}

// PlanRestore validates a resolved session and localises its selected version
// without performing any filesystem or remote operation.
func PlanRestore(resolution syncer.Resolution, space adapter.PathSpace, installation adapter.Installation, options RestoreOptions) (RestorePlan, error) {
	return planRestore(resolution, space, installation, options, true)
}

func planRestore(resolution syncer.Resolution, space adapter.PathSpace, installation adapter.Installation, options RestoreOptions, cloneCanonical bool) (RestorePlan, error) {
	if err := validateRestoreRequest(space, installation, options); err != nil {
		return RestorePlan{}, err
	}
	if err := validateRestoreResolution(resolution); err != nil {
		return RestorePlan{}, err
	}

	index, version, err := selectRestoreVersion(resolution, options)
	if err != nil {
		return RestorePlan{}, err
	}

	canonical := version.Records
	if cloneCanonical {
		canonical = cloneRestoreRecords(canonical)
	}
	localized := make([][]byte, len(canonical))
	for i, record := range canonical {
		local, err := adapter.Localize(record, space)
		if err != nil {
			return RestorePlan{}, fmt.Errorf("%w: record %d: %v", ErrRestoreLocalization, i+1, err)
		}
		localized[i] = append([]byte(nil), local...)
	}

	devices := append([]string(nil), version.Devices...)
	sort.Strings(devices)
	return RestorePlan{
		ResolutionKind:      resolution.Kind,
		CommonPrefix:        resolution.CommonPrefix,
		VersionIndex:        index,
		Devices:             devices,
		HeadDigest:          version.HeadDigest,
		CanonicalRecords:    canonical,
		LocalizedRecords:    localized,
		Compatibility:       installation.Compatibility,
		CompatibilityReason: installation.CompatibilityReason,
	}, nil
}

func validateRestoreRequest(space adapter.PathSpace, installation adapter.Installation, options RestoreOptions) error {
	if strings.TrimSpace(space.ProjectRoot) == "" || strings.TrimSpace(space.AgentHome) == "" {
		return ErrInvalidPathSpace
	}

	switch installation.Compatibility {
	case adapter.CompatFull:
		return nil
	case adapter.CompatLimited:
		if options.AllowLimited {
			return nil
		}
		return restoreCompatibilityError(installation, "limited compatibility requires explicit restore confirmation")
	case adapter.CompatStopped:
		return restoreCompatibilityError(installation, "adapter compatibility policy stopped restore")
	default:
		return restoreCompatibilityError(installation, "agent compatibility has not been classified")
	}
}

func restoreCompatibilityError(installation adapter.Installation, fallback string) error {
	reason := strings.TrimSpace(installation.CompatibilityReason)
	if reason == "" {
		reason = fallback
	}
	return fmt.Errorf("%w: %s", ErrRestoreCompatibility, reason)
}

func validateRestoreResolution(resolution syncer.Resolution) error {
	switch resolution.Kind {
	case syncer.ResolutionConsistent, syncer.ResolutionFastForward, syncer.ResolutionFork:
	default:
		return fmt.Errorf("%w: unknown resolution kind %d", ErrInvalidRestoreResolution, resolution.Kind)
	}
	if len(resolution.Versions) == 0 {
		return fmt.Errorf("%w: no maximal version", ErrInvalidRestoreResolution)
	}
	if resolution.Kind == syncer.ResolutionFork && len(resolution.Versions) < 2 {
		return fmt.Errorf("%w: fork has fewer than two versions", ErrInvalidRestoreResolution)
	}
	if resolution.Kind != syncer.ResolutionFork && len(resolution.Versions) != 1 {
		return fmt.Errorf("%w: non-fork resolution has %d versions", ErrInvalidRestoreResolution, len(resolution.Versions))
	}

	minRecords := uint64(^uint64(0))
	for i, version := range resolution.Versions {
		if err := validateRestoreVersion(version); err != nil {
			return fmt.Errorf("%w: version %d: %v", ErrInvalidRestoreResolution, i, err)
		}
		if count := uint64(len(version.Records)); count < minRecords {
			minRecords = count
		}
	}
	if resolution.CommonPrefix > minRecords {
		return fmt.Errorf("%w: common prefix exceeds a version length", ErrInvalidRestoreResolution)
	}
	return nil
}

func validateRestoreVersion(version syncer.Version) error {
	if len(version.Records) == 0 {
		return errors.New("version has no records")
	}
	if len(version.Devices) == 0 {
		return errors.New("version has no source devices")
	}
	seen := make(map[string]struct{}, len(version.Devices))
	for _, device := range version.Devices {
		if device == "" {
			return errors.New("version has an empty source device")
		}
		if _, exists := seen[device]; exists {
			return errors.New("version has duplicate source devices")
		}
		seen[device] = struct{}{}
	}
	digest, err := syncer.DigestRecords(version.Records)
	if err != nil {
		return fmt.Errorf("validate records: %w", err)
	}
	if digest != version.HeadDigest {
		return errors.New("version head digest does not match its records")
	}
	return nil
}

func selectRestoreVersion(resolution syncer.Resolution, options RestoreOptions) (int, syncer.Version, error) {
	index := 0
	if options.VersionIndex == nil {
		if len(resolution.Versions) != 1 {
			return 0, syncer.Version{}, fmt.Errorf("%w: %d maximal versions are available", ErrForkSelectionRequired, len(resolution.Versions))
		}
	} else {
		index = *options.VersionIndex
		if index < 0 || index >= len(resolution.Versions) {
			return 0, syncer.Version{}, fmt.Errorf("%w: index %d is outside %d versions", ErrInvalidVersionSelection, index, len(resolution.Versions))
		}
	}
	return index, resolution.Versions[index], nil
}

func cloneRestoreRecords(records [][]byte) [][]byte {
	out := make([][]byte, len(records))
	for i, record := range records {
		out[i] = append([]byte(nil), record...)
	}
	return out
}
