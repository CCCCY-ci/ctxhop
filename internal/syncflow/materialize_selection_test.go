package syncflow

import (
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func materializeSelectionContribution(t *testing.T, id string, parents []string, descriptor sessionhub.NativeReplicaDescriptor, records [][]byte, start, end int) sessionhub.Contribution {
	t.Helper()
	prefix, err := syncer.DigestRecords(records[:start])
	if err != nil {
		t.Fatal(err)
	}
	rangeDigest, err := syncer.DigestRecords(records[start:end])
	if err != nil {
		t.Fatal(err)
	}
	contribution := sessionhub.Contribution{
		Version:        sessionhub.ModelVersion,
		ContributionID: id,
		SessionID:      descriptor.SessionID,
		Source: sessionhub.ContributionSource{
			Agent:      descriptor.Source.Agent,
			ReplicaID:  descriptor.ReplicaID,
			DeviceID:   descriptor.Source.DeviceID,
			Generation: descriptor.Source.Generation,
		},
		Parents: parents,
		Ranges: []sessionhub.RangeRef{{
			ReplicaID:    descriptor.ReplicaID,
			StartRecord:  uint64(start),
			EndRecord:    uint64(end),
			PrefixDigest: hex.EncodeToString(prefix[:]),
			RangeDigest:  hex.EncodeToString(rangeDigest[:]),
		}},
		CreatedAt: time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC),
	}
	if err := contribution.Validate(); err != nil {
		t.Fatal(err)
	}
	return contribution
}

func materializeSelectionSnapshot(t *testing.T, records [][]byte, descriptor sessionhub.NativeReplicaDescriptor, layout syncer.ReplicaLayout) syncer.ReplicaSnapshot {
	t.Helper()
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	return syncer.ReplicaSnapshot{
		Layout:     layout,
		Descriptor: descriptor,
		Tip:        syncerTestReplicaTip(t, descriptor.ReplicaID, records, digest),
		Records:    records,
		HeadDigest: digest,
	}
}

func TestPlanMaterializeSelectionVerifiesRangesAndKeepsBranchesSeparate(t *testing.T) {
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	layout := syncerTestReplicaLayout(t, "device")
	descriptor := syncerTestReplicaDescriptor(t, layout, "claude-code")
	root := materializeSelectionContribution(t, "a", nil, descriptor, records, 0, 1)
	child := materializeSelectionContribution(t, "b", []string{"a"}, descriptor, records, 1, 2)
	other := materializeSelectionContribution(t, "c", nil, descriptor, records, 0, 1)
	graph, err := sessionhub.NewGraph(descriptor.SessionID, []sessionhub.Contribution{child, other, root})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	snapshot := materializeSelectionSnapshot(t, records, descriptor, layout)
	selection, err := PlanMaterializeSelection(graph, []string{"b"}, map[string]syncer.ReplicaSnapshot{
		descriptor.ReplicaID: snapshot,
	})
	if err != nil {
		t.Fatalf("PlanMaterializeSelection: %v", err)
	}
	if got, want := selection.Coverage.SelectedIDs, []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected IDs = %v, want %v", got, want)
	}
	if got, want := selection.Coverage.OmittedIDs, []string{"c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("omitted IDs = %v, want %v", got, want)
	}
	if selection.SelectedRecordCount != 2 || len(selection.Ranges) != 2 {
		t.Fatalf("selection coverage = count %d, ranges %d", selection.SelectedRecordCount, len(selection.Ranges))
	}
	if !reflect.DeepEqual(selection.Ranges[0].Records, [][]byte{records[0]}) || !reflect.DeepEqual(selection.Ranges[1].Records, [][]byte{records[1]}) {
		t.Fatalf("selected records = %+v", selection.Ranges)
	}

	records[0][0] = 'X'
	if string(selection.Ranges[0].Records[0]) != `{"n":1}` {
		t.Fatal("selection retained caller-owned Replica buffers")
	}
}

func TestPlanMaterializeSelectionRejectsMissingReplicaDigestAndOverlap(t *testing.T) {
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	layout := syncerTestReplicaLayout(t, "device")
	descriptor := syncerTestReplicaDescriptor(t, layout, "claude-code")
	root := materializeSelectionContribution(t, "a", nil, descriptor, records, 0, 1)
	child := materializeSelectionContribution(t, "b", []string{"a"}, descriptor, records, 1, 2)
	graph, err := sessionhub.NewGraph(descriptor.SessionID, []sessionhub.Contribution{root, child})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if _, err := PlanMaterializeSelection(graph, []string{"b"}, nil); !errors.Is(err, ErrMaterializeReplicaMissing) {
		t.Fatalf("missing Replica error = %v, want ErrMaterializeReplicaMissing", err)
	}

	bad := root
	bad.Ranges = append([]sessionhub.RangeRef(nil), root.Ranges...)
	bad.Ranges[0].RangeDigest = hex.EncodeToString(make([]byte, 32))
	badGraph, err := sessionhub.NewGraph(descriptor.SessionID, []sessionhub.Contribution{bad, child})
	if err != nil {
		t.Fatalf("NewGraph(bad digest): %v", err)
	}
	snapshot := materializeSelectionSnapshot(t, records, descriptor, layout)
	if _, err := PlanMaterializeSelection(badGraph, []string{"b"}, map[string]syncer.ReplicaSnapshot{descriptor.ReplicaID: snapshot}); !errors.Is(err, ErrMaterializeRangeInvalid) {
		t.Fatalf("bad range digest error = %v, want ErrMaterializeRangeInvalid", err)
	}

	overlapping := materializeSelectionContribution(t, "b", []string{"a"}, descriptor, records, 0, 1)
	overlapGraph, err := sessionhub.NewGraph(descriptor.SessionID, []sessionhub.Contribution{root, overlapping})
	if err != nil {
		t.Fatalf("NewGraph(overlap): %v", err)
	}
	if _, err := PlanMaterializeSelection(overlapGraph, []string{"b"}, map[string]syncer.ReplicaSnapshot{descriptor.ReplicaID: snapshot}); !errors.Is(err, ErrMaterializeRangeOverlap) {
		t.Fatalf("overlap error = %v, want ErrMaterializeRangeOverlap", err)
	}
}
