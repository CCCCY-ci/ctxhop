package syncflow

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

type restoreApplyWriterFake struct {
	writes     int
	replaces   int
	project    string
	session    string
	records    [][]byte
	writeErr   error
	replaceErr error
}

func (f *restoreApplyWriterFake) WriteSession(projectRoot, sessionID string, records [][]byte) error {
	f.writes++
	f.project, f.session, f.records = projectRoot, sessionID, cloneRestoreRecords(records)
	return f.writeErr
}

func (f *restoreApplyWriterFake) ReplaceSession(projectRoot, sessionID string, records [][]byte) error {
	f.replaces++
	f.project, f.session, f.records = projectRoot, sessionID, cloneRestoreRecords(records)
	return f.replaceErr
}

func TestApplyRestoreChecksWorkspaceBeforeWriting(t *testing.T) {
	fingerprint := &project.Fingerprint{Head: "head", Branch: "main", Files: map[string]string{}}
	cases := []struct {
		name          string
		verdict       project.Verdict
		allowDiverged bool
		wantWrite     bool
		wantErr       error
	}{
		{name: "consistent", verdict: project.Consistent, wantWrite: true},
		{name: "explainable", verdict: project.Explainable, wantWrite: true},
		{name: "divergent refused", verdict: project.Divergent, wantErr: ErrWorkspaceDiverged},
		{name: "divergent approved", verdict: project.Divergent, allowDiverged: true, wantWrite: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &restoreApplyWriterFake{}
			plan := restoreApplyTestPlan(t, adapter.CompatFull)
			result, err := applyRestore(
				context.Background(), writer, "/project", "session-1", plan,
				RestoreApplyOptions{Fingerprint: fingerprint, AllowDivergent: tc.allowDiverged},
				func(context.Context, string, project.Fingerprint) (project.Report, error) {
					return project.Report{Verdict: tc.verdict}, nil
				},
			)
			if tc.wantErr != nil {
				if err == nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("apply error = %v", err)
			}
			if (writer.writes == 1) != tc.wantWrite || writer.replaces != 0 {
				t.Fatalf("writer calls = writes %d, replaces %d", writer.writes, writer.replaces)
			}
			if result.Workspace.Verdict != tc.verdict {
				t.Fatalf("report = %v, want %v", result.Workspace.Verdict, tc.verdict)
			}
		})
	}
}

func TestApplyRestoreSelectsCreateOrReplaceAndPreservesWriteErrors(t *testing.T) {
	fingerprint := &project.Fingerprint{Head: "head", Branch: "main", Files: map[string]string{}}
	compare := consistentRestoreCompare
	plan := restoreApplyTestPlan(t, adapter.CompatFull)

	create := &restoreApplyWriterFake{}
	result, err := applyRestore(context.Background(), create, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare)
	if err != nil {
		t.Fatalf("create apply: %v", err)
	}
	if create.writes != 1 || create.replaces != 0 || result.Replaced || create.project != "/project" || create.session != "session" {
		t.Fatalf("create result = %+v, writer = %+v", result, create)
	}
	if len(create.records) != len(plan.LocalizedRecords) {
		t.Fatalf("written records = %d, want %d", len(create.records), len(plan.LocalizedRecords))
	}

	replace := &restoreApplyWriterFake{}
	result, err = applyRestore(context.Background(), replace, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint, ReplaceExisting: true}, compare)
	if err != nil {
		t.Fatalf("replace apply: %v", err)
	}
	if replace.writes != 0 || replace.replaces != 1 || !result.Replaced {
		t.Fatalf("replace result = %+v, writer = %+v", result, replace)
	}

	writerErr := errors.New("writer failed")
	failed := &restoreApplyWriterFake{writeErr: writerErr}
	result, err = applyRestore(context.Background(), failed, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare)
	if !errors.Is(err, ErrRestoreWrite) || !errors.Is(err, writerErr) || result.Workspace.Verdict != project.Consistent {
		t.Fatalf("write failure = %v, result = %+v", err, result)
	}
}

func TestApplyRestoreRefusesMissingEvidenceAndUnsafeRequests(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatFull)
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	writer := &restoreApplyWriterFake{}
	compare := consistentRestoreCompare

	if _, err := applyRestore(nil, writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare); err == nil {
		t.Fatal("nil context was accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := applyRestore(cancelled, writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if _, err := applyRestore(context.Background(), nil, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare); err == nil {
		t.Fatal("nil writer was accepted")
	}
	if _, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, nil); err == nil {
		t.Fatal("nil comparer was accepted")
	}
	if _, err := applyRestore(context.Background(), writer, " ", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare); !errors.Is(err, ErrInvalidRestoreTarget) {
		t.Fatalf("empty root error = %v", err)
	}
	if _, err := applyRestore(context.Background(), writer, "/project", "../escape", plan, RestoreApplyOptions{Fingerprint: fingerprint}, compare); !errors.Is(err, ErrInvalidRestoreTarget) {
		t.Fatalf("unsafe session error = %v", err)
	}
	if _, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{}, compare); !errors.Is(err, ErrWorkspaceFingerprintRequired) {
		t.Fatalf("missing fingerprint error = %v", err)
	}
	if writer.writes != 0 || writer.replaces != 0 {
		t.Fatal("writer was called for a rejected request")
	}
}

