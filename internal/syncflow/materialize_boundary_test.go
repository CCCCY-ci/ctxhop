package syncflow

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestPlanMaterializedSuffixExcludesImportedPrefixAndChainsAppends(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	imported := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	records := appendMaterializedRecords(imported, []byte(`{"n":3}`), []byte(`{"n":4}`))
	binding := materializedSuffixBinding(t, imported)
	parent := materializedSuffixParent(binding.SessionID, binding.Origin.BaseHeads[0])
	snapshot := materializedSuffixSnapshot(t, binding, records)

	first, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:               binding,
		Snapshot:              snapshot,
		ExistingContributions: []sessionhub.Contribution{parent},
		IdentifierKey:         identifierKey,
	})
	if err != nil {
		t.Fatalf("PlanMaterializedSuffix(first): %v", err)
	}
	if first.Contribution == nil {
		t.Fatal("first suffix plan did not create a Contribution")
	}
	rangeRef := first.Contribution.Ranges[0]
	if rangeRef.StartRecord != 2 || rangeRef.EndRecord != 4 {
		t.Fatalf("first suffix range = [%d,%d), want [2,4)", rangeRef.StartRecord, rangeRef.EndRecord)
	}
	if len(first.Contribution.Parents) != 1 || first.Contribution.Parents[0] != parent.ContributionID {
		t.Fatalf("first parents = %v, want [%s]", first.Contribution.Parents, parent.ContributionID)
	}
	if first.Binding.ReplicaCursor.RecordCount != 4 || first.Binding.ContributionCursor.EndRecord != 4 || first.Binding.ContributionCursor.LastContributionID != first.Contribution.ContributionID {
		t.Fatalf("first binding cursors = %+v / %+v", first.Binding.ReplicaCursor, first.Binding.ContributionCursor)
	}

	recovered, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:               binding,
		Snapshot:              snapshot,
		ExistingContributions: []sessionhub.Contribution{parent, *first.Contribution},
		IdentifierKey:         identifierKey,
	})
	if err != nil {
		t.Fatalf("PlanMaterializedSuffix(recovery): %v", err)
	}
	if recovered.Contribution != nil || !recovered.Recovered || recovered.Binding.ContributionCursor != first.Binding.ContributionCursor {
		t.Fatalf("recovery plan = %+v", recovered)
	}

	appendedRecords := appendMaterializedRecords(records, []byte(`{"n":5}`))
	secondSnapshot := materializedSuffixSnapshot(t, first.Binding, appendedRecords)
	second, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:               first.Binding,
		Snapshot:              secondSnapshot,
		ExistingContributions: []sessionhub.Contribution{parent, *first.Contribution},
		IdentifierKey:         identifierKey,
	})
	if err != nil {
		t.Fatalf("PlanMaterializedSuffix(second): %v", err)
	}
	if second.Contribution == nil {
		t.Fatal("second suffix plan did not create a Contribution")
	}
	secondRange := second.Contribution.Ranges[0]
	if secondRange.StartRecord != 4 || secondRange.EndRecord != 5 {
		t.Fatalf("second suffix range = [%d,%d), want [4,5)", secondRange.StartRecord, secondRange.EndRecord)
	}
	if len(second.Contribution.Parents) != 1 || second.Contribution.Parents[0] != first.Contribution.ContributionID {
		t.Fatalf("second parents = %v, want [%s]", second.Contribution.Parents, first.Contribution.ContributionID)
	}
}

func TestPlanMaterializedSuffixPublishesNoImportedOnlyContribution(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	binding := materializedSuffixBinding(t, records)
	plan, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:       binding,
		Snapshot:      materializedSuffixSnapshot(t, binding, records),
		IdentifierKey: identifierKey,
	})
	if err != nil {
		t.Fatalf("PlanMaterializedSuffix: %v", err)
	}
	if plan.Contribution != nil {
		t.Fatalf("import-only plan created Contribution %+v", plan.Contribution)
	}
	if plan.Binding.ReplicaCursor.RecordCount != 2 || plan.Binding.ContributionCursor.EndRecord != 2 || plan.Binding.ContributionCursor.LastContributionID != "" {
		t.Fatalf("import-only cursors = %+v / %+v", plan.Binding.ReplicaCursor, plan.Binding.ContributionCursor)
	}
	legacy := binding
	legacy.ContributionCursor = sessionhub.ContributionCursor{}
	legacyPlan, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding: legacy, Snapshot: materializedSuffixSnapshot(t, legacy, records), IdentifierKey: identifierKey,
	})
	if err != nil {
		t.Fatalf("PlanMaterializedSuffix(legacy cursor): %v", err)
	}
	if !legacyPlan.Recovered || legacyPlan.Binding.ContributionCursor.EndRecord != 2 {
		t.Fatalf("legacy cursor was not normalized: %+v", legacyPlan)
	}
}

