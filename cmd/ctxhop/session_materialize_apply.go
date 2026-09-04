package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

// materializeRequestID is stable for one exact materialization intent. It
// deliberately excludes the generated target ID and the post-commit launch
// request, so a retry can address the same local transaction even after the
// process that first planned it has exited.
func materializeRequestID(hubID, projectID string, options materializeOptions) string {
	heads := append([]string(nil), options.heads...)
	sort.Strings(heads)
	wire := struct {
		HubID            string   `json:"hubId"`
		ProjectID        string   `json:"projectId"`
		SessionID        string   `json:"sessionId"`
		TargetAgent      string   `json:"targetAgent"`
		ContextPolicy    string   `json:"contextPolicy"`
		SourceAgent      string   `json:"sourceAgent"`
		Heads            []string `json:"heads"`
		AllowUnsupported bool     `json:"allowUnsupported"`
		ApplyEnvironment bool     `json:"applyEnvironment"`
	}{
		HubID:            hubID,
		ProjectID:        projectID,
		SessionID:        options.sessionID,
		TargetAgent:      options.targetAgent,
		ContextPolicy:    options.contextPolicy,
		SourceAgent:      options.sourceAgent,
		Heads:            heads,
		AllowUnsupported: options.allowUnsupported,
		ApplyEnvironment: options.applyEnvironment,
	}
	data, _ := json.Marshal(wire)
	digest := sessionhub.DigestBytes(data)
	return hex.EncodeToString(digest[:])
}

// materializeNativeSessionID puts deterministic apply targets in a private
// filename namespace. The ID is still a normal Agent-native ID and is
// validated again by both the adapter and syncflow before writing.
func materializeNativeSessionID(transactionID string) string {
	return "ctxhop-" + transactionID
}

// Materialize encoders use target.CreatedAt for records that do not expose a
// source timestamp (Codex session_meta always has one). A stable fallback is
// required so two recovery attempts produce the same bytes for the same
// transaction. Source item timestamps remain authoritative whenever present.
func materializeStableTargetTime(transactionID string) time.Time {
	digest := sessionhub.DigestBytes([]byte("ctxhop/materialize-created-at\x00" + transactionID))
	var seconds uint64
	for _, value := range digest[:8] {
		seconds = seconds<<8 | uint64(value)
	}
	const yearSeconds = uint64(365 * 24 * 60 * 60)
	return time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds%yearSeconds) * time.Second)
}