func TestApplyRestoreChecksContextAfterWorkspaceComparison(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatFull)
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	writer := &restoreApplyWriterFake{}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := applyRestore(ctx, writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, func(context.Context, string, project.Fingerprint) (project.Report, error) {
		cancel()
		return project.Report{Verdict: project.Consistent}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-compare cancellation = %v", err)
	}
	if writer.writes != 0 {
		t.Fatal("writer was called after cancellation")
	}
}

func TestApplyRestoreRejectsInvalidPlansAndWorkspaceErrors(t *testing.T) {
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	for name, mutate := range map[string]func(*RestorePlan){
		"record count":     func(plan *RestorePlan) { plan.LocalizedRecords = nil },
		"canonical digest": func(plan *RestorePlan) { plan.CanonicalRecords[0] = []byte(`{"changed":true}`) },
		"localized record": func(plan *RestorePlan) { plan.LocalizedRecords[0] = []byte(`{"ok":true} `) },
		"resolution kind":  func(plan *RestorePlan) { plan.ResolutionKind = 99 },
		"empty device":     func(plan *RestorePlan) { plan.Devices = nil },
	} {
		t.Run(name, func(t *testing.T) {
			plan := restoreApplyTestPlan(t, adapter.CompatFull)
			mutate(&plan)
			writer := &restoreApplyWriterFake{}
			_, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, consistentRestoreCompare)
			if err == nil || !errors.Is(err, ErrInvalidRestorePlan) {
				t.Fatalf("error = %v, want ErrInvalidRestorePlan", err)
			}
			if writer.writes != 0 || writer.replaces != 0 {
				t.Fatal("writer was called for an invalid plan")
			}
		})
	}

	writer := &restoreApplyWriterFake{}
	_, err := applyRestore(context.Background(), writer, "/project", "session", restoreApplyTestPlan(t, adapter.CompatFull), RestoreApplyOptions{Fingerprint: fingerprint}, func(context.Context, string, project.Fingerprint) (project.Report, error) {
		return project.Report{}, errors.New("compare failed")
	})
	if !errors.Is(err, ErrWorkspaceCheck) || writer.writes != 0 {
		t.Fatalf("comparison error = %v, writes = %d", err, writer.writes)
	}

	writer = &restoreApplyWriterFake{}
	_, err = applyRestore(context.Background(), writer, "/project", "session", restoreApplyTestPlan(t, adapter.CompatFull), RestoreApplyOptions{Fingerprint: fingerprint}, func(context.Context, string, project.Fingerprint) (project.Report, error) {
		return project.Report{Verdict: project.Verdict(99)}, nil
	})
	if !errors.Is(err, ErrWorkspaceCheck) || writer.writes != 0 {
		t.Fatalf("unknown verdict error = %v, writes = %d", err, writer.writes)
	}
}

func TestApplyRestoreRequiresLimitedConsent(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatLimited)
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	writer := &restoreApplyWriterFake{}
	if _, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, consistentRestoreCompare); !errors.Is(err, ErrRestoreCompatibility) {
		t.Fatalf("limited refusal = %v", err)
	}
	if writer.writes != 0 {
		t.Fatal("writer was called without limited consent")
	}
	if _, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint, AllowLimited: true}, consistentRestoreCompare); err != nil {
		t.Fatalf("limited consent: %v", err)
	}
}

func restoreApplyTestPlan(t *testing.T, compatibility adapter.Compatibility) RestorePlan {
	t.Helper()
	records := [][]byte{[]byte(`{"ok":true}`)}
	resolution := restoreTestResolution(t, syncer.Branch{DeviceID: "device", Records: records, HeadDigest: mustDigest(t, records)})
	plan, err := PlanRestore(
		resolution,
		adapter.PathSpace{ProjectRoot: "/source/project", AgentHome: "/source/agent"},
		adapter.Installation{Compatibility: compatibility},
		RestoreOptions{AllowLimited: compatibility == adapter.CompatLimited},
	)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	return plan
}

func consistentRestoreCompare(context.Context, string, project.Fingerprint) (project.Report, error) {
	return project.Report{Verdict: project.Consistent}, nil
}
