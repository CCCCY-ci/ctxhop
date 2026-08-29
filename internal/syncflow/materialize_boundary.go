package syncflow

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrMaterializeBoundaryUnknown prevents a materialized target from
	// publishing Contributions when its imported prefix cannot be proven from
	// local-only binding state and an authenticated Replica snapshot.
	ErrMaterializeBoundaryUnknown = errors.New("syncflow: materialized import boundary is unknown")

	// ErrMaterializePrefixRewrite reports a target NativeSession whose already
	// observed prefix changed. Appending it to the current Replica would make
	// existing immutable ranges untrue, so the caller must start a new
	// generation instead.
	ErrMaterializePrefixRewrite = errors.New("syncflow: materialized target prefix was rewritten")

	// ErrMaterializeContributionConflict reports a remote Contribution chain
	// that cannot be reconciled with the local materialization boundary.
	ErrMaterializeContributionConflict = errors.New("syncflow: materialized contribution chain conflicts")
)

// MaterializedSuffixRequest contains only authenticated or local-only facts
// needed to plan the next target-Agent Contribution. ExistingContributions
// must be one complete Session snapshot when a suffix needs to be published.
type MaterializedSuffixRequest struct {
	Binding               sessionhub.LocalBinding
	Snapshot              syncer.ReplicaSnapshot
	ExistingContributions []sessionhub.Contribution
	IdentifierKey         []byte
}

// MaterializedSuffixPlan advances local binding cursors to the authenticated
// Replica/Contribution state. Contribution is nil when the target has no new
// post-import records or a prior remote write was fully recovered.
type MaterializedSuffixPlan struct {
	Binding      sessionhub.LocalBinding
	Contribution *sessionhub.Contribution
	Recovered    bool
}

// ValidateMaterializedPushPreflight proves the imported boundary and every
// locally remembered prefix before a caller performs any v2 remote write.
// It deliberately does not inspect remote state.
func ValidateMaterializedPushPreflight(binding sessionhub.LocalBinding, records [][]byte) error {
	if err := binding.Validate(); err != nil {
		return fmt.Errorf("%w: binding: %v", ErrMaterializeBoundaryUnknown, err)
	}
	if binding.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize || binding.Origin.ImportBoundary == nil {
		return fmt.Errorf("%w: binding is not a local materialization", ErrMaterializeBoundaryUnknown)
	}
	return validateMaterializedLocalPrefixes(binding, records)
}

