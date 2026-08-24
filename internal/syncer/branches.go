package syncer

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
)

// ShardPart associates an immutable shard with the device-local sequence
// number used in its remote object name.
type ShardPart struct {
	Number uint64
	Shard  Shard
}

// Branch is one complete device stream for a session.
type Branch struct {
	// DeviceID identifies the device that wrote this stream. It is an opaque
	// derived identifier, not a local path or a user-facing device name.
	DeviceID string

	// Records is the complete canonical record sequence assembled from the
	// device's contiguous shards.
	Records [][]byte

	// HeadDigest is the digest after the final record in Records.
	HeadDigest [32]byte
}

// AssembleBranch validates and assembles one device's shard stream.
//
// Shard parts may arrive in arbitrary list order. A gap, duplicate sequence,
// base mismatch, or digest mismatch is an error: callers must not silently
// use the available prefix as a complete session.
func AssembleBranch(deviceID string, parts []ShardPart) (Branch, error) {
	if deviceID == "" {
		return Branch{}, errors.New("syncer: device identity is required")
	}
	if len(parts) == 0 {
		return Branch{}, fmt.Errorf("%w: device %q has no shards", ErrIncompleteBranch, deviceID)
	}

	ordered := append([]ShardPart(nil), parts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Number < ordered[j].Number
	})

	builder := newBranchBuilder(deviceID)
	for _, part := range ordered {
		if err := builder.append(part.Number, part.Shard); err != nil {
			return Branch{}, err
		}
	}

	return builder.finish()
}

type branchBuilder struct {
	deviceID   string
	nextNumber uint64
	base       uint64
	digest     [32]byte
	records    [][]byte
	totalBytes uint64
}

func newBranchBuilder(deviceID string) *branchBuilder {
	return &branchBuilder{deviceID: deviceID, nextNumber: 1, digest: EmptyDigest()}
}

func (b *branchBuilder) append(number uint64, shard Shard) error {
	if number == 0 || number != b.nextNumber {
		return fmt.Errorf("%w: device %q has a gap near shard %d", ErrIncompleteBranch, b.deviceID, number)
	}
	if err := shard.Validate(); err != nil {
		return fmt.Errorf("%w: device %q shard %d: %v", ErrIncompleteBranch, b.deviceID, number, err)
	}
	if shard.Base != b.base {
		return fmt.Errorf("%w: device %q shard %d starts at %d, want %d", ErrIncompleteBranch, b.deviceID, number, shard.Base, b.base)
	}
	if shard.PrefixDigest != b.digest {
		return fmt.Errorf("%w: device %q shard %d has a mismatched prefix digest", ErrIncompleteBranch, b.deviceID, number)
	}
	shardCount := shard.Count()
	if ^uint64(0)-b.base < shardCount {
		return fmt.Errorf("%w: device %q record count overflows", ErrIncompleteBranch, b.deviceID)
	}
	if shardCount > maxSessionRecords || uint64(len(b.records)) > maxSessionRecords-shardCount {
		return fmt.Errorf("%w: device %q has more than %d records", ErrSessionTooLarge, b.deviceID, maxSessionRecords)
	}
	for _, record := range shard.Records {
		recordBytes := uint64(len(record))
		if recordBytes > maxSessionBytes || b.totalBytes > maxSessionBytes-recordBytes {
			return fmt.Errorf("%w: device %q has more than %d record bytes", ErrSessionTooLarge, b.deviceID, maxSessionBytes)
		}
		b.totalBytes += recordBytes
	}

	b.records = append(b.records, cloneRecords(shard.Records)...)
	b.base += shardCount
	b.digest = shard.Digest()
	if b.nextNumber == ^uint64(0) {
		b.nextNumber = 0
	} else {
		b.nextNumber++
	}
	return nil
}

func (b *branchBuilder) finish() (Branch, error) {
	if len(b.records) == 0 {
		return Branch{}, fmt.Errorf("%w: device %q has no shards", ErrIncompleteBranch, b.deviceID)
	}
	return Branch{DeviceID: b.deviceID, Records: b.records, HeadDigest: b.digest}, nil
}

// Relation describes the prefix relationship between two record sequences.
type Relation int

const (
	// Equal means both sequences contain the same records.
	Equal Relation = iota
	// LeftPrefix means left is a strict prefix of right.
	LeftPrefix
	// RightPrefix means right is a strict prefix of left.
	RightPrefix
	// Diverged means neither sequence is a prefix of the other.
	Diverged
)

// String returns a stable name for a relation.
func (r Relation) String() string {
	switch r {
	case Equal:
		return "equal"
	case LeftPrefix:
		return "left-prefix"
	case RightPrefix:
		return "right-prefix"
	case Diverged:
		return "diverged"
	default:
		return "unknown"
	}
}

// Comparison is the result of comparing two canonical record sequences.
type Comparison struct {
	Relation     Relation
	CommonPrefix uint64
	LeftCount    uint64
	RightCount   uint64
}

