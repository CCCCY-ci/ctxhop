package syncflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

type materializeInstallerStub struct {
	records       [][]byte
	exists        bool
	writeErr      error
	readErr       error
	corruptRead   bool
	writeCalls    int
	readCalls     int
	removeCalls   int
	callbackOrder *[]string
}

func (s *materializeInstallerStub) Name() string { return "codex" }

func (s *materializeInstallerStub) WriteSession(_, _ string, records [][]byte) error {
	s.writeCalls++
	if s.callbackOrder != nil {
		*s.callbackOrder = append(*s.callbackOrder, "write")
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.exists {
		return adapter.ErrSessionExists
	}
	s.records = cloneMaterializeRecords(records)
	s.exists = true
	return nil
}

func (s *materializeInstallerStub) ReplaceSession(string, string, [][]byte) error {
	return errors.New("replace must not be called by materialize apply")
}

func (s *materializeInstallerStub) ReadSession(adapter.SessionRef) (adapter.SessionData, error) {
	s.readCalls++
	if s.callbackOrder != nil {
		*s.callbackOrder = append(*s.callbackOrder, "read")
	}
	if s.readErr != nil {
		return adapter.SessionData{}, s.readErr
	}
	if !s.exists {
		return adapter.SessionData{}, os.ErrNotExist
	}
	records := cloneMaterializeRecords(s.records)
	if s.corruptRead && len(records) != 0 {
		records[0] = []byte(`{"corrupt":true}`)
	}
	return adapter.SessionData{Records: records}, nil
}

func (s *materializeInstallerStub) RemoveSession(_, _ string) error {
	s.removeCalls++
	if s.callbackOrder != nil {
		*s.callbackOrder = append(*s.callbackOrder, "remove")
	}
	s.exists = false
	s.records = nil
	return nil
}

func applyPreviewAndBinding(t *testing.T) (MaterializePreview, sessionhub.LocalBinding, *materializeCapabilityStub) {
	t.Helper()
	selection := testMaterializeSelection()
	selection.SelectedHeads = []string{"head"}
	target := &previewCaptureCapability{materializeCapabilityStub: materializeCapabilityStub{encoded: testEncodedContext()}}
	preview, err := PlanMaterializePreview(context.Background(), selection, testPreviewOptions(target))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := syncer.DigestRecords(preview.EncodedRecords)
	if err != nil {
		t.Fatal(err)
	}
	hexDigest := fmtDigest(digest)
	emptyDigest := syncer.EmptyDigest()
	binding := sessionhub.LocalBinding{
		Version:            sessionhub.ModelVersion,
		HubID:              "hub",
		ProjectID:          "project",
		SessionID:          "session",
		Agent:              "codex",
		NativeSessionID:    preview.TargetNativeID,
		ReplicaID:          "replica",
		Generation:         1,
		ReplicaCursor:      sessionhub.ReplicaCursor{NextShard: 1, HeadDigest: fmtDigest(emptyDigest)},
		LocalSnapshot:      &sessionhub.LocalSessionSnapshot{RecordCount: uint64(len(preview.EncodedRecords)), HeadDigest: hexDigest},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: uint64(len(preview.EncodedRecords))},
		Origin: sessionhub.BindingOrigin{
			Kind:      sessionhub.ReplicaOriginLocalMaterialize,
			BaseHeads: []string{"head"},
			ImportBoundary: &sessionhub.ImportBoundary{
				RecordCount:  uint64(len(preview.EncodedRecords)),
				PrefixDigest: "sha256:" + hexDigest,
			},
			Converter: &sessionhub.ConverterProvenance{
				SourceViewVersion:    preview.SourceViewVersion,
				TargetAdapterVersion: preview.TargetAdapterVersion,
			},
		},
	}
	return preview, binding, &target.materializeCapabilityStub
}

func fmtDigest(digest [32]byte) string {
	return fmt.Sprintf("%x", digest[:])
}

func newApplyRequest(t *testing.T, installer *materializeInstallerStub, capability adapter.MaterializeCapability, preview MaterializePreview, binding sessionhub.LocalBinding) MaterializeApplyRequest {
	t.Helper()
	return MaterializeApplyRequest{
		Installer:   installer,
		Capability:  capability,
		ProjectRoot: `C:\project`,
		TargetAgent: "codex",
		Target: adapter.MaterializeTarget{
			NativeID: preview.TargetNativeID,
			PathSpace: adapter.PathSpace{
				ProjectRoot: `C:\project`,
				AgentHome:   `C:\agent`,
			},
			CreatedAt: time.Unix(100, 0).UTC(),
		},
		Preview: preview,
		Binding: binding,
	}
}

