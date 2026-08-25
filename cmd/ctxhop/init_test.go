package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const initTestPassphrase = "correct horse battery staple"

func TestInitCreatesDirectoryBackendWithoutLeakingSecrets(t *testing.T) {
	configDir := t.TempDir()
	remoteRoot := t.TempDir()
	claudeHome := filepath.Join(t.TempDir(), "claude-home")
	if err := os.MkdirAll(claudeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CTXHOP_ACCESS_KEY_ID", "")
	t.Setenv("CTXHOP_SECRET_ACCESS_KEY", "")
	t.Setenv("CTXHOP_SESSION_TOKEN", "")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	var output bytes.Buffer
	input := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\nyes\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "test-device",
		"--no-hook",
	}, input, &output, "test-ctxhop"); err != nil {
		t.Fatalf("runInitWithIO: %v", err)
	}
	if !strings.Contains(output.String(), "Recovery Key") || !strings.Contains(output.String(), "initialization complete") || !strings.Contains(output.String(), "filtered Agent configuration sync: enabled") {
		t.Errorf("init output = %q", output.String())
	}
	if !strings.Contains(output.String(), "config directory: "+configDir) {
		t.Errorf("init output does not show the config directory: %q", output.String())
	}
	if !strings.Contains(output.String(), "Encryption password: ") || !strings.Contains(output.String(), "Repeat encryption password: ") {
		t.Errorf("init password prompts = %q", output.String())
	}
	if recovery, configPrompt := strings.Index(output.String(), "Type 'saved'"), strings.Index(output.String(), "Sync filtered Agent configuration"); recovery < 0 || configPrompt < recovery {
		t.Errorf("configuration prompt was not placed after recovery confirmation: %q", output.String())
	}
	if strings.Contains(output.String(), "Passphrase: ") || strings.Contains(output.String(), initTestPassphrase) {
		t.Error("init output contains the passphrase")
	}

	c, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if c.Device.ID == "" || c.Device.Name != "test-device" {
		t.Errorf("device = %+v", c.Device)
	}
	if c.Remote.Path != filepath.Clean(remoteRoot) {
		t.Errorf("remote path = %q, want absolute %q", c.Remote.Path, filepath.Clean(remoteRoot))
	}
	if len(c.IdentityPublic) == 0 {
		t.Error("the public identity was not pinned")
	}
	if !c.SyncConfigEnabled() {
		t.Errorf("sync config = %q, want enabled", c.SyncConfig)
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if len(secrets.IdentifierKey) == 0 {
		t.Error("the identifier key was not persisted")
	}
	for _, path := range []string{filepath.Join(configDir, "config.json"), filepath.Join(configDir, "secrets")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(initTestPassphrase)) {
			t.Errorf("%s contains the passphrase", filepath.Base(path))
		}
	}

	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	keyfile, err := syncer.FetchKeyfile(t.Context(), store)
	if err != nil {
		t.Fatalf("FetchKeyfile: %v", err)
	}
	dataKey, err := keyfile.UnlockWithPassphrase(initTestPassphrase)
	if err != nil {
		t.Fatalf("UnlockWithPassphrase: %v", err)
	}
	dataKey.Close()
}

func TestInitDoesNotPublishBeforeRecoveryConfirmation(t *testing.T) {
	configDir := t.TempDir()
	remoteRoot := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	input := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nno\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "test-device",
		"--no-hook",
	}, input, ioDiscard{}, "test-ctxhop"); err == nil {
		t.Fatal("init succeeded without Recovery Key confirmation")
	}
	if _, err := config.Load(configDir); !errors.Is(err, config.ErrNotInitialised) {
		t.Errorf("config after rejected confirmation = %v", err)
	}
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.FetchKeyfile(t.Context(), store); !errors.Is(err, syncer.ErrNoRemoteKeyfile) {
		t.Errorf("remote keyfile after rejected confirmation = %v", err)
	}
}

func TestInitOpensAnExistingKeyfileWithoutReplacingIt(t *testing.T) {
	remoteRoot := t.TempDir()
	firstConfig := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", firstConfig)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))
	firstInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "first-device",
		"--no-hook",
	}, firstInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("first init: %v", err)
	}

	secondConfig := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", secondConfig)
	var output bytes.Buffer
	secondInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nyes\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "second-device",
		"--no-hook",
	}, secondInput, &output, "test-ctxhop"); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if !strings.Contains(output.String(), "remote keyfile: opened") || strings.Contains(output.String(), "Recovery Key") {
		t.Errorf("second init output = %q", output.String())
	}
}