// CompareRecords compares two complete canonical sequences byte-for-byte.
func CompareRecords(left, right [][]byte) Comparison {
	common := 0
	for common < len(left) && common < len(right) && bytes.Equal(left[common], right[common]) {
		common++
	}

	relation := Diverged
	switch {
	case common == len(left) && common == len(right):
		relation = Equal
	case common == len(left):
		relation = LeftPrefix
	case common == len(right):
		relation = RightPrefix
	}
	return Comparison{
		Relation:     relation,
		CommonPrefix: uint64(common),
		LeftCount:    uint64(len(left)),
		RightCount:   uint64(len(right)),
	}
}

// ResolutionKind describes the maximal versions found across devices.
type ResolutionKind int

const (
	// ResolutionConsistent means every device exposed the same sequence.
	ResolutionConsistent ResolutionKind = iota
	// ResolutionFastForward means one sequence strictly extends the others.
	ResolutionFastForward
	// ResolutionFork means multiple incomparable sequences must be preserved.
	ResolutionFork
)

// String returns a stable name for a resolution kind.
func (k ResolutionKind) String() string {
	switch k {
	case ResolutionConsistent:
		return "consistent"
	case ResolutionFastForward:
		return "fast-forward"
	case ResolutionFork:
		return "fork"
	default:
		return "unknown"
	}
}

// Version is one maximal session version. Devices contains every device whose
// assembled branch has exactly this content.
type Version struct {
	Records    [][]byte
	Devices    []string
	HeadDigest [32]byte
}

// Resolution is the non-destructive result of combining device branches.
type Resolution struct {
	Kind         ResolutionKind
	CommonPrefix uint64
	Versions     []Version
}

// ResolveBranches removes only redundant prefix branches and preserves every
// incomparable maximal version.
func ResolveBranches(branches []Branch) (Resolution, error) {
	return resolveBranches(branches, true)
}

// ResolveBranchesOwned resolves branches by transferring their record
// buffers into the result. Callers must not mutate or reuse branches after
// this call; the function is intended for one-shot restore flows that release
// the input branches after resolution.
func ResolveBranchesOwned(branches []Branch) (Resolution, error) {
	return resolveBranches(branches, false)
}

func resolveBranches(branches []Branch, cloneInput bool) (Resolution, error) {
	if len(branches) == 0 {
		return Resolution{}, errors.New("syncer: at least one branch is required")
	}

	unique := make([]Version, 0, len(branches))
	for _, branch := range branches {
		if branch.DeviceID == "" {
			return Resolution{}, errors.New("syncer: branch has no device identity")
		}
		if len(branch.Records) == 0 {
			return Resolution{}, fmt.Errorf("%w: branch %q has no records", ErrIncompleteBranch, branch.DeviceID)
		}
		digest, err := DigestRecords(branch.Records)
		if err != nil {
			return Resolution{}, fmt.Errorf("validate branch %q: %w", branch.DeviceID, err)
		}
		if digest != branch.HeadDigest {
			return Resolution{}, fmt.Errorf("%w: branch %q has a mismatched head digest", ErrIncompleteBranch, branch.DeviceID)
		}
		found := -1
		for i := range unique {
			if recordsEqual(unique[i].Records, branch.Records) {
				found = i
				break
			}
		}
		if found < 0 {
			records := branch.Records
			if cloneInput {
				records = cloneRecords(records)
			}
			unique = append(unique, Version{
				Records:    records,
				Devices:    []string{branch.DeviceID},
				HeadDigest: branch.HeadDigest,
			})
		} else {
			unique[found].Devices = append(unique[found].Devices, branch.DeviceID)
		}
	}

	for i := range unique {
		sort.Strings(unique[i].Devices)
	}
	common := commonPrefixOfVersions(unique)

	maximal := make([]Version, 0, len(unique))
	for i, candidate := range unique {
		redundant := false
		for j, other := range unique {
			if i == j || len(candidate.Records) >= len(other.Records) {
				continue
			}
			if CompareRecords(candidate.Records, other.Records).Relation == LeftPrefix {
				redundant = true
				break
			}
		}
		if !redundant {
			maximal = append(maximal, candidate)
		}
	}

	sort.Slice(maximal, func(i, j int) bool {
		if len(maximal[i].Records) != len(maximal[j].Records) {
			return len(maximal[i].Records) < len(maximal[j].Records)
		}
		return firstDevice(maximal[i].Devices) < firstDevice(maximal[j].Devices)
	})

	kind := ResolutionConsistent
	if len(maximal) > 1 {
		kind = ResolutionFork
	} else if len(unique) > 1 {
		kind = ResolutionFastForward
	}
	return Resolution{Kind: kind, CommonPrefix: common, Versions: maximal}, nil
}

func cloneRecords(records [][]byte) [][]byte {
	out := make([][]byte, len(records))
	for i, record := range records {
		out[i] = append([]byte(nil), record...)
	}
	return out
}

func recordsEqual(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}
	return true
}

func firstDevice(devices []string) string {
	if len(devices) == 0 {
		return ""
	}
	return devices[0]
}

func commonPrefixOfVersions(versions []Version) uint64 {
	if len(versions) == 0 {
		return 0
	}
	common := versions[0].Records
	for _, version := range versions[1:] {
		comparison := CompareRecords(common, version.Records)
		common = common[:comparison.CommonPrefix]
	}
	return uint64(len(common))
}
