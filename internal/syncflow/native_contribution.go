package syncflow

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrNativeContributionConflict means that the local source cursor and the
	// immutable Contribution chain no longer describe one append-only stream.
	// The caller must stop or allocate a new Replica generation; it must never
	// overwrite an existing Contribution.
	ErrNativeContributionConflict = errors.New("syncflow: native contribution chain conflicts")
)

// NativeContributionRequest contains the authenticated local facts needed to
// plan one native source Contribution. ExistingContributions must be a
// complete snapshot of the logical Session when it is non-empty.
type NativeContributionRequest struct {
	Binding               sessionhub.LocalBinding
	DeviceID              string
	Records               [][]byte
	ExistingContributions []sessionhub.Contribution
	IdentifierKey         []byte
	Parents               []string
	EnvironmentRefs       []string
	Cursor                sessionhub.ReplicaCursor
	Now                   time.Time
}

// NativeContributionPlan advances the local contribution cursor and returns
// at most one immutable Contribution. A nil Contribution means the remote
// chain already covers the complete local Replica prefix.
type NativeContributionPlan struct {
	Binding      sessionhub.LocalBinding
	Contribution *sessionhub.Contribution
	Recovered    bool
}

// PlanNativeContribution reconciles a normal native Replica with its logical
// Session Contribution chain. It is deliberately write-free: remote object
// publication and local binding persistence happen in the command layer after
// this plan has passed all checks.
func PlanNativeContribution(request NativeContributionRequest) (NativeContributionPlan, error) {
	if len(request.IdentifierKey) == 0 {
		return NativeContributionPlan{}, errors.New("syncflow: native Contribution identifier key is required")
	}
	if err := request.Binding.Validate(); err != nil {
		return NativeContributionPlan{}, fmt.Errorf("%w: binding: %v", ErrNativeContributionConflict, err)
	}
	if request.Binding.Origin.Kind == sessionhub.ReplicaOriginLocalMaterialize {
		return NativeContributionPlan{}, fmt.Errorf("%w: native planner received a materialized binding", ErrNativeContributionConflict)
	}
	if err := syncflowNativeDeviceID(request.DeviceID); err != nil {
		return NativeContributionPlan{}, err
	}
	if request.Cursor.RecordCount != uint64(len(request.Records)) {
		return NativeContributionPlan{}, fmt.Errorf("%w: Replica cursor has %d records, local stream has %d", ErrNativeContributionConflict, request.Cursor.RecordCount, len(request.Records))
	}
	digest, err := syncer.DigestRecords(request.Records)
	if err != nil {
		return NativeContributionPlan{}, fmt.Errorf("syncflow: digest native Replica: %w", err)
	}
	if request.Cursor.HeadDigest != hex.EncodeToString(digest[:]) {
		return NativeContributionPlan{}, fmt.Errorf("%w: Replica cursor digest differs from local stream", ErrNativeContributionConflict)
	}
	if request.Cursor.NextShard == 0 && request.Cursor.RecordCount != 0 {
		return NativeContributionPlan{}, fmt.Errorf("%w: Replica cursor has no next shard", ErrNativeContributionConflict)
	}

	parents := append([]string(nil), request.Parents...)
	if len(parents) == 0 {
		parents = append(parents, request.Binding.Origin.BaseHeads...)
	}
	parents = uniqueSortedNativeStrings(parents)
	for _, parent := range parents {
		if parent == "" {
			return NativeContributionPlan{}, fmt.Errorf("%w: empty parent", ErrNativeContributionConflict)
		}
	}

	target, err := nativeTargetContributions(request)
	if err != nil {
		return NativeContributionPlan{}, err
	}
	remoteCursor := sessionhub.ContributionCursor{}
	localCursor := request.Binding.ContributionCursor
	// A same-Agent restore can land on a different device. The restored
	// NativeSession already contains the source Replica prefix, but that prefix
	// is not a Contribution owned by the new device's Replica. Keep the
	// independent contribution cursor at the restore boundary until the target
	// device appends something of its own. The target Replica may be empty (new
	// device) or may already contain the same-device prefix (resume/retry), so
	// both cases must be reconciled below.
	sameAgentRestoreBaseline := request.Binding.Origin.Kind == sessionhub.ReplicaOriginSameAgentRestore &&
		localCursor.LastContributionID == "" && localCursor.EndRecord > 0
	if sameAgentRestoreBaseline && localCursor.EndRecord > request.Cursor.RecordCount {
		return NativeContributionPlan{}, fmt.Errorf("%w: same-Agent restore boundary exceeds Replica", ErrNativeContributionConflict)
	}
	localCursorFound := localCursor.EndRecord == 0 && localCursor.LastContributionID == ""
	if sameAgentRestoreBaseline {
		remoteCursor = localCursor
		localCursorFound = true
	}
	expectedParents := append([]string(nil), parents...)
	if sameAgentRestoreBaseline {
		// Existing Contributions for the target Replica describe its own
		// append chain, which starts at a root even though the restored target
		// will use Origin.BaseHeads for its first *new* range. Do not compare
		// that old target chain with the restore parent.
		expectedParents = nil
	}
	next := uint64(0)
	for _, contribution := range target {
		rangeRef := contribution.Ranges[0]
		if rangeRef.StartRecord != next || rangeRef.EndRecord > uint64(len(request.Records)) {
			return NativeContributionPlan{}, fmt.Errorf("%w: target Contribution ranges overlap, gap, or exceed Replica", ErrNativeContributionConflict)
		}
		if !sameNativeStringSet(contribution.Parents, expectedParents) {
			return NativeContributionPlan{}, fmt.Errorf("%w: target Contribution parents differ", ErrNativeContributionConflict)
		}
		if err := validateNativeContributionRange(request.Records, rangeRef); err != nil {
			return NativeContributionPlan{}, err
		}
		next = rangeRef.EndRecord
		remoteCursor = sessionhub.ContributionCursor{EndRecord: next, LastContributionID: contribution.ContributionID}
		parents = []string{contribution.ContributionID}
		expectedParents = append(expectedParents[:0], contribution.ContributionID)
		if localCursor.EndRecord == next && localCursor.LastContributionID == contribution.ContributionID {
			localCursorFound = true
		}
	}
	if sameAgentRestoreBaseline && len(target) != 0 && next < localCursor.EndRecord {
		return NativeContributionPlan{}, fmt.Errorf("%w: target Contribution chain stops before same-Agent restore boundary", ErrNativeContributionConflict)
	}
	if !localCursorFound {
		return NativeContributionPlan{}, fmt.Errorf("%w: local Contribution cursor is not in the remote chain", ErrNativeContributionConflict)
	}
	recovered := remoteCursor.EndRecord != localCursor.EndRecord || remoteCursor.LastContributionID != localCursor.LastContributionID

	updated := request.Binding
	updated.ReplicaCursor = request.Cursor
	updated.ContributionCursor = remoteCursor
	if len(target) != 0 && len(updated.Origin.BaseHeads) == 0 {
		// Preserve the actual branch boundary in local state. This is useful on a
		// later generation rewrite and does not change the immutable remote graph.
		updated.Origin.BaseHeads = append([]string(nil), target[0].Parents...)
	}

	start := remoteCursor.EndRecord
	end := uint64(len(request.Records))
	if start > end {
		return NativeContributionPlan{}, fmt.Errorf("%w: Contribution cursor exceeds Replica", ErrNativeContributionConflict)
	}
	if start == end {
		if err := updated.Validate(); err != nil {
			return NativeContributionPlan{}, fmt.Errorf("syncflow: update native binding: %w", err)
		}
		return NativeContributionPlan{Binding: updated, Recovered: recovered}, nil
	}

	prefixDigest, err := syncer.DigestRecords(request.Records[:start])
	if err != nil {
		return NativeContributionPlan{}, fmt.Errorf("syncflow: digest native Contribution prefix: %w", err)
	}
	rangeDigest, err := syncer.DigestRecords(request.Records[start:end])
	if err != nil {
		return NativeContributionPlan{}, fmt.Errorf("syncflow: digest native Contribution range: %w", err)
	}
	createdAt := request.Now
	if createdAt.IsZero() {
		createdAt = stableNativeContributionTime(updated.SessionID, updated.ReplicaID, start, end, rangeDigest)
	}
	contribution := sessionhub.Contribution{
		Version:   sessionhub.ModelVersion,
		SessionID: updated.SessionID,
		Source: sessionhub.ContributionSource{
			Agent:      updated.Agent,
			ReplicaID:  updated.ReplicaID,
			DeviceID:   request.DeviceID,
			Generation: updated.Generation,
		},
		Parents: append([]string(nil), parents...),
		Ranges: []sessionhub.RangeRef{{
			ReplicaID:    updated.ReplicaID,
			StartRecord:  start,
			EndRecord:    end,
			PrefixDigest: hex.EncodeToString(prefixDigest[:]),
			RangeDigest:  hex.EncodeToString(rangeDigest[:]),
		}},
		EnvironmentRefs: uniqueSortedNativeStrings(request.EnvironmentRefs),
		CreatedAt:       createdAt.UTC().Round(0),
	}
	contribution, err = contribution.WithDerivedID(request.IdentifierKey)
	if err != nil {
		return NativeContributionPlan{}, fmt.Errorf("syncflow: derive native Contribution: %w", err)
	}
	updated.ContributionCursor = sessionhub.ContributionCursor{
		EndRecord:          end,
		LastContributionID: contribution.ContributionID,
	}
	if err := updated.Validate(); err != nil {
		return NativeContributionPlan{}, fmt.Errorf("syncflow: update native binding: %w", err)
	}
	return NativeContributionPlan{Binding: updated, Contribution: &contribution, Recovered: recovered}, nil
}