func TestApplyMaterializeInstallsAndCommitsOnlyAfterReadBack(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	var order []string
	installer := &materializeInstallerStub{callbackOrder: &order}
	request := newApplyRequest(t, installer, capability, preview, binding)
	request.AfterTargetVerified = func() error {
		order = append(order, "checkpoint")
		return nil
	}
	request.CommitBinding = func() error {
		order = append(order, "binding")
		return nil
	}

	result, err := ApplyMaterialize(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyMaterialize() error = %v", err)
	}
	if !result.TargetInstalled || !result.TargetVerified || !result.BindingCommitted || result.TargetAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
	if installer.writeCalls != 1 || installer.readCalls != 1 || installer.removeCalls != 0 {
		t.Fatalf("installer calls = write %d read %d remove %d", installer.writeCalls, installer.readCalls, installer.removeCalls)
	}
	wantOrder := []string{"write", "read", "checkpoint", "binding"}
	if !equalMaterializeStrings(order, wantOrder) {
		t.Fatalf("callback order = %v, want %v", order, wantOrder)
	}
}

func TestApplyMaterializeRecoveryAcceptsOnlyExactExistingTarget(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	installer := &materializeInstallerStub{exists: true, records: cloneMaterializeRecords(preview.EncodedRecords)}
	request := newApplyRequest(t, installer, capability, preview, binding)
	request.AllowExisting = true
	called := false
	request.CommitBinding = func() error {
		called = true
		return nil
	}

	result, err := ApplyMaterialize(context.Background(), request)
	if err != nil {
		t.Fatalf("recovery ApplyMaterialize() error = %v", err)
	}
	if !result.TargetAlreadyPresent || result.TargetInstalled || !result.TargetVerified || !called {
		t.Fatalf("recovery result = %+v, called=%t", result, called)
	}

	installer.records[0] = []byte(`{"different":true}`)
	if _, err := ApplyMaterialize(context.Background(), request); !errors.Is(err, ErrMaterializeTargetConflict) {
		t.Fatalf("mismatched recovery error = %v, want target conflict", err)
	}
}

func TestApplyMaterializeRefusesExistingTargetWithoutRecoveryMode(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	installer := &materializeInstallerStub{exists: true, records: cloneMaterializeRecords(preview.EncodedRecords)}
	request := newApplyRequest(t, installer, capability, preview, binding)

	result, err := ApplyMaterialize(context.Background(), request)
	if !errors.Is(err, ErrMaterializeTargetConflict) || result.TargetVerified {
		t.Fatalf("error/result = %v / %+v", err, result)
	}
}

func TestApplyMaterializeRollsBackWhenReadBackValidationFails(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	installer := &materializeInstallerStub{corruptRead: true}
	request := newApplyRequest(t, installer, capability, preview, binding)

	result, err := ApplyMaterialize(context.Background(), request)
	if !errors.Is(err, ErrMaterializeTargetValidation) || !result.TargetInstalled || result.TargetVerified {
		t.Fatalf("error/result = %v / %+v", err, result)
	}
	if installer.removeCalls != 1 || installer.exists {
		t.Fatalf("rollback state = remove %d exists %t", installer.removeCalls, installer.exists)
	}
}

func TestApplyMaterializeRetainsVerifiedTargetWhenBindingCommitFails(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	installer := &materializeInstallerStub{}
	request := newApplyRequest(t, installer, capability, preview, binding)
	request.CommitBinding = func() error { return errors.New("registry unavailable") }

	result, err := ApplyMaterialize(context.Background(), request)
	if !errors.Is(err, ErrMaterializeBindingCommit) || !result.TargetInstalled || !result.TargetVerified || result.BindingCommitted {
		t.Fatalf("error/result = %v / %+v", err, result)
	}
	if !installer.exists || installer.removeCalls != 0 {
		t.Fatalf("verified target was not retained: exists=%t removes=%d", installer.exists, installer.removeCalls)
	}
}

func TestApplyMaterializeValidatesCanonicalBoundaryAndKeepsNativeOutput(t *testing.T) {
	preview, binding, capability := applyPreviewAndBinding(t)
	canonicalRecords := [][]byte{
		[]byte(`{"canonical":"one"}`),
		[]byte(`{"canonical":"two"}`),
	}
	digest, err := syncer.DigestRecords(canonicalRecords)
	if err != nil {
		t.Fatal(err)
	}
	hexDigest := fmtDigest(digest)
	binding.LocalSnapshot = &sessionhub.LocalSessionSnapshot{
		RecordCount: uint64(len(canonicalRecords)),
		HeadDigest:  hexDigest,
	}
	binding.Origin.ImportBoundary = &sessionhub.ImportBoundary{
		RecordCount:  uint64(len(canonicalRecords)),
		PrefixDigest: "sha256:" + hexDigest,
	}
	binding.ContributionCursor = sessionhub.ContributionCursor{EndRecord: uint64(len(canonicalRecords))}

	installer := &materializeInstallerStub{}
	request := newApplyRequest(t, installer, capability, preview, binding)
	request.CanonicalRecords = canonicalRecords
	if _, err := ApplyMaterialize(context.Background(), request); err != nil {
		t.Fatalf("ApplyMaterialize() error = %v", err)
	}
	if !equalMaterializeRecords(installer.records, preview.EncodedRecords) {
		t.Fatalf("installed records = %q, want native preview records %q", installer.records, preview.EncodedRecords)
	}
}
