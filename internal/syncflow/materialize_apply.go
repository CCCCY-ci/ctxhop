package syncflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrMaterializeApply reports a request that is not safe to execute. It is
	// intentionally separate from ErrMaterializePreview so callers can tell a
	// read-only planning failure from a mutation gate failure.
	ErrMaterializeApply = errors.New("syncflow: materialize apply failed")

	// ErrMaterializeTargetConflict reports a target native ID that already
	// exists but cannot be proven to be the exact output of this operation.
	// Materialization never replaces an existing native session.
	ErrMaterializeTargetConflict = errors.New("syncflow: materialize target conflicts with an existing session")

	// ErrMaterializeTargetValidation reports a target session that could not be
	// read back and validated after the atomic install.
	ErrMaterializeTargetValidation = errors.New("syncflow: materialize target validation failed")

	// ErrMaterializeCheckpoint reports a failure to persist the local
	// transaction checkpoint after the target was verified. The target remains
	// installed so a later retry can inspect and complete it.
	ErrMaterializeCheckpoint = errors.New("syncflow: materialize checkpoint failed")

	// ErrMaterializeBindingCommit reports a failure to commit the local
	// Registry/LocalBinding pair after the target was verified. The target is
	// deliberately retained for recovery; no remote object is changed.
	ErrMaterializeBindingCommit = errors.New("syncflow: materialize binding commit failed")
)

// MaterializeSessionInstaller is the deliberately small mutation boundary
// required by materialization. Built-in Agent layouts implement it with an
// atomic create, a complete read, and a delete used only to roll back a newly
// created target that fails post-write verification.
//
// It is kept separate from adapter.SessionLayout so adding the rollback
// capability does not force every read-only/test layout to expose a delete
// operation.
type MaterializeSessionInstaller interface {
	Name() string
	SessionWriter
	ExistingSessionReader
	RemoveSession(projectRoot, sessionID string) error
}

// MaterializeApplyRequest contains an already planned target-native session.
// The request must carry a LocalBinding prepared by the command layer; the
// core verifies that the binding describes exactly the output it is about to
// install before touching the Agent filesystem.
type MaterializeApplyRequest struct {
	Installer   MaterializeSessionInstaller
	Capability  adapter.MaterializeCapability
	ProjectRoot string
	TargetAgent string
	Target      adapter.MaterializeTarget
	Preview     MaterializePreview
	Binding     sessionhub.LocalBinding

	// CanonicalRecords are the same records that a native Replica publisher
	// will derive from Preview.EncodedRecords. The preview remains in target
	// Agent-native bytes for installation and read-back verification; these
	// records are only used to validate the durable materialization boundary.
	CanonicalRecords [][]byte

	// AllowExisting is used only by a recovery retry. An existing target is
	// accepted only when its complete contents are read back, validated by the
	// target capability, and byte-for-byte equal to Preview. It is never a
	// replacement path.
	AllowExisting bool

	// AfterTargetVerified is persisted before CommitBinding. If this callback
	// fails, the target remains installed and the caller can retry from the
	// durable prepared transaction.
	AfterTargetVerified func() error

	// CommitBinding records the local Registry and LocalBinding only after the
	// Agent-native target has been verified. It must be idempotent: retries may
	// invoke it after one of its two local files was already written.
	CommitBinding func() error
}

// MaterializeApplyResult describes which durable boundaries were crossed.
// TargetInstalled is true only when this invocation created the target file;
// TargetAlreadyPresent is true for a verified recovery retry.
type MaterializeApplyResult struct {
	TargetNativeID       string
	TargetRecordCount    uint64
	TargetInstalled      bool
	TargetAlreadyPresent bool
	TargetVerified       bool
	BindingCommitted     bool
}

