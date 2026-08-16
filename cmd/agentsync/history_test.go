package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

func TestParseHistoryOptions(t *testing.T) {
	options, err := parseHistoryOptions([]string{"--json", "native-session"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.json || options.session != "native-session" {
		t.Fatalf("options = %+v", options)
	}

	for _, args := range [][]string{
		nil,
		{"--unknown", "native-session"},
		{"native-session", "extra"},
		{"--json"},
	} {
		if _, err := parseHistoryOptions(args); err == nil {
			t.Errorf("parseHistoryOptions(%v) accepted invalid input", args)
		}
	}
}

func TestHistoryCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "history" {
			if command.run == nil {
				t.Fatal("history command has no handler")
			}
			return
		}
	}
	t.Fatal("history command is missing")
}

func TestHistoryVersionReportOutput(t *testing.T) {
	records := [][]byte{[]byte("{\"n\":1}"), []byte("{\"n\":2}")}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	ref := adapter.SessionRef{
		NativeID:  "native-session",
		Title:     "continue the migration",
		CreatedAt: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 2, 5, 0, 0, time.UTC),
	}
	payload, err := syncflow.EncodeSessionSummary(ref)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(uint64(len(records)), digest, payload)
	if err != nil {
		t.Fatal(err)
	}
	report := buildHistoryReport(
		"localdevice",
		historyCandidate{
			Group: syncer.ProjectMetadataRef{
				SessionID: "remote-session",
				Devices:   []syncer.MetadataRef{{DeviceID: "localdevice", Metadata: metadata}},
			},
			Summary:    syncflow.SessionSummary{NativeID: ref.NativeID, Title: ref.Title},
			HasSummary: true,
		},
		syncer.Resolution{
			Kind:         syncer.ResolutionConsistent,
			CommonPrefix: uint64(len(records)),
			Versions: []syncer.Version{{
				Records:    records,
				Devices:    []string{"localdevice"},
				HeadDigest: digest,
			}},
		},
	)

	var textOutput bytes.Buffer
	if err := writeHistoryText(&textOutput, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"scope: session\n",
		"session: native-session\n",
		"resolution: consistent\n",
		"common-prefix: 2\n",
		"- version=0 records=2 updated=2026-08-15T02:05:00Z sources=local\n",
	} {
		if !strings.Contains(textOutput.String(), want) {
			t.Errorf("text output missing %q:\n%s", want, textOutput.String())
		}
	}

	var jsonOutput bytes.Buffer
	if err := writeHistoryJSON(&jsonOutput, report); err != nil {
		t.Fatal(err)
	}
	var decoded historyReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Resolution != "consistent" || len(decoded.Versions) != 1 || decoded.Versions[0].RecordCount != 2 {
		t.Fatalf("JSON report = %+v", decoded)
	}
	if decoded.Versions[0].UpdatedAt == nil || !decoded.Versions[0].UpdatedAt.Equal(ref.UpdatedAt) {
		t.Fatalf("JSON updatedAt = %v", decoded.Versions[0].UpdatedAt)
	}
}

func TestCollectHistoryStopsAtDeviceBoundary(t *testing.T) {
	for _, mode := range []config.DeviceMode{config.DeviceModePushOnly, config.DeviceModeDisabled} {
		c := config.New()
		c.Device.ID = "localdevice"
		c.Device.Mode = mode
		c.Remote.Type = "dir"
		_, err := collectHistory(
			context.Background(),
			c,
			t.TempDir(),
			filepath.Join(t.TempDir(), "not-a-project"),
			"native-session",
			strings.NewReader("passphrase\n"),
			&bytes.Buffer{},
		)
		if err == nil || !strings.Contains(err.Error(), string(mode)) {
			t.Fatalf("mode %q error = %v", mode, err)
		}
	}
}
