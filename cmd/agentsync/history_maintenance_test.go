package main

import (
	"context"
	"errors"
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

func TestSelectHistoryPruneDevicesBeforeKeepsUnknownAndBoundaryVersions(t *testing.T) {
	oldRecords := [][]byte{[]byte(`{"version":"old"}`)}
	boundaryRecords := [][]byte{[]byte(`{"version":"boundary"}`)}
	unknownRecords := [][]byte{[]byte(`{"version":"unknown"}`)}
	makeBranch := func(id string, records [][]byte) syncer.Branch {
		t.Helper()
		digest, err := syncer.DigestRecords(records)
		if err != nil {
			t.Fatal(err)
		}
		return syncer.Branch{DeviceID: id, Records: records, HeadDigest: digest}
	}
	branches := []syncer.Branch{
		makeBranch("old", oldRecords),
		makeBranch("boundary", boundaryRecords),
		makeBranch("unknown", unknownRecords),
	}
	resolution, err := syncer.ResolveBranches(branches)
	if err != nil {
		t.Fatal(err)
	}
	unknownDigest, err := syncer.DigestRecords(unknownRecords)
	if err != nil {
		t.Fatal(err)
	}
	unknownMetadata, err := syncer.NewMetadata(uint64(len(unknownRecords)), unknownDigest, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	metadata := []syncer.MetadataRef{
		historyPruneMetadata(t, "old", "native-old", time.Date(2026, 8, 15, 0, 59, 0, 0, time.UTC), oldRecords),
		historyPruneMetadata(t, "boundary", "native-boundary", time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC), boundaryRecords),
		{DeviceID: "unknown", Metadata: unknownMetadata},
	}
	before := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	targets, retained := selectHistoryPruneDevices(metadata, resolution, branches, historyPruneOptions{before: &before})
	if retained != 2 {
		t.Fatalf("retained versions = %d, want 2", retained)
	}
	if len(targets) != 1 || targets[0] != "old" {
		t.Fatalf("targets = %v, want [old]", targets)
	}
}

func TestSafeHistoryPruneReadErrorFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "missing metadata", err: syncer.ErrNoRemoteMetadata, want: "history prune: no complete remote versions are available"},
		{name: "missing branches", err: syncer.ErrNoRemoteBranches, want: "history prune: no complete remote versions are available"},
		{name: "generic remote failure", err: errors.New("bucket credentials leaked"), want: "history prune: remote session could not be read safely"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := safeHistoryPruneReadError(context.Background(), test.err); got == nil || got.Error() != test.want {
				t.Fatalf("error = %v, want %q", got, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := safeHistoryPruneReadError(ctx, errors.New("remote read failed"))
	if got == nil || !errors.Is(got, context.Canceled) {
		t.Fatalf("cancelled error = %v, want context.Canceled", got)
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