// ApplyMaterialize executes the local half of a cross-Agent materialization.
// It never reads or writes the Remote and never modifies any source session.
// The only Agent mutation is an atomic create of a new native ID; an existing
// ID is accepted solely for exact recovery and is never replaced.
func ApplyMaterialize(ctx context.Context, request MaterializeApplyRequest) (MaterializeApplyResult, error) {
	if ctx == nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: context is required", ErrMaterializeApply)
	}
	if err := ctx.Err(); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: %w", ErrMaterializeApply, err)
	}
	if request.Installer == nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: session installer is required", ErrMaterializeApply)
	}
	if request.Capability == nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target materialize capability is required", ErrMaterializeApply)
	}
	if request.Installer.Name() != request.TargetAgent {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target Agent does not match the selected layout", ErrMaterializeApply)
	}
	if strings.TrimSpace(request.ProjectRoot) == "" || request.Target.PathSpace.ProjectRoot != request.ProjectRoot {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target project path does not match the apply project", ErrMaterializeApply)
	}
	if err := validateMaterializeAgent(request.TargetAgent, "target"); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: %v", ErrMaterializeApply, err)
	}
	if request.Preview.TargetAgent != request.TargetAgent {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target Agent differs from the preview", ErrMaterializeApply)
	}
	if err := request.Preview.Validate(); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: preview: %v", ErrMaterializeApply, err)
	}
	if request.Preview.TargetNativeID != request.Target.NativeID {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target native ID differs from the preview", ErrMaterializeApply)
	}
	canonicalRecords := request.CanonicalRecords
	if len(canonicalRecords) == 0 {
		// Keep low-level callers and older persisted test fixtures compatible.
		// The command path always supplies the canonical stream explicitly.
		canonicalRecords = request.Preview.EncodedRecords
	}
	if err := validateMaterializeApplyBinding(request.Preview, request.TargetAgent, request.Binding, canonicalRecords); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: binding: %v", ErrMaterializeApply, err)
	}

	records := cloneMaterializeRecords(request.Preview.EncodedRecords)
	if err := request.Capability.ValidateMaterialized(ctx, records, request.Target); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: target preflight: %w", ErrMaterializeApply, err)
	}
	if err := ctx.Err(); err != nil {
		return MaterializeApplyResult{}, fmt.Errorf("%w: %w", ErrMaterializeApply, err)
	}

	result := MaterializeApplyResult{
		TargetNativeID:    request.Preview.TargetNativeID,
		TargetRecordCount: uint64(len(records)),
	}
	writeErr := request.Installer.WriteSession(request.ProjectRoot, request.Preview.TargetNativeID, records)
	switch {
	case writeErr == nil:
		result.TargetInstalled = true
	case errors.Is(writeErr, adapter.ErrSessionExists) && request.AllowExisting:
		result.TargetAlreadyPresent = true
	case errors.Is(writeErr, adapter.ErrSessionExists):
		return result, fmt.Errorf("%w: %w", ErrMaterializeTargetConflict, writeErr)
	default:
		return result, fmt.Errorf("%w: install target session: %w", ErrMaterializeApply, writeErr)
	}

	if err := verifyMaterializeTarget(ctx, request, records); err != nil {
		if result.TargetAlreadyPresent {
			return result, fmt.Errorf("%w: %w", ErrMaterializeTargetConflict, err)
		}
		if result.TargetInstalled {
			if rollbackErr := request.Installer.RemoveSession(request.ProjectRoot, request.Preview.TargetNativeID); rollbackErr != nil && !errors.Is(rollbackErr, os.ErrNotExist) {
				return result, fmt.Errorf("%w: %v; rollback target: %w", ErrMaterializeTargetValidation, err, rollbackErr)
			}
		}
		return result, err
	}
	result.TargetVerified = true

	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrMaterializeApply, err)
	}
	if request.AfterTargetVerified != nil {
		if err := request.AfterTargetVerified(); err != nil {
			return result, fmt.Errorf("%w: %w", ErrMaterializeCheckpoint, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("%w: %w", ErrMaterializeApply, err)
	}
	if request.CommitBinding != nil {
		if err := request.CommitBinding(); err != nil {
			return result, fmt.Errorf("%w: %w", ErrMaterializeBindingCommit, err)
		}
	}
	result.BindingCommitted = request.CommitBinding != nil
	return result, nil
}

func verifyMaterializeTarget(ctx context.Context, request MaterializeApplyRequest, expected [][]byte) error {
	data, err := request.Installer.ReadSession(adapter.SessionRef{
		Agent:       request.TargetAgent,
		NativeID:    request.Preview.TargetNativeID,
		ProjectPath: request.ProjectRoot,
	})
	if err != nil {
		return fmt.Errorf("%w: read installed target: %v", ErrMaterializeTargetValidation, err)
	}
	if data.DroppedTail || data.Skipped != 0 {
		return fmt.Errorf("%w: target contains an incomplete or skipped record", ErrMaterializeTargetValidation)
	}
	if !equalMaterializeRecords(data.Records, expected) {
		return fmt.Errorf("%w: target records differ from the planned output", ErrMaterializeTargetValidation)
	}
	if err := request.Capability.ValidateMaterialized(ctx, data.Records, request.Target); err != nil {
		return fmt.Errorf("%w: target adapter validation: %w", ErrMaterializeTargetValidation, err)
	}
	return nil
}

func validateMaterializeApplyBinding(preview MaterializePreview, targetAgent string, binding sessionhub.LocalBinding, canonicalRecords [][]byte) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if binding.Agent != targetAgent {
		return errors.New("binding Agent differs from the target Agent")
	}
	if binding.NativeSessionID != preview.TargetNativeID {
		return errors.New("binding native session ID differs from the target")
	}
	if len(preview.SelectedHeads) == 0 {
		return errors.New("preview has no selected heads")
	}
	if !equalMaterializeStrings(binding.Origin.BaseHeads, preview.SelectedHeads) {
		return errors.New("binding base heads differ from the preview")
	}
	if binding.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize || binding.Origin.ImportBoundary == nil || binding.Origin.Converter == nil {
		return errors.New("binding is not a complete materialization origin")
	}
	if binding.Origin.Converter.SourceViewVersion != preview.SourceViewVersion {
		return errors.New("binding source view version differs from the preview")
	}
	if binding.Origin.Converter.TargetAdapterVersion != preview.TargetAdapterVersion {
		return errors.New("binding target adapter version differs from the preview")
	}
	recordDigest, err := syncer.DigestRecords(canonicalRecords)
	if err != nil {
		return fmt.Errorf("digest target records: %w", err)
	}
	expectedDigest := "sha256:" + fmt.Sprintf("%x", recordDigest[:])
	boundary := binding.Origin.ImportBoundary
	if boundary.RecordCount != uint64(len(canonicalRecords)) || boundary.PrefixDigest != expectedDigest {
		return errors.New("binding import boundary differs from the target output")
	}
	emptyDigest := syncer.EmptyDigest()
	if binding.ReplicaCursor.RecordCount != 0 || binding.ReplicaCursor.HeadDigest != fmt.Sprintf("%x", emptyDigest[:]) {
		return errors.New("binding Replica cursor must not claim unpublished target records")
	}
	if binding.LocalSnapshot == nil || binding.LocalSnapshot.RecordCount != uint64(len(canonicalRecords)) || binding.LocalSnapshot.HeadDigest != fmt.Sprintf("%x", recordDigest[:]) {
		return errors.New("binding local snapshot differs from the target output")
	}
	if binding.ContributionCursor.EndRecord != boundary.RecordCount || binding.ContributionCursor.LastContributionID != "" {
		return errors.New("binding Contribution cursor must start at the unpublished import boundary")
	}
	return nil
}

func equalMaterializeRecords(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func equalMaterializeStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