func applyMaterializeExecution(ctx context.Context, execution materializeExecution, output io.Writer, jsonOutput bool) error {
	if ctx == nil {
		return errors.New("session switch: context is required")
	}
	if execution.TransactionID == "" {
		return errors.New("session switch: apply transaction identity is unavailable")
	}
	installer, ok := execution.Target.Layout.(syncflow.MaterializeSessionInstaller)
	if !ok {
		return errors.New("session switch: target Agent layout cannot roll back an unverified target")
	}

	previewDigest, err := syncflow.DigestMaterializePreview(execution.Preview)
	if err != nil {
		return fmt.Errorf("session switch: digest preview: %w", err)
	}
	previewDigestText := hex.EncodeToString(previewDigest[:])
	canonicalRecords, err := canonicalizeMaterializeTarget(execution)
	if err != nil {
		return fmt.Errorf("session switch: canonicalize target output: %w", err)
	}
	binding, err := materializeLocalBindingForRecords(execution, canonicalRecords)
	if err != nil {
		return fmt.Errorf("session switch: prepare local binding: %w", err)
	}
	mutationLock, err := acquireLocalMutationLock(ctx, execution.ConfigDir, "session switch")
	if err != nil {
		return err
	}
	defer mutationLock.Close() //nolint:errcheck // the operation result is already determined

	transaction, err := sessionhub.LoadMaterializeTransaction(execution.ConfigDir, execution.HubID, execution.ProjectID, execution.SessionID, execution.TransactionID)
	wasCommitted := false
	switch {
	case errors.Is(err, sessionhub.ErrMaterializeTransactionNotFound):
		now := time.Now().UTC().Round(0)
		transaction = sessionhub.MaterializeTransaction{
			Version:             sessionhub.MaterializeTransactionVersion,
			TransactionID:       execution.TransactionID,
			HubID:               execution.HubID,
			ProjectID:           execution.ProjectID,
			SessionID:           execution.SessionID,
			TargetAgent:         execution.Preview.TargetAgent,
			TargetNativeID:      execution.Preview.TargetNativeID,
			ReplicaID:           binding.ReplicaID,
			ContextPolicy:       execution.Report.ContextPolicy,
			SourceAgent:         execution.Report.SourceAgent,
			SelectedHeads:       append([]string(nil), execution.Preview.SelectedHeads...),
			PreviewDigest:       previewDigestText,
			SelectedRecordCount: execution.Preview.SelectedRecordCount,
			TargetRecordCount:   uint64(len(execution.Preview.EncodedRecords)),
			State:               sessionhub.MaterializeTransactionPrepared,
			CreatedAt:           now,
			UpdatedAt:           now,
		}
		if err := sessionhub.SaveMaterializeTransaction(execution.ConfigDir, transaction); err != nil {
			return fmt.Errorf("session switch: save prepared transaction: %w", err)
		}
	case err != nil:
		return fmt.Errorf("session switch: load transaction: %w", err)
	default:
		wasCommitted = transaction.State == sessionhub.MaterializeTransactionCommitted
		if err := validateMaterializeTransactionForRetry(transaction, execution, binding, previewDigestText); err != nil {
			return fmt.Errorf("session switch: transaction no longer matches the plan: %w", err)
		}
	}

	request := syncflow.MaterializeApplyRequest{
		Installer:   installer,
		Capability:  execution.TargetCapability,
		ProjectRoot: execution.ProjectRoot,
		TargetAgent: execution.Preview.TargetAgent,
		Target: adapter.MaterializeTarget{
			NativeID: execution.Preview.TargetNativeID,
			PathSpace: adapter.PathSpace{
				ProjectRoot: execution.ProjectRoot,
				AgentHome:   execution.Target.Installation.DataDir,
			},
			CreatedAt: materializeStableTargetTime(execution.TransactionID),
		},
		Preview:          execution.Preview,
		Binding:          binding,
		CanonicalRecords: canonicalRecords,
		AllowExisting:    true,
	}
	request.AfterTargetVerified = func() error {
		if transaction.State == sessionhub.MaterializeTransactionCommitted {
			return nil
		}
		updated, err := transaction.Advance(sessionhub.MaterializeTransactionTargetVerified, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := sessionhub.SaveMaterializeTransaction(execution.ConfigDir, updated); err != nil {
			return err
		}
		transaction = updated
		if execution.ApplyEnvironment {
			status, err := applyMaterializeEnvironment(execution)
			if err != nil {
				return err
			}
			execution.Report.EnvironmentStatus = status
		}
		return nil
	}
	request.CommitBinding = func() error {
		return commitMaterializeBinding(execution, binding)
	}

	result, err := syncflow.ApplyMaterialize(ctx, request)
	if err != nil {
		return fmt.Errorf("session switch: %w", err)
	}
	updated, err := transaction.Advance(sessionhub.MaterializeTransactionCommitted, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("session switch: mark transaction committed: %w", err)
	}
	if err := sessionhub.SaveMaterializeTransaction(execution.ConfigDir, updated); err != nil {
		return fmt.Errorf("session switch: mark transaction committed: %w", err)
	}

	report := execution.Report
	report.Preview = false
	report.TransactionID = execution.TransactionID
	report.AlreadyApplied = wasCommitted || result.TargetAlreadyPresent
	switch {
	case wasCommitted:
		report.WriteStatus = "already-committed"
	case result.TargetAlreadyPresent:
		report.WriteStatus = "recovered-existing-target"
	default:
		report.WriteStatus = "created-and-committed"
	}
	if execution.Launch {
		report.LaunchStatus = launchMaterializedSession(execution)
	}
	if jsonOutput {
		return writeMaterializePreviewJSON(output, report)
	}
	return writeMaterializePreviewText(output, report)
}

func validateMaterializeTransactionForRetry(transaction sessionhub.MaterializeTransaction, execution materializeExecution, binding sessionhub.LocalBinding, previewDigest string) error {
	if transaction.TransactionID != execution.TransactionID {
		return errors.New("transaction identity differs")
	}
	if transaction.HubID != execution.HubID || transaction.ProjectID != execution.ProjectID || transaction.SessionID != execution.SessionID {
		return errors.New("transaction Session scope differs")
	}
	if transaction.TargetAgent != execution.Preview.TargetAgent || transaction.TargetNativeID != execution.Preview.TargetNativeID || transaction.ReplicaID != binding.ReplicaID {
		return errors.New("transaction target differs")
	}
	if transaction.ContextPolicy != execution.Report.ContextPolicy || transaction.SourceAgent != execution.Report.SourceAgent {
		return errors.New("transaction context selector differs")
	}
	if !sameMaterializeStringList(transaction.SelectedHeads, execution.Preview.SelectedHeads) {
		return errors.New("transaction selected heads differ")
	}
	if transaction.PreviewDigest != previewDigest || transaction.SelectedRecordCount != execution.Preview.SelectedRecordCount || transaction.TargetRecordCount != uint64(len(execution.Preview.EncodedRecords)) {
		return errors.New("source snapshot or target output digest differs")
	}
	return nil
}

// canonicalizeMaterializeTarget produces the exact byte representation that
// the subsequent native Replica push will publish. The target file itself is
// still written and verified using Preview.EncodedRecords, which must remain
// in the target Agent's native format.
func canonicalizeMaterializeTarget(execution materializeExecution) ([][]byte, error) {
	stream, err := syncflow.CanonicalizeSession(
		adapter.SessionData{Records: execution.Preview.EncodedRecords},
		adapter.PathSpace{
			ProjectRoot: execution.ProjectRoot,
			AgentHome:   execution.Target.Installation.DataDir,
		},
		execution.Target.Installation,
	)
	if err != nil {
		return nil, err
	}
	if len(stream.Records) == 0 {
		return nil, errors.New("canonical target output is empty")
	}
	return stream.Records, nil
}

// materializeLocalBinding retains the legacy raw-record helper for tests and
// callers that only need to derive the target Replica identity. The apply
// path uses materializeLocalBindingForRecords with canonical records so its
// durable boundary matches native Replica publication.
func materializeLocalBinding(execution materializeExecution) (sessionhub.LocalBinding, error) {
	return materializeLocalBindingForRecords(execution, execution.Preview.EncodedRecords)
}

func materializeLocalBindingForRecords(execution materializeExecution, records [][]byte) (sessionhub.LocalBinding, error) {
	if len(execution.Preview.SelectedHeads) == 0 {
		return sessionhub.LocalBinding{}, errors.New("preview has no selected heads")
	}
	if len(records) == 0 {
		return sessionhub.LocalBinding{}, errors.New("target records are empty")
	}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		return sessionhub.LocalBinding{}, fmt.Errorf("digest target records: %w", err)
	}
	hexDigest := hex.EncodeToString(digest[:])
	emptyDigest := syncer.EmptyDigest()
	emptyDigestText := hex.EncodeToString(emptyDigest[:])
	nativeKey, err := sessionhub.DeriveNativeSessionKey(execution.IdentifierKey, execution.Preview.TargetAgent, execution.Preview.TargetNativeID)
	if err != nil {
		return sessionhub.LocalBinding{}, fmt.Errorf("derive target NativeSession key: %w", err)
	}
	replicaID, err := sessionhub.DeriveReplicaKey(execution.IdentifierKey, execution.SessionID, execution.Preview.TargetAgent, nativeKey, execution.LocalDeviceID, 1)
	if err != nil {
		return sessionhub.LocalBinding{}, fmt.Errorf("derive target Replica key: %w", err)
	}
	recordCount := uint64(len(records))
	return sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           execution.HubID,
		ProjectID:       execution.ProjectID,
		SessionID:       execution.SessionID,
		Agent:           execution.Preview.TargetAgent,
		NativeSessionID: execution.Preview.TargetNativeID,
		ReplicaID:       replicaID,
		Generation:      1,
		ReplicaCursor: sessionhub.ReplicaCursor{
			NextShard:   1,
			RecordCount: 0,
			HeadDigest:  emptyDigestText,
		},
		LocalSnapshot: &sessionhub.LocalSessionSnapshot{
			RecordCount: recordCount,
			HeadDigest:  hexDigest,
		},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: recordCount},
		Origin: sessionhub.BindingOrigin{
			Kind:      sessionhub.ReplicaOriginLocalMaterialize,
			BaseHeads: append([]string(nil), execution.Preview.SelectedHeads...),
			ImportBoundary: &sessionhub.ImportBoundary{
				RecordCount:  recordCount,
				PrefixDigest: "sha256:" + hexDigest,
			},
			Converter: &sessionhub.ConverterProvenance{
				SourceViewVersion:    execution.Preview.SourceViewVersion,
				TargetAdapterVersion: execution.Preview.TargetAdapterVersion,
			},
		},
	}, nil
}