// PlanMaterializedSuffix proves the imported target prefix, reconciles any
// already-published target Contributions, and creates at most one append-only
// suffix Contribution. It performs no local or remote writes.
func PlanMaterializedSuffix(request MaterializedSuffixRequest) (MaterializedSuffixPlan, error) {
	binding := request.Binding
	if len(request.IdentifierKey) == 0 {
		return MaterializedSuffixPlan{}, errors.New("syncflow: materialized suffix identifier key is required")
	}
	if err := ValidateMaterializedPushPreflight(binding, request.Snapshot.Records); err != nil {
		return MaterializedSuffixPlan{}, err
	}
	if err := validateMaterializedReplicaSnapshot(binding, request.Snapshot); err != nil {
		return MaterializedSuffixPlan{}, err
	}
	records := request.Snapshot.Records

	remoteCursor, remoteParents, recovered, err := reconcileMaterializedContributions(binding, request.Snapshot, request.ExistingContributions, request.IdentifierKey)
	if err != nil {
		return MaterializedSuffixPlan{}, err
	}

	updated := binding
	updated.ReplicaCursor = sessionhub.ReplicaCursor{
		NextShard:   request.Snapshot.Tip.LastShard + 1,
		RecordCount: request.Snapshot.Tip.RecordCount,
		HeadDigest:  request.Snapshot.Tip.HeadDigest,
	}
	updated.LocalSnapshot = &sessionhub.LocalSessionSnapshot{
		RecordCount: request.Snapshot.Tip.RecordCount,
		HeadDigest:  request.Snapshot.Tip.HeadDigest,
	}
	updated.ContributionCursor = remoteCursor

	start := materializedContributionStart(remoteCursor)
	end := uint64(len(records))
	if start > end {
		return MaterializedSuffixPlan{}, fmt.Errorf("%w: contribution cursor exceeds authenticated Replica", ErrMaterializeContributionConflict)
	}
	if start == end {
		if err := updated.Validate(); err != nil {
			return MaterializedSuffixPlan{}, fmt.Errorf("syncflow: update materialized binding: %w", err)
		}
		return MaterializedSuffixPlan{Binding: updated, Recovered: recovered}, nil
	}
	if len(request.ExistingContributions) == 0 {
		return MaterializedSuffixPlan{}, fmt.Errorf("%w: materialized base heads are unavailable", ErrMaterializeContributionConflict)
	}

	prefixDigest, err := syncer.DigestRecords(records[:start])
	if err != nil {
		return MaterializedSuffixPlan{}, fmt.Errorf("syncflow: digest materialized Contribution prefix: %w", err)
	}
	rangeDigest, err := syncer.DigestRecords(records[start:end])
	if err != nil {
		return MaterializedSuffixPlan{}, fmt.Errorf("syncflow: digest materialized Contribution range: %w", err)
	}
	contribution := sessionhub.Contribution{
		Version:   sessionhub.ModelVersion,
		SessionID: binding.SessionID,
		Source: sessionhub.ContributionSource{
			Agent:      binding.Agent,
			ReplicaID:  binding.ReplicaID,
			DeviceID:   request.Snapshot.Descriptor.Source.DeviceID,
			Generation: binding.Generation,
		},
		Parents: append([]string(nil), remoteParents...),
		Ranges: []sessionhub.RangeRef{{
			ReplicaID:    binding.ReplicaID,
			StartRecord:  start,
			EndRecord:    end,
			PrefixDigest: hex.EncodeToString(prefixDigest[:]),
			RangeDigest:  hex.EncodeToString(rangeDigest[:]),
		}},
		EnvironmentRefs: []string{},
		CreatedAt:       stableMaterializedContributionTime(binding.SessionID, binding.ReplicaID, start, end, rangeDigest),
	}
	contribution, err = contribution.WithDerivedID(request.IdentifierKey)
	if err != nil {
		return MaterializedSuffixPlan{}, fmt.Errorf("syncflow: derive materialized Contribution: %w", err)
	}
	updated.ContributionCursor = sessionhub.ContributionCursor{
		EndRecord:          end,
		LastContributionID: contribution.ContributionID,
	}
	if err := updated.Validate(); err != nil {
		return MaterializedSuffixPlan{}, fmt.Errorf("syncflow: update materialized binding: %w", err)
	}
	return MaterializedSuffixPlan{Binding: updated, Contribution: &contribution, Recovered: recovered}, nil
}

func validateMaterializedReplicaSnapshot(binding sessionhub.LocalBinding, snapshot syncer.ReplicaSnapshot) error {
	if err := snapshot.Descriptor.Validate(); err != nil {
		return fmt.Errorf("syncflow: invalid materialized Replica descriptor: %w", err)
	}
	if err := snapshot.Tip.Validate(); err != nil {
		return fmt.Errorf("syncflow: invalid materialized Replica tip: %w", err)
	}
	if snapshot.Descriptor.ReplicaID != binding.ReplicaID || snapshot.Descriptor.SessionID != binding.SessionID || snapshot.Tip.ReplicaID != binding.ReplicaID {
		return fmt.Errorf("%w: Replica identity differs from binding", ErrMaterializeBoundaryUnknown)
	}
	if snapshot.Descriptor.Source.Agent != binding.Agent || snapshot.Descriptor.Source.Generation != binding.Generation {
		return fmt.Errorf("%w: Replica source differs from binding", ErrMaterializeBoundaryUnknown)
	}
	if snapshot.Descriptor.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize || !sameMaterializedIDs(snapshot.Descriptor.Origin.BaseHeads, binding.Origin.BaseHeads) {
		return fmt.Errorf("%w: Replica origin differs from binding", ErrMaterializeBoundaryUnknown)
	}
	if snapshot.Tip.RecordCount != uint64(len(snapshot.Records)) || snapshot.Tip.RecordCount == 0 || snapshot.Tip.LastShard == ^uint64(0) {
		return fmt.Errorf("%w: Replica count is invalid", ErrMaterializeBoundaryUnknown)
	}
	digest, err := syncer.DigestRecords(snapshot.Records)
	if err != nil {
		return fmt.Errorf("syncflow: digest materialized Replica: %w", err)
	}
	if digest != snapshot.HeadDigest || hex.EncodeToString(digest[:]) != snapshot.Tip.HeadDigest {
		return fmt.Errorf("%w: Replica body digest differs from tip", ErrMaterializeBoundaryUnknown)
	}
	return nil
}

