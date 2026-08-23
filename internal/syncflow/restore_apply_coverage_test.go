package syncflow

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/project"
)

func TestApplyRestorePublicWrapperAndCompatibilityStops(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatFull)
	writer := &restoreApplyWriterFake{}
	_, err := ApplyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{})
	if !errors.Is(err, ErrWorkspaceFingerprintRequired) {
		t.Fatalf("public wrapper error = %v, want fingerprint sentinel", err)
	}

	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	for _, compatibility := range []adapter.Compatibility{adapter.CompatStopped, adapter.Compatibility(99)} {
		testPlan := plan
		testPlan.Compatibility = compatibility
		writer := &restoreApplyWriterFake{}
		_, err := applyRestore(context.Background(), writer, "/project", "session", testPlan, RestoreApplyOptions{Fingerprint: fingerprint}, consistentRestoreCompare)
		if !errors.Is(err, ErrRestoreCompatibility) {
			t.Fatalf("compatibility %d error = %v", compatibility, err)
		}
		if writer.writes != 0 || writer.replaces != 0 {
			t.Fatalf("compatibility %d called writer", compatibility)
		}
	}
}

func TestApplyRestoreValidatesEveryRecordAndSourceDeviceBoundary(t *testing.T) {
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	cases := map[string]func(*RestorePlan){
		"empty canonical": func(plan *RestorePlan) {
			plan.CanonicalRecords = nil
			plan.LocalizedRecords = nil
		},
		"negative index": func(plan *RestorePlan) { plan.VersionIndex = -1 },
		"empty device":   func(plan *RestorePlan) { plan.Devices = []string{""} },
		"duplicate device": func(plan *RestorePlan) {
			plan.Devices = []string{"device", "device"}
		},
		"invalid canonical": func(plan *RestorePlan) {
			plan.CanonicalRecords[0] = []byte("not json")
		},
		"empty localized":   func(plan *RestorePlan) { plan.LocalizedRecords[0] = nil },
		"newline localized": func(plan *RestorePlan) { plan.LocalizedRecords[0] = []byte("{\"ok\":true}\n") },
		"invalid localized": func(plan *RestorePlan) { plan.LocalizedRecords[0] = []byte("not json") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			plan := restoreApplyTestPlan(t, adapter.CompatFull)
			mutate(&plan)
			writer := &restoreApplyWriterFake{}
			_, err := applyRestore(context.Background(), writer, "/project", "session", plan, RestoreApplyOptions{Fingerprint: fingerprint}, consistentRestoreCompare)
			if err == nil || !errors.Is(err, ErrInvalidRestorePlan) {
				t.Fatalf("error = %v, want ErrInvalidRestorePlan", err)
			}
			if writer.writes != 0 || writer.replaces != 0 {
				t.Fatal("writer was called for invalid plan")
			}
		})
	}
}

func TestApplyRestoreRejectsUnsafeSessionIdentifierCharacters(t *testing.T) {
	plan := restoreApplyTestPlan(t, adapter.CompatFull)
	fingerprint := &project.Fingerprint{Head: "head", Files: map[string]string{}}
	for _, sessionID := range []string{"has space", "quote\"mark", "sub/dir", `sub\dir`} {
		writer := &restoreApplyWriterFake{}
		_, err := applyRestore(context.Background(), writer, "/project", sessionID, plan, RestoreApplyOptions{Fingerprint: fingerprint}, consistentRestoreCompare)
		if !errors.Is(err, ErrInvalidRestoreTarget) {
			t.Errorf("session %q error = %v, want ErrInvalidRestoreTarget", sessionID, err)
		}
	}
}