func commitMaterializeBinding(execution materializeExecution, binding sessionhub.LocalBinding) error {
	registry, err := loadSessionRegistryForRead(execution.ConfigDir, execution.IdentifierKey, execution.HubID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Round(0)
	hubName := execution.HubName
	if hubName == "" {
		hubName = sessionhub.DefaultHubLogicalID
	}
	projectRecord, err := registry.EnsureProjectInHub(execution.IdentifierKey, hubName, execution.IdentityKind, execution.IdentityValue, now)
	if err != nil {
		return err
	}
	if projectRecord.Descriptor.ProjectID != execution.ProjectID {
		return errors.New("local Session Hub Project does not match the materialization")
	}
	if _, err := registry.EnsureSession(execution.ProjectID, sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: execution.SessionID,
		ProjectID: execution.ProjectID,
		CreatedAt: now,
		CreatedBy: sessionhub.SessionCreator{
			Agent:    execution.Preview.TargetAgent,
			DeviceID: execution.LocalDeviceID,
		},
		Lifecycle: sessionhub.SessionActive,
	}); err != nil {
		return err
	}
	if err := registry.BindNativeSession(execution.ProjectID, execution.SessionID, sessionhub.NativeSessionBinding{
		Agent:           execution.Preview.TargetAgent,
		NativeSessionID: execution.Preview.TargetNativeID,
		BoundAt:         now,
	}); err != nil {
		return err
	}
	if err := sessionhub.SaveRegistry(execution.ConfigDir, registry); err != nil {
		return err
	}
	return sessionhub.SaveLocalBinding(execution.ConfigDir, binding)
}

func sameMaterializeStringList(left, right []string) bool {
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

func projectIdentityKind(kind project.IdentityKind) sessionhub.ProjectIdentityKind {
	if kind == project.KindManual {
		return sessionhub.ProjectIdentityManual
	}
	return sessionhub.ProjectIdentityRemote
}
