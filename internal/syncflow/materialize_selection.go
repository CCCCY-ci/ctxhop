package syncflow

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrMaterializeSelection reports a causal selection that cannot be used
	// as a materialize source snapshot.
	ErrMaterializeSelection = errors.New("syncflow: invalid materialize selection")

	// ErrMaterializeReplicaMissing reports a selected Contribution whose
	// verified Replica body was not supplied to the pure selection function.
	ErrMaterializeReplicaMissing = errors.New("syncflow: materialize source Replica is missing")

	// ErrMaterializeRangeInvalid reports a range outside a verified Replica or
	// a range whose authenticated digests do not match its records.
	ErrMaterializeRangeInvalid = errors.New("syncflow: materialize source range is invalid")

	// ErrMaterializeRangeOverlap reports two selected Contributions that would
	// duplicate or reorder the same source Replica records.
	ErrMaterializeRangeOverlap = errors.New("syncflow: selected materialize ranges overlap")
)

// MaterializeRange is one verified, source-native canonical range selected
// from a Contribution ancestry. Records are copied from the supplied
// ReplicaSnapshot and are safe for a later local adapter decode.
type MaterializeRange struct {
	ContributionID string
	SourceAgent    string
	ReplicaID      string
	StartRecord    uint64
	EndRecord      uint64
	Records        [][]byte
}

// MaterializeSelection is a read-only causal source selection. It keeps
// ranges separate instead of flattening different Agent formats into one
// byte stream; a later multi-source planner can decode each range with its
// owning source capability and combine only the transient views.
type MaterializeSelection struct {
	Coverage sessionhub.Coverage
	// SelectedHeads is the explicit policy result used to produce Coverage.
	// Keeping it beside the selected ranges lets a caller explain an
	// all-heads or agent-only selection without reconstructing policy from the
	// graph after the remote snapshot has been consumed.
	SelectedHeads       []string
	Ranges              []MaterializeRange
	SelectedRecordCount uint64
}

// PlanMaterializeSelection selects Contribution ancestry and verifies every
// selected range against an already complete ReplicaSnapshot. It performs no
// remote or filesystem I/O and never modifies the graph, snapshots, or Agent
// records. A missing parent/head, missing Replica, bad digest, or overlap is
// a hard failure; callers must not present a partial result as materializable.
func PlanMaterializeSelection(graph *sessionhub.Graph, heads []string, replicas map[string]syncer.ReplicaSnapshot) (MaterializeSelection, error) {
	if graph == nil {
		return MaterializeSelection{}, fmt.Errorf("%w: graph is nil", ErrMaterializeSelection)
	}
	coverage, err := graph.Select(heads...)
	if err != nil {
		return MaterializeSelection{}, fmt.Errorf("%w: select contribution ancestry: %w", ErrMaterializeSelection, err)
	}

	validated := make(map[string]struct{})
	intervals := make([]materializeRangeInterval, 0)
	ranges := make([]MaterializeRange, 0)
	var selectedRecordCount uint64
	for _, contributionID := range coverage.SelectedIDs {
		contribution, ok := graph.Contribution(contributionID)
		if !ok {
			return MaterializeSelection{}, fmt.Errorf("%w: selected Contribution is unavailable", ErrMaterializeSelection)
		}
		replicaID := contribution.Source.ReplicaID
		snapshot, ok := replicas[replicaID]
		if !ok {
			return MaterializeSelection{}, fmt.Errorf("%w: Contribution source is unavailable", ErrMaterializeReplicaMissing)
		}
		if _, done := validated[replicaID]; !done {
			if err := validateMaterializeReplicaSnapshot(snapshot, graph.SessionID(), replicaID); err != nil {
				return MaterializeSelection{}, err
			}
			validated[replicaID] = struct{}{}
		}
		if contribution.Source.Agent != snapshot.Descriptor.Source.Agent ||
			contribution.Source.DeviceID != snapshot.Descriptor.Source.DeviceID ||
			contribution.Source.Generation != snapshot.Descriptor.Source.Generation {
			return MaterializeSelection{}, fmt.Errorf("%w: Contribution source does not match its Replica", ErrMaterializeSelection)
		}

		contributionRanges := append([]sessionhub.RangeRef(nil), contribution.Ranges...)
		sort.Slice(contributionRanges, func(i, j int) bool {
			if contributionRanges[i].StartRecord != contributionRanges[j].StartRecord {
				return contributionRanges[i].StartRecord < contributionRanges[j].StartRecord
			}
			return contributionRanges[i].EndRecord < contributionRanges[j].EndRecord
		})
		for _, sourceRange := range contributionRanges {
			if err := validateMaterializeRange(snapshot, sourceRange); err != nil {
				return MaterializeSelection{}, fmt.Errorf("%w: Contribution range validation: %w", ErrMaterializeRangeInvalid, err)
			}
			intervals = append(intervals, materializeRangeInterval{
				ReplicaID:      sourceRange.ReplicaID,
				StartRecord:    sourceRange.StartRecord,
				EndRecord:      sourceRange.EndRecord,
				ContributionID: contribution.ContributionID,
			})
			length := sourceRange.EndRecord - sourceRange.StartRecord
			if ^uint64(0)-selectedRecordCount < length {
				return MaterializeSelection{}, fmt.Errorf("%w: selected record count overflow", ErrMaterializeSelection)
			}
			selectedRecordCount += length
			ranges = append(ranges, MaterializeRange{
				ContributionID: contribution.ContributionID,
				SourceAgent:    contribution.Source.Agent,
				ReplicaID:      sourceRange.ReplicaID,
				StartRecord:    sourceRange.StartRecord,
				EndRecord:      sourceRange.EndRecord,
				Records:        cloneMaterializeRecords(snapshot.Records[int(sourceRange.StartRecord):int(sourceRange.EndRecord)]),
			})
		}
	}

	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].ReplicaID != intervals[j].ReplicaID {
			return intervals[i].ReplicaID < intervals[j].ReplicaID
		}
		if intervals[i].StartRecord != intervals[j].StartRecord {
			return intervals[i].StartRecord < intervals[j].StartRecord
		}
		if intervals[i].EndRecord != intervals[j].EndRecord {
			return intervals[i].EndRecord < intervals[j].EndRecord
		}
		return intervals[i].ContributionID < intervals[j].ContributionID
	})
	for i := 1; i < len(intervals); i++ {
		previous, current := intervals[i-1], intervals[i]
		if previous.ReplicaID == current.ReplicaID && current.StartRecord < previous.EndRecord {
			return MaterializeSelection{}, fmt.Errorf("%w: selected ranges belong to different Contributions", ErrMaterializeRangeOverlap)
		}
	}

	return MaterializeSelection{
		Coverage:            coverage,
		SelectedHeads:       sortedMaterializeHeads(heads),
		Ranges:              ranges,
		SelectedRecordCount: selectedRecordCount,
	}, nil
}

