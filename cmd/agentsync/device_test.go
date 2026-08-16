package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestParseDeviceOptions(t *testing.T) {
	status, err := parseDeviceOptions([]string{"status", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if status.action != deviceActionStatus || !status.json {
		t.Fatalf("status options = %+v", status)
	}

	mode, err := parseDeviceOptions([]string{"mode", " PUSH-ONLY "})
	if err != nil {
		t.Fatal(err)
	}
	if mode.action != deviceActionMode || mode.mode != config.DeviceModePushOnly {
		t.Fatalf("mode options = %+v", mode)
	}

	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"mode"},
		{"mode", "unknown"},
		{"mode", "normal", "extra"},
		{"status", "extra"},
		{"status", "--unknown"},
	} {
		if _, err := parseDeviceOptions(args); err == nil {
			t.Errorf("parseDeviceOptions(%v) accepted invalid input", args)
		}
	}
}

func TestCollectDeviceStatusRedactsIdentityAndName(t *testing.T) {
	c := config.New()
	c.Device = config.Device{
		ID:   "opaque-device-id",
		Name: "private-workstation",
	}
	c.Remote = config.Remote{
		Type: "s3",
	}

	report, err := collectDeviceStatus(c)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := writeDeviceStatusJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\"mode\": \"normal\"") {
		t.Errorf("status JSON = %s", output.String())
	}
	for _, secret := range []string{"opaque-device-id", "private-workstation"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("status JSON contains %q: %s", secret, output.String())
		}
	}

	var decoded deviceStatusReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Device.Configured || decoded.Device.Mode != string(config.DeviceModeNormal) {
		t.Fatalf("decoded status = %+v", decoded)
	}
}

func TestRunDeviceStatusDoesNotReadBackendSecrets(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", configDir)

	c := config.New()
	c.Device.ID = "deviceid"
	c.Remote = config.Remote{
		Type:     "s3",
		Endpoint: "https://storage.example.invalid",
		Bucket:   "private-bucket",
	}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runDeviceWithIO([]string{"status"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "mode: normal") {
		t.Errorf("status output = %q", output.String())
	}
}

func TestRunDeviceModePersistsOnlyTheMode(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENTSYNC_CONFIG_DIR", configDir)

	c := config.New()
	c.Device = config.Device{
		ID:   "deviceid",
		Name: "workstation",
	}
	c.Remote = config.Remote{
		Type: "dir",
		Path: filepath.Join(t.TempDir(), "remote"),
	}
	c.Projects.Bindings = []config.Binding{{
		Identity:  "provider/project",
		LocalRoot: filepath.Join(t.TempDir(), "project"),
	}}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runDeviceWithIO([]string{"mode", " PUSH-ONLY "}, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "device mode: push-only\n" {
		t.Errorf("mode output = %q", output.String())
	}

	updated, err := config.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Device.Mode != config.DeviceModePushOnly {
		t.Fatalf("mode = %q", updated.Device.Mode)
	}
	if updated.Device.ID != c.Device.ID || updated.Device.Name != c.Device.Name {
		t.Fatalf("device identity changed: before=%+v after=%+v", c.Device, updated.Device)
	}
	if len(updated.Projects.Bindings) != 1 {
		t.Fatalf("bindings = %+v", updated.Projects.Bindings)
	}
}

func TestSaveDeviceModeRestoresMemoryOnWriteFailure(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(configDir, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := config.New()
	c.Remote.Type = "dir"
	c.Device.Mode = config.DeviceModeNormal

	err := saveDeviceMode(configDir, c, config.DeviceModeDisabled)
	if err == nil {
		t.Fatal("saveDeviceMode succeeded with a file as the config directory")
	}
	if c.Device.Mode != config.DeviceModeNormal {
		t.Fatalf("mode after failed save = %q", c.Device.Mode)
	}
}

func TestDeviceCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "device" {
			if command.run == nil {
				t.Fatal("device command has no handler")
			}
			return
		}
	}
	t.Fatal("device command is missing")
}

func TestDeviceStatusText(t *testing.T) {
	report := deviceStatusReport{
		Device: deviceStatus{Configured: false, Mode: string(config.DeviceModeDisabled)},
	}
	var output bytes.Buffer
	if err := writeDeviceStatusText(&output, report); err != nil {
		t.Fatal(err)
	}
	want := "device:\n  identity: not configured\n  mode: disabled\n"
	if output.String() != want {
		t.Errorf("status text = %q, want %q", output.String(), want)
	}
}

func TestSaveDeviceModeRejectsInvalidMode(t *testing.T) {
	c := config.New()
	c.Remote.Type = "dir"
	c.Device.Mode = config.DeviceModeNormal
	if err := saveDeviceMode(t.TempDir(), c, config.DeviceMode("invalid")); err == nil {
		t.Fatal("saveDeviceMode accepted an invalid mode")
	}
	if c.Device.Mode != config.DeviceModeNormal {
		t.Fatalf("mode after invalid input = %q", c.Device.Mode)
	}
}
