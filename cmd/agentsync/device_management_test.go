package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestParseDeviceManagementOptions(t *testing.T) {
	list, err := parseDeviceOptions([]string{"list", "--json"})
	if err != nil {
		t.Fatalf("list options: %v", err)
	}
	if list.action != deviceActionList || !list.json {
		t.Fatalf("list options = %+v", list)
	}

	rename, err := parseDeviceOptions([]string{"rename", "office"})
	if err != nil {
		t.Fatalf("rename options: %v", err)
	}
	if rename.action != deviceActionRename || rename.name != "office" {
		t.Fatalf("rename options = %+v", rename)
	}

	remove, err := parseDeviceOptions([]string{"remove", "--yes", "deviceb"})
	if err != nil {
		t.Fatalf("remove options: %v", err)
	}
	if remove.action != deviceActionRemove || remove.target != "deviceb" || !remove.yes {
		t.Fatalf("remove options = %+v", remove)
	}
}

func TestMergeDeviceListSortsAndKeepsExplicitRecords(t *testing.T) {
	lastActive := time.Date(2026, 8, 15, 2, 3, 4, 0, time.UTC)
	record, err := syncer.NewDeviceRecord("deviceb", "office", "windows", lastActive)
	if err != nil {
		t.Fatal(err)
	}
	report := mergeDeviceList("deviceb", []syncer.DeviceRecord{record}, []syncer.DeviceActivity{
		{DeviceID: "devicec", LastActivityAt: lastActive.Add(time.Minute)},
		{DeviceID: "deviceb", LastActivityAt: lastActive.Add(time.Hour)},
	})
	if len(report.Devices) != 2 {
		t.Fatalf("devices = %+v, want 2 entries", report.Devices)
	}
	if report.Devices[0].ID != "deviceb" || report.Devices[1].ID != "devicec" {
		t.Fatalf("device order = %+v", report.Devices)
	}
	if report.Devices[0].Name != "office" || !report.Devices[0].Local {
		t.Fatalf("explicit local device = %+v", report.Devices[0])
	}
	if report.Devices[1].Name != "unknown" || report.Devices[1].System != "unknown" {
		t.Fatalf("legacy device = %+v", report.Devices[1])
	}
	if !report.Devices[0].LastActiveAt.Equal(lastActive) {
		t.Fatalf("explicit timestamp was replaced: %v", report.Devices[0].LastActiveAt)
	}
}

func TestWriteDeviceListTextAndConfirmRemoval(t *testing.T) {
	lastActive := time.Date(2026, 8, 15, 2, 3, 4, 0, time.UTC)
	report := deviceListReport{
		Scope: "remote",
		Devices: []deviceListItem{{
			ID:           "devicea",
			Name:         "office",
			System:       "windows",
			LastActiveAt: &lastActive,
			Local:        true,
		}},
	}
	var output bytes.Buffer
	if err := writeDeviceListText(&output, report); err != nil {
		t.Fatal(err)
	}
	want := "scope: remote\ndevices: 1\n- id=devicea name=office system=windows last-active=2026-08-15T02:03:04Z local\n"
	if output.String() != want {
		t.Fatalf("text output = %q, want %q", output.String(), want)
	}

	var prompt bytes.Buffer
	confirmed, err := confirmDeviceRemoval(strings.NewReader("yes\n"), &prompt, "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed || !strings.Contains(prompt.String(), "deviceb") {
		t.Fatalf("confirmation = %v, prompt = %q", confirmed, prompt.String())
	}
	cancelled, err := confirmDeviceRemoval(strings.NewReader("no\n"), &prompt, "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled {
		t.Fatal("negative confirmation was accepted")
	}
}