func TestPlanMaterializedSuffixRejectsRewriteMissingHeadsAndConflictingChain(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	imported := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	binding := materializedSuffixBinding(t, imported)
	records := appendMaterializedRecords(imported, []byte(`{"n":3}`))

	_, err := PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:       binding,
		Snapshot:      materializedSuffixSnapshot(t, binding, records),
		IdentifierKey: identifierKey,
	})
	if !errors.Is(err, ErrMaterializeContributionConflict) {
		t.Fatalf("missing head error = %v, want ErrMaterializeContributionConflict", err)
	}

	rewritten := [][]byte{[]byte(`{"n":99}`), imported[1], records[2]}
	_, err = PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:               binding,
		Snapshot:              materializedSuffixSnapshot(t, binding, rewritten),
		ExistingContributions: []sessionhub.Contribution{materializedSuffixParent(binding.SessionID, binding.Origin.BaseHeads[0])},
		IdentifierKey:         identifierKey,
	})
	if !errors.Is(err, ErrMaterializePrefixRewrite) {
		t.Fatalf("rewrite error = %v, want ErrMaterializePrefixRewrite", err)
	}

	parent := materializedSuffixParent(binding.SessionID, binding.Origin.BaseHeads[0])
	conflicting := sessionhub.Contribution{
		Version:        sessionhub.ModelVersion,
		ContributionID: "conflicting",
		SessionID:      binding.SessionID,
		Source: sessionhub.ContributionSource{
			Agent: binding.Agent, ReplicaID: binding.ReplicaID, DeviceID: "device", Generation: 1,
		},
		Parents: []string{parent.ContributionID},
		Ranges: []sessionhub.RangeRef{{
			ReplicaID: binding.ReplicaID, StartRecord: 1, EndRecord: 3,
			PrefixDigest: strings.Repeat("0", 64), RangeDigest: strings.Repeat("1", 64),
		}},
		EnvironmentRefs: []string{},
		CreatedAt:       time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}
	_, err = PlanMaterializedSuffix(MaterializedSuffixRequest{
		Binding:               binding,
		Snapshot:              materializedSuffixSnapshot(t, binding, records),
		ExistingContributions: []sessionhub.Contribution{parent, conflicting},
		IdentifierKey:         identifierKey,
	})
	if !errors.Is(err, ErrMaterializeContributionConflict) {
		t.Fatalf("conflicting chain error = %v, want ErrMaterializeContributionConflict", err)
	}
}

func materializedSuffixBinding(t *testing.T, imported [][]byte) sessionhub.LocalBinding {
	t.Helper()
	digest, err := syncer.DigestRecords(imported)
	if err != nil {
		t.Fatal(err)
	}
	empty := syncer.EmptyDigest()
	return sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           "hub",
		ProjectID:       "project",
		SessionID:       "session",
		Agent:           "codex",
		NativeSessionID: "native",
		ReplicaID:       "replica",
		Generation:      1,
		ReplicaCursor: sessionhub.ReplicaCursor{
			NextShard: 1, HeadDigest: hex.EncodeToString(empty[:]),
		},
		LocalSnapshot: &sessionhub.LocalSessionSnapshot{
			RecordCount: uint64(len(imported)), HeadDigest: hex.EncodeToString(digest[:]),
		},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: uint64(len(imported))},
		Origin: sessionhub.BindingOrigin{
			Kind:      sessionhub.ReplicaOriginLocalMaterialize,
			BaseHeads: []string{"parent"},
			ImportBoundary: &sessionhub.ImportBoundary{
				RecordCount: uint64(len(imported)), PrefixDigest: "sha256:" + hex.EncodeToString(digest[:]),
			},
			Converter: &sessionhub.ConverterProvenance{SourceViewVersion: 1, TargetAdapterVersion: "1"},
		},
	}
}

func materializedSuffixSnapshot(t *testing.T, binding sessionhub.LocalBinding, records [][]byte) syncer.ReplicaSnapshot {
	t.Helper()
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	descriptor := sessionhub.NativeReplicaDescriptor{
		Version: sessionhub.ModelVersion, ReplicaID: binding.ReplicaID, SessionID: binding.SessionID,
		Source: sessionhub.NativeSource{
			Agent: binding.Agent, NativeSessionKey: "nativekey", DeviceID: "device", Generation: binding.Generation, NativeFormat: "codex-jsonl",
		},
		Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginLocalMaterialize, BaseHeads: append([]string(nil), binding.Origin.BaseHeads...)},
		CreatedAt: when,
	}
	tip := sessionhub.ReplicaTip{
		Version: sessionhub.ModelVersion, ReplicaID: binding.ReplicaID, RecordCount: uint64(len(records)), ShardCount: 1, LastShard: 1,
		HeadDigest: hex.EncodeToString(digest[:]), UpdatedAt: when,
	}
	return syncer.ReplicaSnapshot{Descriptor: descriptor, Tip: tip, Records: records, HeadDigest: digest}
}

func materializedSuffixParent(sessionID, contributionID string) sessionhub.Contribution {
	return sessionhub.Contribution{
		Version: sessionhub.ModelVersion, ContributionID: contributionID, SessionID: sessionID,
		Source:  sessionhub.ContributionSource{Agent: "claude-code", ReplicaID: "sourcereplica", DeviceID: "sourcedevice", Generation: 1},
		Parents: []string{},
		Ranges: []sessionhub.RangeRef{{
			ReplicaID: "sourcereplica", StartRecord: 0, EndRecord: 1,
			PrefixDigest: strings.Repeat("0", 64), RangeDigest: strings.Repeat("1", 64),
		}},
		EnvironmentRefs: []string{},
		CreatedAt:       time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC),
	}
}

func appendMaterializedRecords(base [][]byte, extra ...[]byte) [][]byte {
	records := make([][]byte, 0, len(base)+len(extra))
	records = append(records, base...)
	records = append(records, extra...)
	return records
}
