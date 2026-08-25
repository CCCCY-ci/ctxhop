package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fullConfig is a configuration with every field populated, using invented
// values that are distinctive enough to search for in file contents.
func fullConfig() *Config {
	c := New()
	c.Device = Device{ID: "dev-abcdef", Name: "workstation-北京"}
	c.Remote = Remote{
		Type:      "s3",
		Endpoint:  "https://storage.example.invalid",
		Bucket:    "acme-private-bucket",
		Region:    "eu-west-1",
		Prefix:    "ctxhop/",
		PathStyle: true,
	}
	c.IdentityPublic = []byte{1, 2, 3, 4}
	c.DomainFingerprint = "domain-fingerprint-test"
	c.Projects = Projects{
		Bindings: []Binding{
			{Identity: "github.com/acme/secret-app", LocalRoot: filepath.Join("C:", "work", "secret app")},
		},
		Excluded: []string{"github.com/acme/internal-tools"},
		PushOnly: []string{"github.com/acme/work-laptop-only"},
	}
	c.Agents = map[string]AgentState{"claude-code": {HookInstalled: true}}
	return c
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := fullConfig()

	if err := want.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Device != want.Device {
		t.Errorf("device = %+v, want %+v", got.Device, want.Device)
	}
	if got.Remote != want.Remote {
		t.Errorf("remote = %+v, want %+v", got.Remote, want.Remote)
	}
	if got.DomainFingerprint != want.DomainFingerprint {
		t.Errorf("domain fingerprint = %q, want %q", got.DomainFingerprint, want.DomainFingerprint)
	}
	if len(got.Projects.Bindings) != 1 || got.Projects.Bindings[0] != want.Projects.Bindings[0] {
		t.Errorf("bindings = %+v", got.Projects.Bindings)
	}
	if got.Agents["claude-code"] != want.Agents["claude-code"] {
		t.Errorf("agents = %+v", got.Agents)
	}
	if got.HookScope != want.HookScope {
		t.Errorf("hook scope = %q, want %q", got.HookScope, want.HookScope)
	}
	if got.SyncConfig != want.SyncConfig {
		t.Errorf("sync config = %q, want %q", got.SyncConfig, want.SyncConfig)
	}
}

func TestConfigSyncModeRoundTripAndLegacyDefault(t *testing.T) {
	dir := t.TempDir()
	want := fullConfig()
	want.SyncConfig = ConfigSyncDisabled
	if err := want.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncConfig != ConfigSyncDisabled || got.SyncConfigEnabled() {
		t.Fatalf("disabled sync config = %q, enabled=%v", got.SyncConfig, got.SyncConfigEnabled())
	}

	legacy := filepath.Join(t.TempDir(), configFile)
	if err := os.WriteFile(legacy, []byte(`{"version":1,"remote":{"type":"dir","path":"remote"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyConfig, err := Load(filepath.Dir(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if legacyConfig.SyncConfigEnabled() == false {
		t.Fatal("legacy configuration unexpectedly disabled filtered Agent configuration")
	}
}

func TestConfigRejectsUnknownSyncMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(`{"version":1,"remote":{"type":"dir","path":"remote"},"syncConfig":"unexpected"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "unsupported config sync mode") {
		t.Fatalf("Load error = %v", err)
	}
}

// TestTheConfigFileHoldsNoSecret is the single most important assertion in this
// package. It reads the bytes rather than the struct, because the question is
// what landed on disk (spec §4.1).
func TestTheConfigFileHoldsNoSecret(t *testing.T) {
	dir := t.TempDir()
	if err := fullConfig().Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := SaveSecrets(dir, &Secrets{
		Credentials:   Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY"},
		IdentifierKey: []byte("0123456789abcdef0123456789abcdef"),
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", "0123456789abcdef"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("the configuration file contains %q", secret)
		}
	}
}

// TestADamagedConfigIsNeverRebuilt. Bindings, exclusions and the device name all
// live in this file; "restored to defaults" would silently delete everything the
// user configured, and a hand-edited file is a likely way to arrive here
// (BR-12).
func TestADamagedConfigIsNeverRebuilt(t *testing.T) {
	for name, content := range map[string]string{
		"truncated json":   `{"version": 1, "device": {`,
		"not json at all":  "# a config file, surely",
		"empty":            "",
		"missing version":  `{"device": {"id": "x"}}`,
		"newer version":    `{"version": 99, "remote": {"type": "s3"}}`,
		"unknown version":  `{"version": 0, "remote": {"type": "s3"}}`,
		"no backend named": `{"version": 1, "device": {"id": "x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, configFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Load(dir); err == nil {
				t.Fatal("a damaged configuration loaded")
			}

			// And the file is exactly as it was.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != content {
				t.Errorf("the file was rewritten: %q", after)
			}
		})
	}
}

// TestANewerConfigSaysUpgradeAndAnOlderDoesNot keeps the two directions apart:
// telling a user to upgrade over a zero sends them after a release that does
// not exist (crypto-spec §9).
func TestANewerConfigSaysUpgradeAndAnOlderDoesNot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFile)

	if err := os.WriteFile(path, []byte(`{"version": 99, "remote": {"type":"s3"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("got %v, want ErrUnsupportedVersion", err)
	}

	if err := os.WriteFile(path, []byte(`{"version": 0, "remote": {"type":"s3"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("an unknown version loaded")
	}
	if errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("an older version was reported as newer: %v", err)
	}
}

func TestLoadReportsAnUninitialisedMachine(t *testing.T) {
	// Distinct from a failure, because the answer is a specific instruction.
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNotInitialised) {
		t.Errorf("got %v, want ErrNotInitialised", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "never-created")); !errors.Is(err, ErrNotInitialised) {
		t.Errorf("got %v, want ErrNotInitialised", err)
	}
}

// TestLoadDoesNotCreateAnything: a read must not decide the machine is now set
// up, or a mistyped CTXHOP_CONFIG_DIR would leave litter and report success.
func TestLoadDoesNotCreateAnything(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "not-there")

	if _, err := Load(dir); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("Load created the configuration directory")
	}
}

func TestSaveRefusesAnUnusableConfig(t *testing.T) {
	dir := t.TempDir()

	if err := (&Config{Version: configVersion}).Save(dir); err == nil {
		t.Error("a configuration naming no backend was saved")
	}
	if err := (&Config{Version: configVersion + 1, Remote: Remote{Type: "s3"}}).Save(dir); err == nil {
		t.Error("a configuration from the future was saved")
	}
	var nilConfig *Config
	if err := nilConfig.Save(dir); err == nil {
		t.Error("a nil configuration was saved")
	}

	if _, err := os.Stat(filepath.Join(dir, configFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused save left a file behind")
	}
}

func TestSaveReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	c := fullConfig()
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}

	c.Device.Name = "laptop"
	c.Projects.Bindings = append(c.Projects.Bindings, Binding{Identity: "github.com/acme/second", LocalRoot: "/srv/second"})
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Device.Name != "laptop" || len(got.Projects.Bindings) != 2 {
		t.Errorf("the second save did not take: %+v", got)
	}

	// No temporary files survived.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestConfigDirFollowsTheEnvironment(t *testing.T) {
	// The hook inherits whatever environment the agent was started with, so
	// there has to be one explicit way to say where the configuration lives.
	want := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(dirEnv, want)

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(want) {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

func TestConfigDirFollowsPlatformConvention(t *testing.T) {
	t.Setenv(dirEnv, "")

	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != appDir {
		t.Errorf("Dir() = %q, want it to end in %q", got, appDir)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Dir() = %q, want an absolute path", got)
	}
}
