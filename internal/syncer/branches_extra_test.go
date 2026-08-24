package syncer

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestRelationAndResolutionNames(t *testing.T) {
	for relation, want := range map[Relation]string{
		Equal:        "equal",
		LeftPrefix:   "left-prefix",
		RightPrefix:  "right-prefix",
		Diverged:     "diverged",
		Relation(99): "unknown",
	} {
		if got := relation.String(); got != want {
			t.Errorf("Relation(%d).String() = %q, want %q", relation, got, want)
		}
	}
	for kind, want := range map[ResolutionKind]string{
		ResolutionConsistent:  "consistent",
		ResolutionFastForward: "fast-forward",
		ResolutionFork:        "fork",
		ResolutionKind(99):    "unknown",
	} {
		if got := kind.String(); got != want {
			t.Errorf("ResolutionKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

func TestCompareRecordsHandlesEmptyAndEqualSequences(t *testing.T) {
	record := [][]byte{[]byte(`{"ok":true}`)}
	tests := []struct {
		name        string
		left, right [][]byte
		want        Relation
	}{
		{"both empty", nil, nil, Equal},
		{"left empty", nil, record, LeftPrefix},
		{"right empty", record, nil, RightPrefix},
		{"equal", record, cloneRecords(record), Equal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CompareRecords(test.left, test.right); got.Relation != test.want {
				t.Fatalf("CompareRecords() = %+v, want %s", got, test.want)
			}
		})
	}
}

func TestAssembleBranchRejectsInvalidInputs(t *testing.T) {
	valid := mustShard(t, 0, EmptyDigest(), [][]byte{[]byte(`{"ok":true}`)})
	tests := map[string]struct {
		device string
		parts  []ShardPart
	}{
		"missing device": {"", []ShardPart{{Number: 1, Shard: valid}}},
		"missing shards": {"device", nil},
		"zero sequence":  {"device", []ShardPart{{Shard: valid}}},
		"invalid shard":  {"device", []ShardPart{{Number: 1}}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := AssembleBranch(test.device, test.parts)
			if err == nil {
				t.Fatal("AssembleBranch unexpectedly succeeded")
			}
			if name != "missing device" && !errors.Is(err, ErrIncompleteBranch) {
				t.Fatalf("error = %v, want ErrIncompleteBranch", err)
			}
		})
	}
}

func TestResolveBranchesRejectsInvalidInputs(t *testing.T) {
	valid := testBranch(t, "device", [][]byte{[]byte(`{"ok":true}`)})
	tests := map[string][]Branch{
		"no branches":     nil,
		"missing device":  {{Records: valid.Records, HeadDigest: valid.HeadDigest}},
		"empty records":   {{DeviceID: "device", HeadDigest: EmptyDigest()}},
		"bad record":      {{DeviceID: "device", Records: [][]byte{[]byte("no")}, HeadDigest: EmptyDigest()}},
		"bad head digest": {{DeviceID: "device", Records: valid.Records}},
	}
	for name, branches := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveBranches(branches); err == nil {
				t.Fatal("ResolveBranches unexpectedly succeeded")
			}
		})
	}
}

func TestRecordValidationAndDigestFailures(t *testing.T) {
	if _, err := DigestRecords([][]byte{[]byte(`{ "not":"compact" }`)}); err == nil || !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("DigestRecords error = %v, want ErrInvalidRecord", err)
	}
	if err := (Shard{}).Validate(); err == nil || !errors.Is(err, ErrInvalidShard) {
		t.Fatalf("empty Shard.Validate() = %v, want ErrInvalidShard", err)
	}
	if _, err := (Shard{}).MarshalBinary(); err == nil || !errors.Is(err, ErrInvalidShard) {
		t.Fatalf("empty Shard.MarshalBinary() = %v, want ErrInvalidShard", err)
	}
}

func TestParseShardRejectsBadDigestEncodingAndOversizeInput(t *testing.T) {
	digest := EmptyDigest()
	prefix := hex.EncodeToString(digest[:])
	valid := `{"version":1,"base":0,"count":1,"prefixDigest":"` + prefix + `","records":[{"ok":true}]}`
	for name, input := range map[string]string{
		"uppercase": strings.ToUpper(valid),
		"nonhex":    strings.Replace(valid, prefix, strings.Repeat("g", 64), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseShard([]byte(input)); err == nil || !errors.Is(err, ErrInvalidShard) {
				t.Fatalf("ParseShard error = %v, want ErrInvalidShard", err)
			}
		})
	}
	if _, err := ParseShard(make([]byte, maxShardBytes+1)); err == nil || !errors.Is(err, ErrInvalidShard) {
		t.Fatalf("oversize ParseShard error = %v, want ErrInvalidShard", err)
	}
}

func TestCommonPrefixAndFastForwardReportTheInputPrefix(t *testing.T) {
	short := testBranch(t, "short", [][]byte{[]byte(`{"n":1}`)})
	long := testBranch(t, "long", [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)})
	resolution, err := ResolveBranches([]Branch{short, long})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != ResolutionFastForward || resolution.CommonPrefix != 1 {
		t.Fatalf("resolution = %+v, want fast-forward with common prefix 1", resolution)
	}
	if got := commonPrefixOfVersions(nil); got != 0 {
		t.Fatalf("empty common prefix = %d, want 0", got)
	}
	if got := firstDevice(nil); got != "" {
		t.Fatalf("empty first device = %q, want empty", got)
	}
}
