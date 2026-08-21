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

	var (
		nextNumber uint64 = 1
		base       uint64
		digest     = EmptyDigest()
		records    [][]byte
		totalBytes uint64
	)
	for i, part := range ordered {
		if part.Number == 0 || part.Number != nextNumber {
			return Branch{}, fmt.Errorf("%w: device %q has a gap near shard %d", ErrIncompleteBranch, deviceID, part.Number)
		}
		if err := part.Shard.Validate(); err != nil {
			return Branch{}, fmt.Errorf("%w: device %q shard %d: %v", ErrIncompleteBranch, deviceID, part.Number, err)
		}
		if part.Shard.Base != base {
			return Branch{}, fmt.Errorf("%w: device %q shard %d starts at %d, want %d", ErrIncompleteBranch, deviceID, part.Number, part.Shard.Base, base)
		}
		if part.Shard.PrefixDigest != digest {
			return Branch{}, fmt.Errorf("%w: device %q shard %d has a mismatched prefix digest", ErrIncompleteBranch, deviceID, part.Number)
		}
		shardCount := part.Shard.Count()
		if ^uint64(0)-base < shardCount {
			return Branch{}, fmt.Errorf("%w: device %q record count overflows", ErrIncompleteBranch, deviceID)
		}
		if shardCount > maxSessionRecords || uint64(len(records)) > maxSessionRecords-shardCount {
			return Branch{}, fmt.Errorf("%w: device %q has more than %d records", ErrSessionTooLarge, deviceID, maxSessionRecords)
		}
		for _, record := range part.Shard.Records {
			recordBytes := uint64(len(record))
			if recordBytes > maxSessionBytes || totalBytes > maxSessionBytes-recordBytes {
				return Branch{}, fmt.Errorf("%w: device %q has more than %d record bytes", ErrSessionTooLarge, deviceID, maxSessionBytes)
			}
			totalBytes += recordBytes
		}

		records = append(records, cloneRecords(part.Shard.Records)...)
		base += shardCount
		digest = part.Shard.Digest()
		if i == len(ordered)-1 {
			break
		}
		if nextNumber == ^uint64(0) {
			return Branch{}, fmt.Errorf("%w: device %q has too many shards", ErrIncompleteBranch, deviceID)
		}
		nextNumber++
	}

	return Branch{DeviceID: deviceID, Records: records, HeadDigest: digest}, nil
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
			unique = append(unique, Version{
				Records:    cloneRecords(branch.Records),
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