func validateMaterializedLocalPrefixes(binding sessionhub.LocalBinding, records [][]byte) error {
	boundary := binding.Origin.ImportBoundary
	if boundary == nil || boundary.RecordCount > uint64(len(records)) {
		return fmt.Errorf("%w: imported prefix exceeds local Replica", ErrMaterializeBoundaryUnknown)
	}
	boundaryDigest, err := syncer.DigestRecords(records[:boundary.RecordCount])
	if err != nil {
		return fmt.Errorf("syncflow: digest materialized import boundary: %w", err)
	}
	wantBoundary := strings.TrimPrefix(boundary.PrefixDigest, "sha256:")
	if hex.EncodeToString(boundaryDigest[:]) != wantBoundary {
		return fmt.Errorf("%w: imported prefix digest differs", ErrMaterializePrefixRewrite)
	}
	if binding.LocalSnapshot == nil || binding.LocalSnapshot.RecordCount > uint64(len(records)) {
		return fmt.Errorf("%w: prior local snapshot exceeds current Replica", ErrMaterializePrefixRewrite)
	}
	localDigest, err := syncer.DigestRecords(records[:binding.LocalSnapshot.RecordCount])
	if err != nil {
		return fmt.Errorf("syncflow: digest prior materialized snapshot: %w", err)
	}
	if hex.EncodeToString(localDigest[:]) != binding.LocalSnapshot.HeadDigest {
		return fmt.Errorf("%w: prior local snapshot digest differs", ErrMaterializePrefixRewrite)
	}
	if binding.ReplicaCursor.RecordCount > uint64(len(records)) {
		return fmt.Errorf("%w: published Replica cursor exceeds current body", ErrMaterializePrefixRewrite)
	}
	replicaPrefix, err := syncer.DigestRecords(records[:binding.ReplicaCursor.RecordCount])
	if err != nil {
		return fmt.Errorf("syncflow: digest published materialized prefix: %w", err)
	}
	if hex.EncodeToString(replicaPrefix[:]) != binding.ReplicaCursor.HeadDigest {
		return fmt.Errorf("%w: published Replica cursor digest differs", ErrMaterializePrefixRewrite)
	}
	return nil
}

