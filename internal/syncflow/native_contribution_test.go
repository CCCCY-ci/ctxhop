package syncflow

import (
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestPlanNativeContributionKeepsSameAgentRestorePrefixOutOfContribution(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`), []byte(`{"n":3}`)}
	emptyDigest := syncer.EmptyDigest()
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           "hub",
		ProjectID:       "project",
		SessionID:       "session",
		Agent:           "codex",
		NativeSessionID: "native",
		ReplicaID:       "replica",
		Generation:      1,
		ReplicaCursor: sessionhub.ReplicaCursor{
			NextShard:   2,
			RecordCount: uint64(len(records)),
			HeadDigest:  hex.EncodeToString(digest[:]),
		},
		// Records [0,2) were restored from another device. They are present in
		// the new local Replica but must not become this device's Contribution.
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: 2},
		Origin: sessionhub.BindingOrigin{
			Kind:      sessionhub.ReplicaOriginSameAgentRestore,
			BaseHeads: []string{"sourcehead"},
		},
	}
	plan, err := PlanNativeContribution(NativeContributionRequest{
		Binding:       binding,
		DeviceID:      "device",
		Records:       records,
		IdentifierKey: identifierKey,
		Cursor:        binding.ReplicaCursor,
		Now:           time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("PlanNativeContribution: %v", err)
	}
	if plan.Contribution == nil {
		t.Fatal("same-Agent restore did not create the new suffix Contribution")
	}
	if got := plan.Contribution.Ranges[0]; got.StartRecord != 2 || got.EndRecord != 3 {
		t.Fatalf("new Contribution range = [%d,%d), want [2,3)", got.StartRecord, got.EndRecord)
	}
	if !reflect.DeepEqual(plan.Contribution.Parents, []string{"sourcehead"}) {
		t.Fatalf("new Contribution parents = %v, want [source-head]", plan.Contribution.Parents)
	}
	if plan.Binding.ContributionCursor.EndRecord != 3 || plan.Binding.ContributionCursor.LastContributionID == "" {
		t.Fatalf("updated contribution cursor = %+v", plan.Binding.ContributionCursor)
	}
	if plan.Contribution.Ranges[0].PrefixDigest == hex.EncodeToString(emptyDigest[:]) {
		t.Fatal("suffix Contribution used the empty prefix digest")
	}
}

func TestPlanNativeContributionReconcilesSameDeviceRestoreChain(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	prefixDigest, err := syncer.DigestRecords(records[:0])
	if err != nil {
		t.Fatal(err)
	}
	rangeDigest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	root := sessionhub.Contribution{
		Version:   sessionhub.ModelVersion,
		SessionID: "session",
		Source: sessionhub.ContributionSource{
			Agent: "codex", ReplicaID: "replica", DeviceID: "device", Generation: 1,
		},
		Ranges: []sessionhub.RangeRef{{
			ReplicaID: "replica", StartRecord: 0, EndRecord: 2,
			PrefixDigest: hex.EncodeToString(prefixDigest[:]),
			RangeDigest:  hex.EncodeToString(rangeDigest[:]),
		}},
		EnvironmentRefs: []string{},
		CreatedAt:       time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	}
	root, err = root.WithDerivedID(identifierKey)
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionhub.LocalBinding{
		Version:            sessionhub.ModelVersion,
		HubID:              "hub",
		ProjectID:          "project",
		SessionID:          "session",
		Agent:              "codex",
		NativeSessionID:    "native",
		ReplicaID:          "replica",
		Generation:         1,
		ReplicaCursor:      sessionhub.ReplicaCursor{NextShard: 2, RecordCount: 2, HeadDigest: hex.EncodeToString(digest[:])},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: 2},
		Origin:             sessionhub.BindingOrigin{Kind: sessionhub.ReplicaOriginSameAgentRestore, BaseHeads: []string{"sourcehead"}},
	}
	plan, err := PlanNativeContribution(NativeContributionRequest{
		Binding:               binding,
		DeviceID:              "device",
		Records:               records,
		ExistingContributions: []sessionhub.Contribution{root},
		IdentifierKey:         identifierKey,
		Cursor:                binding.ReplicaCursor,
	})
	if err != nil {
		t.Fatalf("PlanNativeContribution with existing chain: %v", err)
	}
	if plan.Contribution != nil {
		t.Fatalf("reconciliation created a duplicate Contribution: %+v", plan.Contribution)
	}
	if plan.Binding.ContributionCursor.LastContributionID != root.ContributionID || plan.Binding.ContributionCursor.EndRecord != 2 {
		t.Fatalf("reconciled cursor = %+v, want contribution %s at record 2", plan.Binding.ContributionCursor, root.ContributionID)
	}
}

func TestPlanNativeContributionRejectsRestoreBoundaryAfterIncompleteTargetChain(t *testing.T) {
	identifierKey := []byte("0123456789abcdef0123456789abcdef")
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	prefixDigest, err := syncer.DigestRecords(records[:0])
	if err != nil {
		t.Fatal(err)
	}
	rangeDigest, err := syncer.DigestRecords(records[:1])
	if err != nil {
		t.Fatal(err)
	}
	partial := sessionhub.Contribution{
		Version:   sessionhub.ModelVersion,
		SessionID: "session",
		Source: sessionhub.ContributionSource{
			Agent: "codex", ReplicaID: "replica", DeviceID: "device", Generation: 1,
		},
		Ranges: []sessionhub.RangeRef{{
			ReplicaID: "replica", StartRecord: 0, EndRecord: 1,
			PrefixDigest: hex.EncodeToString(prefixDigest[:]),
			RangeDigest:  hex.EncodeToString(rangeDigest[:]),
		}},
		EnvironmentRefs: []string{},
		CreatedAt:       time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	}
	partial, err = partial.WithDerivedID(identifierKey)
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionhub.LocalBinding{
		Version:            sessionhub.ModelVersion,
		HubID:              "hub",
		ProjectID:          "project",
		SessionID:          "session",
		Agent:              "codex",
		NativeSessionID:    "native",
		ReplicaID:          "replica",
		Generation:         1,
		ReplicaCursor:      sessionhub.ReplicaCursor{NextShard: 2, RecordCount: 2, HeadDigest: hex.EncodeToString(digest[:])},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: 2},
		Origin:             sessionhub.BindingOrigin{Kind: sessionhub.ReplicaOriginSameAgentRestore, BaseHeads: []string{"sourcehead"}},
	}
	_, err = PlanNativeContribution(NativeContributionRequest{
		Binding:               binding,
		DeviceID:              "device",
		Records:               records,
		ExistingContributions: []sessionhub.Contribution{partial},
		IdentifierKey:         identifierKey,
		Cursor:                binding.ReplicaCursor,
	})
	if !errors.Is(err, ErrNativeContributionConflict) {
		t.Fatalf("incomplete target chain error = %v, want ErrNativeContributionConflict", err)
	}
	if !strings.Contains(err.Error(), "restore boundary") {
		t.Fatalf("incomplete target chain error = %v, want restore boundary detail", err)
	}
}
