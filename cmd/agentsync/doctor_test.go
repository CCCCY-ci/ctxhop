package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestDoctorReportProbesDirectoryWithoutLeakingValues(t *testing.T) {
	remoteRoot := t.TempDir()
	projectRoot := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude-home"))

	c := config.New()
	c.Device = config.Device{ID: "deviceopaque", Name: "private-workstation"}
	c.IdentityPublic = []byte{1, 2, 3}
	c.Remote = config.Remote{
		Type:     "dir",
		Path:     remoteRoot,
		Endpoint: "https://private-endpoint.invalid",
		Bucket:   "private-bucket",
	}

	report, err := collectDoctor(c, t.TempDir(), projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if report.Backend.Status != "passed" {
		t.Fatalf("backend status = %+v", report.Backend)
	}
	if report.Agent.Installed {
		t.Error("an absent Claude home was reported as installed")
	}

	var output bytes.Buffer
	if err := writeDoctorJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, secret := range []string{
		remoteRoot,
		projectRoot,
		"private-workstation",
		"private-endpoint.invalid",
		"private-bucket",
		"deviceopaque",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("doctor JSON contains %q: %s", secret, text)
		}
	}
}

func TestDoctorClassifiesMissingS3Credentials(t *testing.T) {
	t.Setenv("AGENTSYNC_ACCESS_KEY_ID", "")
	t.Setenv("AGENTSYNC_SECRET_ACCESS_KEY", "")
	t.Setenv("AGENTSYNC_SESSION_TOKEN", "")
	c := config.New()
	c.Remote = config.Remote{
		Type:     "s3",
		Endpoint: "https://private-endpoint.invalid",
		Bucket:   "private-bucket",
	}

	report, err := collectDoctor(c, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if report.Backend.Status != "not-configured" {
		t.Errorf("backend status = %+v", report.Backend)
	}
	if !strings.Contains(report.Backend.Reason, "credentials") {
		t.Errorf("backend reason = %q", report.Backend.Reason)
	}
	if strings.Contains(report.Backend.Reason, "private-") {
		t.Errorf("backend reason leaked configuration values: %q", report.Backend.Reason)
	}
}

func TestDoctorSanitizesAgentVersionAndProbeErrors(t *testing.T) {
	if got := safeAgentVersion(`C:\Users\alice\session`); got != "unknown" {
		t.Errorf("safeAgentVersion(path) = %q", got)
	}
	if got := safeAgentVersion("2.1.227"); got != "2.1.227" {
		t.Errorf("safeAgentVersion(version) = %q", got)
	}
	if got := safeBackendProbeError(errors.New(`bucket private-bucket at C:\secret`)); strings.Contains(got, "private-") || strings.Contains(got, `C:\`) {
		t.Errorf("safeBackendProbeError leaked input: %q", got)
	}
}

func TestDoctorCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "doctor" {
			if command.run == nil {
				t.Fatal("doctor command has no handler")
			}
			return
		}
	}
	t.Fatal("doctor command is missing")
}

func TestParseDoctorOptions(t *testing.T) {
	jsonOutput, err := parseDoctorOptions([]string{"--json"})
	if err != nil || !jsonOutput {
		t.Fatalf("parseDoctorOptions(--json) = %v, %v", jsonOutput, err)
	}
	if _, err := parseDoctorOptions([]string{"unexpected"}); err == nil {
		t.Error("an unexpected positional argument was accepted")
	}
}
