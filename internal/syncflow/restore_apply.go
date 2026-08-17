package syncflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

var (
	// ErrWorkspaceFingerprintRequired reports an apply request without the
	// source workspace evidence required by the safety policy.
	ErrWorkspaceFingerprintRequired = errors.New("syncflow: workspace fingerprint is required")

	// ErrWorkspaceDiverged reports a target workspace that does not match the
	// source fingerprint and was not explicitly approved.
	ErrWorkspaceDiverged = errors.New("syncflow: target workspace diverges from the session")

	// ErrWorkspaceContextInjection reports a failure to build the local-only
	// explanation that is appended when the caller enables it for a non-consistent workspace.
	ErrWorkspaceContextInjection = errors.New("syncflow: workspace context injection failed")

	// ErrWorkspaceCheck reports a failure while comparing the target project.
	ErrWorkspaceCheck = errors.New("syncflow: workspace comparison failed")

	// ErrInvalidRestorePlan reports a plan that is not a complete localised
	// record sequence.
	ErrInvalidRestorePlan = errors.New("syncflow: invalid restore plan")

	// ErrInvalidRestoreTarget reports a target that cannot be passed safely to
	// the adapter writer.
	ErrInvalidRestoreTarget = errors.New("syncflow: invalid restore target")

	// ErrRestoreWrite reports an adapter write failure after all checks passed.
	ErrRestoreWrite = errors.New("syncflow: restore write failed")
)

// SessionWriter is the narrow adapter contract needed to install one session.
// adapter.Layout implements it with atomic create and replacement operations.
type SessionWriter interface {
	WriteSession(projectRoot, sessionID string, records [][]byte) error
	ReplaceSession(projectRoot, sessionID string, records [][]byte) error
}

// RestoreApplyOptions contains decisions that may make a restore less
// conservative. All fields default to false.
type RestoreApplyOptions struct {
	// Fingerprint is the source workspace evidence captured when the session
	// was pushed. A nil value is never treated as a clean workspace.
	Fingerprint *project.Fingerprint

	// AllowLimited confirms that the caller accepts a structurally limited
	// compatibility level.
	AllowLimited bool

	// AllowDivergent confirms that the caller accepts a workspace mismatch.
	AllowDivergent bool

	// ReplaceExisting selects the adapter's explicit replacement operation.
	ReplaceExisting bool
	// InjectWorkspaceContext appends a local-only explanation when the caller
	// enables a non-consistent workspace explanation. The marker is filtered from
	// future remote pushes.
	InjectWorkspaceContext bool
}

// RestoreApplyResult reports the workspace decision and whether an existing
// Agent session was replaced. The workspace report is retained on a refused
// divergent apply so callers can show the reason without re-running Git.
type RestoreApplyResult struct {
	Workspace project.Report
	Replaced  bool
	// ContextInjected reports that the restored local session received a
	// local-only workspace difference explanation.
	ContextInjected bool
}

type workspaceComparer func(context.Context, string, project.Fingerprint) (project.Report, error)

// ApplyRestore compares the target workspace and atomically installs a
// localised restore plan when every safety gate passes.
func ApplyRestore(ctx context.Context, writer SessionWriter, projectRoot, sessionID string, plan RestorePlan, options RestoreApplyOptions) (RestoreApplyResult, error) {
	return applyRestore(ctx, writer, projectRoot, sessionID, plan, options, project.Compare)
}

func applyRestore(ctx context.Context, writer SessionWriter, projectRoot, sessionID string, plan RestorePlan, options RestoreApplyOptions, compare workspaceComparer) (RestoreApplyResult, error) {
	if ctx == nil {
		return RestoreApplyResult{}, errors.New("syncflow: context is required")
	}
	if err := ctx.Err(); err != nil {
		return RestoreApplyResult{}, fmt.Errorf("syncflow: apply restore: %w", err)
	}
	if writer == nil {
		return RestoreApplyResult{}, errors.New("syncflow: session writer is required")
	}
	if compare == nil {
		return RestoreApplyResult{}, errors.New("syncflow: workspace comparer is required")
	}
	if strings.TrimSpace(projectRoot) == "" || !safeRestoreSessionID(sessionID) {
		return RestoreApplyResult{}, ErrInvalidRestoreTarget
	}
	if options.Fingerprint == nil {
		return RestoreApplyResult{}, ErrWorkspaceFingerprintRequired
	}
	if err := validateApplyCompatibility(plan, options); err != nil {
		return RestoreApplyResult{}, err
	}
	if err := validateRestorePlan(plan); err != nil {
		return RestoreApplyResult{}, err
	}

	report, err := compare(ctx, projectRoot, *options.Fingerprint)
	if err != nil {
		return RestoreApplyResult{}, fmt.Errorf("%w: %w", ErrWorkspaceCheck, err)
	}
	result := RestoreApplyResult{Workspace: report}
	if err := validateWorkspaceVerdict(report.Verdict); err != nil {
		return result, err
	}
	if report.Verdict == project.Divergent && !options.AllowDivergent {
		return result, fmt.Errorf("%w: workspace verdict is divergent", ErrWorkspaceDiverged)
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("syncflow: apply restore: %w", err)
	}

	localized := plan.LocalizedRecords
	if options.InjectWorkspaceContext && report.Verdict != project.Consistent {
		contextRecord, err := workspaceContextRecordFor(report)
		if err != nil {
			return result, fmt.Errorf("%w: %v", ErrWorkspaceContextInjection, err)
		}
		localized = append(cloneRestoreRecords(plan.LocalizedRecords), contextRecord)
		result.ContextInjected = true
	}

	var writeErr error
	if options.ReplaceExisting {
		writeErr = writer.ReplaceSession(projectRoot, sessionID, localized)
	} else {
		writeErr = writer.WriteSession(projectRoot, sessionID, localized)
	}
	if writeErr != nil {
		return result, fmt.Errorf("%w: %w", ErrRestoreWrite, writeErr)
	}
	result.Replaced = options.ReplaceExisting
	return result, nil
}