func sortedMaterializeHeads(heads []string) []string {
	result := append([]string(nil), heads...)
	sort.Strings(result)
	return result
}

type materializeRangeInterval struct {
	ReplicaID      string
	StartRecord    uint64
	EndRecord      uint64
	ContributionID string
}

func validateMaterializeReplicaSnapshot(snapshot syncer.ReplicaSnapshot, sessionID, replicaID string) error {
	if err := snapshot.Descriptor.Validate(); err != nil {
		return fmt.Errorf("%w: Replica descriptor: %w", ErrMaterializeSelection, err)
	}
	if err := snapshot.Tip.Validate(); err != nil {
		return fmt.Errorf("%w: Replica tip: %w", ErrMaterializeSelection, err)
	}
	if _, err := snapshot.Layout.ReplicaPrefix(); err != nil {
		return fmt.Errorf("%w: Replica layout: %w", ErrMaterializeSelection, err)
	}
	if snapshot.Descriptor.SessionID != sessionID || snapshot.Descriptor.ReplicaID != replicaID || snapshot.Tip.ReplicaID != replicaID {
		return fmt.Errorf("%w: Replica does not belong to the selected Session", ErrMaterializeSelection)
	}
	if snapshot.Descriptor.ReplicaID != snapshot.Layout.ReplicaKey() || snapshot.Descriptor.SessionID != snapshot.Layout.SessionKey() || snapshot.Descriptor.Source.DeviceID != snapshot.Layout.DeviceID() {
		return fmt.Errorf("%w: Replica descriptor does not match its namespace", ErrMaterializeSelection)
	}
	if snapshot.Tip.RecordCount != uint64(len(snapshot.Records)) || snapshot.Tip.RecordCount == 0 {
		return fmt.Errorf("%w: Replica tip does not match its complete body", ErrMaterializeSelection)
	}
	digest, err := syncer.DigestRecords(snapshot.Records)
	if err != nil {
		return fmt.Errorf("%w: Replica records: %w", ErrMaterializeSelection, err)
	}
	if digest != snapshot.HeadDigest || hex.EncodeToString(digest[:]) != snapshot.Tip.HeadDigest {
		return fmt.Errorf("%w: Replica body digest does not match its tip", ErrMaterializeSelection)
	}
	return nil
}

func validateMaterializeRange(snapshot syncer.ReplicaSnapshot, sourceRange sessionhub.RangeRef) error {
	if sourceRange.ReplicaID != snapshot.Descriptor.ReplicaID {
		return errors.New("range belongs to another Replica")
	}
	if sourceRange.EndRecord > uint64(len(snapshot.Records)) || sourceRange.StartRecord >= sourceRange.EndRecord {
		return errors.New("range is outside the complete Replica body")
	}
	start := int(sourceRange.StartRecord)
	end := int(sourceRange.EndRecord)
	prefix, err := syncer.DigestRecords(snapshot.Records[:start])
	if err != nil {
		return fmt.Errorf("calculate prefix digest: %w", err)
	}
	rangeDigest, err := syncer.DigestRecords(snapshot.Records[start:end])
	if err != nil {
		return fmt.Errorf("calculate range digest: %w", err)
	}
	if hex.EncodeToString(prefix[:]) != sourceRange.PrefixDigest {
		return errors.New("range prefix digest does not match")
	}
	if hex.EncodeToString(rangeDigest[:]) != sourceRange.RangeDigest {
		return errors.New("range digest does not match")
	}
	return nil
}