func TestInitCanLeaveCurrentDomainBeforeInitializingAnother(t *testing.T) {
	configDir := t.TempDir()
	oldRemoteRoot := t.TempDir()
	newRemoteRoot := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	oldInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", oldRemoteRoot,
		"--device-name", "old-domain-device",
		"--no-hook",
	}, oldInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("initial init: %v", err)
	}

	var output bytes.Buffer
	newPassphrase := "new domain password"
	newInput := strings.NewReader("y\n" + newPassphrase + "\n" + newPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", newRemoteRoot,
		"--device-name", "new-domain-device",
		"--no-hook",
	}, newInput, &output, "test-ctxhop"); err != nil {
		t.Fatalf("re-init after leaving current domain: %v", err)
	}
	if !strings.Contains(output.String(), "Current sync domain is configured") || !strings.Contains(output.String(), "initialization complete") {
		t.Fatalf("re-init output = %q", output.String())
	}

	c, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load new configuration: %v", err)
	}
	if c.Remote.Path != filepath.Clean(newRemoteRoot) || c.Device.Name != "new-domain-device" {
		t.Fatalf("new configuration = %+v", c)
	}
	oldStore, err := remote.NewDir(oldRemoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.FetchKeyfile(t.Context(), oldStore); err != nil {
		t.Fatalf("old remote keyfile was not preserved: %v", err)
	}
}

func TestInitKeepsCurrentDomainWhenLeavingIsDeclined(t *testing.T) {
	configDir := t.TempDir()
	oldRemoteRoot := t.TempDir()
	newRemoteRoot := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	oldInput := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", oldRemoteRoot,
		"--device-name", "old-domain-device",
		"--no-hook",
	}, oldInput, ioDiscard{}, "test-ctxhop"); err != nil {
		t.Fatalf("initial init: %v", err)
	}

	err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", newRemoteRoot,
		"--device-name", "new-domain-device",
		"--no-hook",
	}, strings.NewReader("n\n"), ioDiscard{}, "test-ctxhop")
	if err == nil || !strings.Contains(err.Error(), "current sync domain was kept") {
		t.Fatalf("declined re-init error = %v", err)
	}
	c, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load preserved configuration: %v", err)
	}
	if c.Remote.Path != filepath.Clean(oldRemoteRoot) {
		t.Fatalf("preserved configuration remote = %q, want %q", c.Remote.Path, filepath.Clean(oldRemoteRoot))
	}
	newStore, err := remote.NewDir(newRemoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.FetchKeyfile(t.Context(), newStore); !errors.Is(err, syncer.ErrNoRemoteKeyfile) {
		t.Fatalf("declined re-init created new remote keyfile: %v", err)
	}
}

func TestInitSkipsConfigSyncQuestionWithoutSupportedAgent(t *testing.T) {
	configDir := t.TempDir()
	remoteRoot := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	var output bytes.Buffer
	input := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "test-device",
		"--no-hook",
	}, input, &output, "test-ctxhop"); err != nil {
		t.Fatalf("runInitWithIO: %v", err)
	}
	if strings.Contains(output.String(), "Sync filtered Agent configuration") {
		t.Fatalf("configuration sync prompt was shown without an Agent: %q", output.String())
	}
	if !strings.Contains(output.String(), "filtered Agent configuration sync: skipped (no supported Agent detected)") {
		t.Fatalf("init output = %q", output.String())
	}
}

