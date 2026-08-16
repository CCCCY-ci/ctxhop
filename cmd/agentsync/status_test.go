package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
)

func TestStatusJSONDoesNotExposeConfigurationValues(t *testing.T) {
	localRoot := filepath.Join(t.TempDir(), "private-project")
	c := config.New()
	c.Device = config.Device{ID: "deviceopaque", Name: "alice-workstation"}
	c.Remote = config.Remote{
		Type:     "s3",
		Endpoint: "https://storage.example.invalid/private-endpoint",
		Bucket:   "customer-private-bucket",
		Path:     localRoot,
	}
	c.IdentityPublic = []byte{1, 2, 3}
	c.Projects.Bindings = []config.Binding{{
		Identity:  "github.com/customer/private-project",
		LocalRoot: localRoot,
	}}
	c.Agents = map[string]config.AgentState{
		"z-agent": {HookInstalled: false},
		"a-agent": {HookInstalled: true},
	}

	report, err := collectStatus(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := writeStatusJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, secret := range []string{
		"deviceopaque",
		"alice-workstation",
		"storage.example.invalid",
		"customer-private-bucket",
		localRoot,
		"github.com/customer/private-project",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("status JSON contains %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, `"scope": "global"`) {
		t.Errorf("status JSON has no global scope: %s", text)
	}
	if !strings.Contains(text, `"configured": true`) {
		t.Errorf("status JSON lost readiness information: %s", text)
	}
}

func TestParseStatusOptions(t *testing.T) {
	options, err := parseStatusOptions([]string{"--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.json {
		t.Error("--json was not enabled")
	}

	if _, err := parseStatusOptions([]string{"unexpected"}); err == nil {
		t.Error("an unexpected positional argument was accepted")
	}
	if _, err := parseStatusOptions([]string{"--unknown"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

func TestStatusCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "status" {
			if command.run == nil {
				t.Fatal("status command has no handler")
			}
			return
		}
	}
	t.Fatal("status command is missing")
}
