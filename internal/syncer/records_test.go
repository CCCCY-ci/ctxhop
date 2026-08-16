package syncer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestShardRoundTripIsDeterministicAndDoesNotHTMLAlterRecords(t *testing.T) {
	prefix := EmptyDigest()
	records := [][]byte{[]byte(`{"message":"<keep & exact>"}`), []byte(`{"n":9007199254740993}`)}
	shard, err := NewShard(0, prefix, records)
	if err != nil {
		t.Fatal(err)
	}

	first, err := shard.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := shard.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("encoding the same shard twice produced different bytes")
	}
	if strings.Contains(string(first), `\u003c`) || strings.Contains(string(first), `\u0026`) {
		t.Fatalf("canonical record was HTML-escaped: %s", first)
	}

	got, err := ParseShard(first)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != 0 || got.Count() != 2 || got.PrefixDigest != prefix {
		t.Fatalf("decoded header = %+v", got)
	}
	for i := range records {
		if !bytes.Equal(got.Records[i], records[i]) {
			t.Errorf("record %d = %s, want %s", i, got.Records[i], records[i])
		}
	}
}

func TestParseShardRejectsMalformedEnvelope(t *testing.T) {
	digest := EmptyDigest()
	prefix := hex.EncodeToString(digest[:])
	valid := `{"version":1,"base":0,"count":1,"prefixDigest":"` + prefix + `","records":[{"ok":true}]}`

	tests := map[string]string{
		"empty":         "",
		"trailing json": valid + `{}`,
		"trailing text": valid + `x`,
		"unknown field": strings.TrimSuffix(valid, `}`) + `,"extra":1}`,
		"wrong version": strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"wrong count":   strings.Replace(valid, `"count":1`, `"count":2`, 1),
		"bad digest":    strings.Replace(valid, prefix, strings.Repeat("0", 63), 1),
		"pretty record": strings.Replace(valid, `{"ok":true}`, `{ "ok": true }`, 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseShard([]byte(input)); err == nil || !errors.Is(err, ErrInvalidShard) {
				t.Fatalf("ParseShard(%q) error = %v, want ErrInvalidShard", name, err)
			}
		})
	}
}

func TestNewShardRejectsNonCanonicalRecords(t *testing.T) {
	for _, record := range [][]byte{
		{},
		[]byte(`not json`),
		[]byte("{\"ok\":true}\n"),
		[]byte(`{ "ok": true }`),
	} {
		if _, err := NewShard(0, EmptyDigest(), [][]byte{record}); err == nil || !errors.Is(err, ErrInvalidShard) {
			t.Errorf("record %q error = %v, want ErrInvalidShard", record, err)
		}
	}
}

func TestAssembleBranchSortsAndChecksContinuity(t *testing.T) {
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`), []byte(`{"n":3}`)}
	prefix, err := DigestRecords(records[:2])
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewShard(0, EmptyDigest(), records[:2])
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewShard(2, prefix, records[2:])
	if err != nil {
		t.Fatal(err)
	}

	branch, err := AssembleBranch("device-a", []ShardPart{{Number: 2, Shard: second}, {Number: 1, Shard: first}})
	if err != nil {
		t.Fatal(err)
	}
	if len(branch.Records) != 3 || branch.HeadDigest != second.Digest() {
		t.Fatalf("assembled branch = %+v", branch)
	}

	for name, parts := range map[string][]ShardPart{
		"gap":       {{Number: 1, Shard: first}, {Number: 3, Shard: second}},
		"duplicate": {{Number: 1, Shard: first}, {Number: 1, Shard: second}},
		"base":      {{Number: 1, Shard: second}},
		"digest":    {{Number: 1, Shard: mustShard(t, 0, [32]byte{1}, records[:2])}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AssembleBranch("device-a", parts); err == nil || !errors.Is(err, ErrIncompleteBranch) {
				t.Fatalf("AssembleBranch error = %v, want ErrIncompleteBranch", err)
			}
		})
	}
}

func TestCompareAndResolveBranches(t *testing.T) {
	base := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	left := append(cloneRecords(base), []byte(`{"n":3}`))
	right := append(cloneRecords(base), []byte(`{"n":4}`))

	comparison := CompareRecords(base, left)
	if comparison.Relation != LeftPrefix || comparison.CommonPrefix != 2 {
		t.Fatalf("comparison = %+v", comparison)
	}
	if CompareRecords(left, base).Relation != RightPrefix {
		t.Fatal("reverse prefix relation was not detected")
	}
	if CompareRecords(left, right).Relation != Diverged {
		t.Fatal("divergent suffixes were not detected")
	}

	baseBranch := testBranch(t, "device-a", base)
	longBranch := testBranch(t, "device-b", left)
	resolution, err := ResolveBranches([]Branch{longBranch, baseBranch})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != ResolutionFastForward || len(resolution.Versions) != 1 || len(resolution.Versions[0].Records) != 3 {
		t.Fatalf("fast-forward resolution = %+v", resolution)
	}

	resolution, err = ResolveBranches([]Branch{testBranch(t, "device-a", left), testBranch(t, "device-b", left)})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != ResolutionConsistent || len(resolution.Versions) != 1 || len(resolution.Versions[0].Devices) != 2 {
		t.Fatalf("consistent resolution = %+v", resolution)
	}

	resolution, err = ResolveBranches([]Branch{testBranch(t, "device-a", left), testBranch(t, "device-b", right)})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Kind != ResolutionFork || resolution.CommonPrefix != 2 || len(resolution.Versions) != 2 {
		t.Fatalf("fork resolution = %+v", resolution)
	}
}

func TestParseShardFuzzSeed(t *testing.T) {
	digest := EmptyDigest()
	prefix := hex.EncodeToString(digest[:])
	seed := []byte(`{"version":1,"base":0,"count":1,"prefixDigest":"` + prefix + `","records":[{"ok":true}]}`)
	if _, err := ParseShard(seed); err != nil {
		t.Fatal(err)
	}
}

func mustShard(t *testing.T, base uint64, prefix [32]byte, records [][]byte) Shard {
	t.Helper()
	shard, err := NewShard(base, prefix, records)
	if err != nil {
		t.Fatal(err)
	}
	return shard
}

func testBranch(t *testing.T, device string, records [][]byte) Branch {
	t.Helper()
	branch, err := AssembleBranch(device, []ShardPart{{Number: 1, Shard: mustShard(t, 0, EmptyDigest(), records)}})
	if err != nil {
		t.Fatal(err)
	}
	return branch
}