func validateApplyCompatibility(plan RestorePlan, options RestoreApplyOptions) error {
	switch plan.Compatibility {
	case adapter.CompatFull:
		return nil
	case adapter.CompatLimited:
		if options.AllowLimited {
			return nil
		}
		return restoreCompatibilityError(adapter.Installation{
			Compatibility:       plan.Compatibility,
			CompatibilityReason: plan.CompatibilityReason,
		}, "limited compatibility requires explicit restore confirmation")
	case adapter.CompatStopped:
		return restoreCompatibilityError(adapter.Installation{
			Compatibility:       plan.Compatibility,
			CompatibilityReason: plan.CompatibilityReason,
		}, "adapter compatibility policy stopped restore")
	default:
		return restoreCompatibilityError(adapter.Installation{
			Compatibility:       plan.Compatibility,
			CompatibilityReason: plan.CompatibilityReason,
		}, "agent compatibility has not been classified")
	}
}

func validateRestorePlan(plan RestorePlan) error {
	if len(plan.CanonicalRecords) == 0 || len(plan.CanonicalRecords) != len(plan.LocalizedRecords) {
		return fmt.Errorf("%w: canonical and localised record counts do not match", ErrInvalidRestorePlan)
	}
	if plan.VersionIndex < 0 {
		return fmt.Errorf("%w: version index is negative", ErrInvalidRestorePlan)
	}
	switch plan.ResolutionKind {
	case syncer.ResolutionConsistent, syncer.ResolutionFastForward, syncer.ResolutionFork:
	default:
		return fmt.Errorf("%w: unknown resolution kind", ErrInvalidRestorePlan)
	}
	if len(plan.Devices) == 0 {
		return fmt.Errorf("%w: source device list is empty", ErrInvalidRestorePlan)
	}
	seen := make(map[string]struct{}, len(plan.Devices))
	for _, device := range plan.Devices {
		if device == "" {
			return fmt.Errorf("%w: source device is empty", ErrInvalidRestorePlan)
		}
		if _, exists := seen[device]; exists {
			return fmt.Errorf("%w: source device is duplicated", ErrInvalidRestorePlan)
		}
		seen[device] = struct{}{}
	}
	digest, err := syncer.DigestRecords(plan.CanonicalRecords)
	if err != nil {
		return fmt.Errorf("%w: canonical records: %v", ErrInvalidRestorePlan, err)
	}
	if digest != plan.HeadDigest {
		return fmt.Errorf("%w: canonical head digest does not match", ErrInvalidRestorePlan)
	}
	for i, record := range plan.LocalizedRecords {
		if err := validateLocalizedRecord(record); err != nil {
			return fmt.Errorf("%w: localised record %d: %v", ErrInvalidRestorePlan, i+1, err)
		}
	}
	return nil
}

func validateLocalizedRecord(record []byte) error {
	if len(record) == 0 || bytes.ContainsAny(record, "\r\n") || !json.Valid(record) {
		return errors.New("record is not a single-line JSON value")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, record); err != nil {
		return errors.New("record could not be compacted")
	}
	if !bytes.Equal(compact.Bytes(), record) {
		return errors.New("record is not compact")
	}
	return nil
}

func validateWorkspaceVerdict(verdict project.Verdict) error {
	switch verdict {
	case project.Consistent, project.Explainable, project.Divergent:
		return nil
	default:
		return fmt.Errorf("%w: unknown workspace verdict", ErrWorkspaceCheck)
	}
}

func safeRestoreSessionID(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}