func TestInitAsksConfigSyncAfterHookSetup(t *testing.T) {
	configDir := t.TempDir()
	remoteRoot := t.TempDir()
	claudeHome := filepath.Join(t.TempDir(), "claude-home")
	if err := os.MkdirAll(claudeHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing-codex"))

	var output bytes.Buffer
	input := strings.NewReader(initTestPassphrase + "\n" + initTestPassphrase + "\nsaved\n1\ny\nno\n")
	if err := runInitWithIO([]string{
		"--backend", "dir",
		"--path", remoteRoot,
		"--device-name", "test-device",
	}, input, &output, filepath.Join(t.TempDir(), "ctxhop")); err != nil {
		t.Fatalf("runInitWithIO: %v", err)
	}
	hookDone := strings.Index(output.String(), "Claude Code SessionEnd hook: installed")
	configPrompt := strings.Index(output.String(), "Sync filtered Agent configuration")
	if hookDone < 0 || configPrompt < 0 || configPrompt < hookDone {
		t.Fatalf("configuration prompt was not placed after Hook setup: %q", output.String())
	}
	if strings.Contains(output.String(), "Codex config.toml") || !strings.Contains(output.String(), "for detected Agent(s)") {
		t.Fatalf("configuration prompt is Agent-specific or unclear: %q", output.String())
	}
	c, err := config.Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.SyncConfigEnabled() {
		t.Fatalf("sync config = %q, want disabled", c.SyncConfig)
	}
}

func TestInitCredentialsAllowsEmptySessionToken(t *testing.T) {
	t.Setenv("CTXHOP_ACCESS_KEY_ID", "")
	t.Setenv("CTXHOP_SECRET_ACCESS_KEY", "")
	t.Setenv("CTXHOP_SESSION_TOKEN", "")

	input := strings.NewReader("access-id\nsecret-key\n\n")
	var output bytes.Buffer
	prompter := &initPrompter{
		input:       bufio.NewReader(input),
		secretInput: input,
		output:      &output,
	}
	credentials, err := initCredentials(t.TempDir(), prompter)
	if err != nil {
		t.Fatalf("initCredentials: %v", err)
	}
	if credentials.AccessKeyID != "access-id" || credentials.SecretAccessKey != "secret-key" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if credentials.SessionToken != "" {
		t.Fatalf("session token = %q, want empty", credentials.SessionToken)
	}
}
func TestParseInitOptionsRefusesPassphraseFlags(t *testing.T) {
	if _, err := parseInitOptions([]string{"--passphrase", "secret"}); err == nil {
		t.Error("init accepted a passphrase command-line flag")
	}
	if _, err := parseInitOptions([]string{"--backend", "unknown"}); err == nil {
		t.Error("init accepted an unknown backend")
	}
}

func TestParseInitOptionsAcceptsPathStyle(t *testing.T) {
	options, err := parseInitOptions([]string{"--backend", "s3", "--path-style"})
	if err != nil {
		t.Fatalf("parseInitOptions: %v", err)
	}
	if !options.pathStyle {
		t.Error("--path-style was not recorded")
	}
}

func TestInitCommandIsRegistered(t *testing.T) {
	for _, command := range commands {
		if command.name == "init" {
			if command.run == nil {
				t.Fatal("init command has no handler")
			}
			return
		}
	}
	t.Fatal("init command is missing")
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

var _ = crypto.Keyfile{}

func TestMaybeInstallCodexHookRegistersSessionEndHook(t *testing.T) {
	configDir := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: t.TempDir()}
	var output bytes.Buffer
	p := &initPrompter{
		input:  bufio.NewReader(strings.NewReader("y\n")),
		output: &output,
	}
	if err := maybeInstallCodexHook(c, configDir, p, `C:\ctxhop\ctxhop.exe`); err != nil {
		t.Fatalf("maybeInstallCodexHook: %v", err)
	}
	if !c.Agents["codex"].HookInstalled {
		t.Fatalf("agents = %+v", c.Agents)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "hooks.json")); err != nil {
		t.Fatalf("hooks.json: %v", err)
	}
	if !strings.Contains(output.String(), "restart Codex") || !strings.Contains(output.String(), "/hooks") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestPromptHookScopeAcceptsWorkspaceMode(t *testing.T) {
	c := config.New()
	var output bytes.Buffer
	p := &initPrompter{
		input:  bufio.NewReader(strings.NewReader("2\n")),
		output: &output,
	}
	scope, err := promptHookScope(c, p)
	if err != nil {
		t.Fatal(err)
	}
	if scope != config.HookScopeWorkspace {
		t.Fatalf("scope = %q, want workspace", scope)
	}
	if !strings.Contains(output.String(), "1=session, 2=session+workspace") {
		t.Fatalf("prompt = %q", output.String())
	}
}

func TestPromptConfigSyncAcceptsDisabledMode(t *testing.T) {
	c := config.New()
	var output bytes.Buffer
	p := &initPrompter{
		input:  bufio.NewReader(strings.NewReader("no\n")),
		output: &output,
	}
	mode, err := promptConfigSync(c, p)
	if err != nil {
		t.Fatal(err)
	}
	if mode != config.ConfigSyncDisabled {
		t.Fatalf("mode = %q, want disabled", mode)
	}
	if !strings.Contains(output.String(), "for detected Agent(s)") || strings.Contains(output.String(), "Codex config.toml") {
		t.Fatalf("prompt = %q", output.String())
	}
}
