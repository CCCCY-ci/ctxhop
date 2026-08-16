package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestParseStatsOptions(t *testing.T) {
	options, err := parseStatsOptions([]string{"--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.json {
		t.Fatal("--json was not enabled")
	}
	for _, args := range [][]string{
		{"unexpected"},
		{"--unknown"},
	} {
		if _, err := parseStatsOptions(args); err == nil {
			t.Errorf("parseStatsOptions(%v) accepted invalid input", args)
		}
	}
}

func TestStatsCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "stats" {
			if command.run == nil {
				t.Fatal("stats command has no handler")
			}
			return
		}
	}
	t.Fatal("stats command is missing")
}

func TestCollectStatsWithoutConfiguration(t *testing.T) {
	root := t.TempDir()
	store, err := syncer.NewRestoreStatsStore(root)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 15, 2, 5, 0, 0, time.UTC)
	if _, err := store.RecordRestore(context.Background(), "localdevice", []string{"foreign"}, when); err != nil {
		t.Fatal(err)
	}

	report, err := collectStats(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scope != "local" || report.CrossDeviceRestores != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.LastRestoredAt == nil || !report.LastRestoredAt.Equal(when) {
		t.Fatalf("lastRestoredAt = %v, want %v", report.LastRestoredAt, when)
	}
}

func TestStatsOutputIsAggregateOnly(t *testing.T) {
	when := time.Date(2026, 8, 15, 2, 5, 0, 0, time.UTC)
	last := when
	report := statsReport{
		Scope:               "local",
		CrossDeviceRestores: 3,
		LastRestoredAt:      &last,
	}

	var textOutput bytes.Buffer
	if err := writeStatsText(&textOutput, report); err != nil {
		t.Fatal(err)
	}
	wantText := "scope: local\ncross-device-restores: 3\nlast-restored: 2026-08-15T02:05:00Z\n"
	if textOutput.String() != wantText {
		t.Fatalf("text output = %q, want %q", textOutput.String(), wantText)
	}

	var jsonOutput bytes.Buffer
	if err := writeStatsJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	var decoded statsReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope != "local" || decoded.CrossDeviceRestores != 3 || decoded.LastRestoredAt == nil || !decoded.LastRestoredAt.Equal(when) {
		t.Fatalf("JSON report = %+v", decoded)
	}
	for _, forbidden := range []string{
		"project",
		"session",
		"device",
		"path",
		"backend",
	} {
		if strings.Contains(jsonOutput.String(), forbidden) {
			t.Fatalf("JSON output contains forbidden field %q: %s", forbidden, jsonOutput.String())
		}
	}
}

func TestStatsZeroOutputOmitsTimestamp(t *testing.T) {
	report := statsReport{Scope: "local"}
	var textOutput bytes.Buffer
	if err := writeStatsText(&textOutput, report); err != nil {
		t.Fatal(err)
	}
	if want := "scope: local\ncross-device-restores: 0\nlast-restored: never\n"; textOutput.String() != want {
		t.Fatalf("zero text output = %q, want %q", textOutput.String(), want)
	}

	var jsonOutput bytes.Buffer
	if err := writeStatsJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOutput.String(), "lastRestoredAt") {
		t.Fatalf("zero JSON output contains timestamp: %s", jsonOutput.String())
	}
}