func reconcileMaterializedContributions(binding sessionhub.LocalBinding, snapshot syncer.ReplicaSnapshot, existing []sessionhub.Contribution, identifierKey []byte) (sessionhub.ContributionCursor, []string, bool, error) {
	boundary := binding.Origin.ImportBoundary.RecordCount
	remoteCursor := sessionhub.ContributionCursor{EndRecord: boundary}
	parents := append([]string(nil), binding.Origin.BaseHeads...)
	sort.Strings(parents)
	localCursor := binding.ContributionCursor
	if localCursor.EndRecord == 0 && localCursor.LastContributionID == "" {
		localCursor.EndRecord = boundary
	}

	if len(existing) != 0 {
		graph, err := sessionhub.NewContributionGraph(binding.SessionID, existing)
		if err != nil {
			return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: authenticated Session graph: %v", ErrMaterializeContributionConflict, err)
		}
		for _, parent := range parents {
			if _, ok := graph.Contribution(parent); !ok {
				return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: materialized base head is unavailable", ErrMaterializeContributionConflict)
			}
		}
	}

	target := make([]sessionhub.Contribution, 0)
	for _, contribution := range existing {
		if contribution.Source.ReplicaID != binding.ReplicaID {
			continue
		}
		if contribution.Source.Agent != binding.Agent || contribution.Source.DeviceID != snapshot.Descriptor.Source.DeviceID || contribution.Source.Generation != binding.Generation || len(contribution.Ranges) != 1 {
			return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: target Replica has incompatible Contribution source", ErrMaterializeContributionConflict)
		}
		derived, err := contribution.WithDerivedID(identifierKey)
		if err != nil || derived.ContributionID != contribution.ContributionID {
			return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: target Contribution identity is not canonical", ErrMaterializeContributionConflict)
		}
		target = append(target, contribution)
	}
	sort.Slice(target, func(i, j int) bool {
		left, right := target[i].Ranges[0], target[j].Ranges[0]
		if left.StartRecord != right.StartRecord {
			return left.StartRecord < right.StartRecord
		}
		if left.EndRecord != right.EndRecord {
			return left.EndRecord < right.EndRecord
		}
		return target[i].ContributionID < target[j].ContributionID
	})

	next := boundary
	localCursorFound := localCursor.EndRecord == boundary && localCursor.LastContributionID == ""
	for _, contribution := range target {
		rangeRef := contribution.Ranges[0]
		if rangeRef.StartRecord != next || rangeRef.EndRecord > uint64(len(snapshot.Records)) {
			return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: target ranges overlap, have a gap, or exceed Replica", ErrMaterializeContributionConflict)
		}
		if !sameMaterializedIDs(contribution.Parents, parents) {
			return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: target Contribution parents differ", ErrMaterializeContributionConflict)
		}
		if err := validateMaterializedContributionRange(snapshot.Records, rangeRef); err != nil {
			return sessionhub.ContributionCursor{}, nil, false, err
		}
		next = rangeRef.EndRecord
		remoteCursor = sessionhub.ContributionCursor{EndRecord: next, LastContributionID: contribution.ContributionID}
		parents = []string{contribution.ContributionID}
		if localCursor.EndRecord == next && localCursor.LastContributionID == contribution.ContributionID {
			localCursorFound = true
		}
	}
	if !localCursorFound {
		return sessionhub.ContributionCursor{}, nil, false, fmt.Errorf("%w: local Contribution cursor is not in remote chain", ErrMaterializeContributionConflict)
	}
	recovered := remoteCursor.EndRecord != binding.ContributionCursor.EndRecord || remoteCursor.LastContributionID != binding.ContributionCursor.LastContributionID
	return remoteCursor, parents, recovered, nil
}

func validateMaterializedContributionRange(records [][]byte, rangeRef sessionhub.RangeRef) error {
	start, end := rangeRef.StartRecord, rangeRef.EndRecord
	if start >= end || end > uint64(len(records)) {
		return fmt.Errorf("%w: target Contribution range is invalid", ErrMaterializeContributionConflict)
	}
	prefix, err := syncer.DigestRecords(records[:start])
	if err != nil {
		return fmt.Errorf("syncflow: digest existing materialized prefix: %w", err)
	}
	rangeDigest, err := syncer.DigestRecords(records[start:end])
	if err != nil {
		return fmt.Errorf("syncflow: digest existing materialized range: %w", err)
	}
	if hex.EncodeToString(prefix[:]) != rangeRef.PrefixDigest || hex.EncodeToString(rangeDigest[:]) != rangeRef.RangeDigest {
		return fmt.Errorf("%w: target Contribution digest differs", ErrMaterializeContributionConflict)
	}
	return nil
}

func materializedContributionStart(remote sessionhub.ContributionCursor) uint64 {
	return remote.EndRecord
}

func sameMaterializedIDs(left, right []string) bool {
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	if len(leftCopy) != len(rightCopy) {
		return false
	}
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func stableMaterializedContributionTime(sessionID, replicaID string, start, end uint64, rangeDigest [32]byte) time.Time {
	var identity bytes.Buffer
	identity.WriteString("ctxhop/materialized-contribution-time/v1\x00")
	identity.WriteString(sessionID)
	identity.WriteByte(0)
	identity.WriteString(replicaID)
	var bounds [16]byte
	binary.BigEndian.PutUint64(bounds[:8], start)
	binary.BigEndian.PutUint64(bounds[8:], end)
	identity.Write(bounds[:])
	identity.Write(rangeDigest[:])
	digest := sessionhub.DigestBytes(identity.Bytes())
	const windowSeconds = uint64(50 * 365 * 24 * 60 * 60)
	seconds := binary.BigEndian.Uint64(digest[:8]) % windowSeconds
	return time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
}