func nativeTargetContributions(request NativeContributionRequest) ([]sessionhub.Contribution, error) {
	target := make([]sessionhub.Contribution, 0)
	for _, contribution := range request.ExistingContributions {
		if contribution.Source.ReplicaID != request.Binding.ReplicaID {
			continue
		}
		if contribution.Source.Agent != request.Binding.Agent || contribution.Source.DeviceID != request.DeviceID || contribution.Source.Generation != request.Binding.Generation || len(contribution.Ranges) != 1 {
			return nil, fmt.Errorf("%w: target Contribution source differs from binding", ErrNativeContributionConflict)
		}
		derived, err := contribution.WithDerivedID(request.IdentifierKey)
		if err != nil || derived.ContributionID != contribution.ContributionID {
			return nil, fmt.Errorf("%w: target Contribution identity is not canonical", ErrNativeContributionConflict)
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
	return target, nil
}

func validateNativeContributionRange(records [][]byte, rangeRef sessionhub.RangeRef) error {
	if rangeRef.StartRecord >= rangeRef.EndRecord || rangeRef.EndRecord > uint64(len(records)) {
		return fmt.Errorf("%w: Contribution range is invalid", ErrNativeContributionConflict)
	}
	prefix, err := syncer.DigestRecords(records[:rangeRef.StartRecord])
	if err != nil {
		return fmt.Errorf("syncflow: digest existing native Contribution prefix: %w", err)
	}
	rangeDigest, err := syncer.DigestRecords(records[rangeRef.StartRecord:rangeRef.EndRecord])
	if err != nil {
		return fmt.Errorf("syncflow: digest existing native Contribution range: %w", err)
	}
	if hex.EncodeToString(prefix[:]) != rangeRef.PrefixDigest || hex.EncodeToString(rangeDigest[:]) != rangeRef.RangeDigest {
		return fmt.Errorf("%w: Contribution range digest differs", ErrNativeContributionConflict)
	}
	return nil
}

func uniqueSortedNativeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sameNativeStringSet(left, right []string) bool {
	left = uniqueSortedNativeStrings(left)
	right = uniqueSortedNativeStrings(right)
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

func syncflowNativeDeviceID(value string) error {
	if value == "" {
		return errors.New("syncflow: native Contribution device identity is required")
	}
	return nil
}

func stableNativeContributionTime(sessionID, replicaID string, start, end uint64, rangeDigest [32]byte) time.Time {
	data := make([]byte, 0, len(sessionID)+len(replicaID)+64)
	data = append(data, []byte("ctxhop/native-contribution-time/v1\x00")...)
	data = append(data, sessionID...)
	data = append(data, 0)
	data = append(data, replicaID...)
	var bounds [16]byte
	binary.BigEndian.PutUint64(bounds[:8], start)
	binary.BigEndian.PutUint64(bounds[8:], end)
	data = append(data, bounds[:]...)
	data = append(data, rangeDigest[:]...)
	digest := sessionhub.DigestBytes(data)
	seconds := binary.BigEndian.Uint64(digest[:8]) % uint64(50*365*24*60*60)
	return time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
}
