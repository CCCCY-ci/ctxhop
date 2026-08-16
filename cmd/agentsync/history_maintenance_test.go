package main

import (
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

func TestParseHistoryPruneOptions(t *testing.T) {
	options, err := parseHistoryPruneOptions([]string{"--keep", "2", "--yes", "native-session"})
	if err != nil {
		t.Fatal(err)
	}
	if options.keep != 2 || !options.yes || options.session != "native-session" {
		t.Fatalf("options = %+v", options)
	}
	options, err = parseHistoryPruneOptions([]string{"--before", "2026-08-15T00:00:00Z", "native-session"})
	if err != nil {
		t.Fatal(err)
	}
	if options.before == nil || !options.before.Equal(time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("before = %v", options.before)
	}
	if _, err := parseHistoryPruneOptions([]string{"native-session"}); err == nil {
		t.Fatal("prune without policy unexpectedly succeeded")
	}
	if _, err := parseHistoryPruneOptions([]string{"--keep", "1", "--before", "2026-08-15T00:00:00Z", "native-session"}); err == nil {
		t.Fatal("prune accepted two policies")
	}
}

func TestSelectHistoryPruneDevicesKeepsNewestVersionAndDeletesRedundantBranches(t *testing.T) {
	old := [][]byte{[]byte{'{', '"', 'n', '"', ':', '1', '}'}, []byte{'{', '"', 'n', '"', ':', '2', '}'}}
	newer := [][]byte{[]byte{'{', '"', 'n', '"', ':', '1', '}'}, []byte{'{', '"', 'n', '"', ':', '3', '}'}}
	prefix := [][]byte{[]byte{'{', '"', 'n', '"', ':', '1', '}'}}

	makeBranch := func(id string, records [][]byte) syncer.Branch {
		digest, err := syncer.DigestRecords(records)
		if err != nil {
			t.Fatal(err)
		}
		return syncer.Branch{DeviceID: id, Records: records, HeadDigest: digest}
	}
	branches := []syncer.Branch{
		makeBranch("devicea", old),
		makeBranch("deviceb", newer),
		makeBranch("devicec", prefix),
	}
	resolution, err := syncer.ResolveBranches(branches)
	if err != nil {
		t.Fatal(err)
	}
	metadata := []syncer.MetadataRef{
		historyPruneMetadata(t, "devicea", "native-a", time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC), old),
		historyPruneMetadata(t, "deviceb", "native-b", time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC), newer),
		historyPruneMetadata(t, "devicec", "native-c", time.Date(2026, 8, 15, 0, 30, 0, 0, time.UTC), prefix),
	}
	targets, retained := selectHistoryPruneDevices(metadata, resolution, branches, historyPruneOptions{keep: 1})
	if retained != 1 {
		t.Fatalf("retained versions = %d, want 1", retained)
	}
	if len(targets) != 2 || targets[0] != "devicea" || targets[1] != "devicec" {
		t.Fatalf("targets = %v, want devicea and redundant devicec", targets)
	}
}

func historyPruneMetadata(t *testing.T, deviceID, nativeID string, updated time.Time, records [][]byte) syncer.MetadataRef {
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		NativeID:  nativeID,
		Title:     nativeID,
		CreatedAt: updated.Add(-time.Hour),
		UpdatedAt: updated,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(uint64(len(records)), digest, payload)
	if err != nil {
		t.Fatal(err)
	}
	return syncer.MetadataRef{DeviceID: deviceID, Metadata: metadata}
}
