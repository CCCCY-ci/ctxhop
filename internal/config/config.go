package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// configVersion is the format this build writes.
const configVersion = 1

// configFile holds settings. It carries no secret, but it is not a file to
// share either: bindings hold local absolute paths and the remote holds a
// bucket name. The shareable view is Summarise (spec §2.1).
const configFile = "config.json"

// ErrUnsupportedVersion reports a configuration written by a newer release.
var ErrUnsupportedVersion = errors.New("config: this configuration was written by a newer ctxhop; upgrade to read it")

// Device is how this machine identifies itself to the others.
type Device struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	Mode DeviceMode `json:"mode,omitempty"`
}

// Remote describes the storage backend. It holds no credentials: those live in
// the secrets file, which is the only thing on disk worth protecting (spec §2.2).
type Remote struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Path     string `json:"path,omitempty"`
	// PathStyle selects URI path-style S3 addressing. The default is
	// virtual-hosted style; some S3-compatible gateways require this option.
	PathStyle bool `json:"pathStyle,omitempty"`
}

// Binding records that the user declared a directory to be a given project.
//
// Deliberately a copy of the project layer's type rather than an import of it.
// The two packages are siblings, and a dependency between them would exist only
// so that one struct is written once (spec §1.2).
type Binding struct {
	Identity  string `json:"identity"`
	LocalRoot string `json:"localRoot"`
}

// Projects is which projects sync and how.
type Projects struct {
	Bindings []Binding `json:"bindings,omitempty"`
	Excluded []string  `json:"excluded,omitempty"`
	PushOnly []string  `json:"pushOnly,omitempty"`
}

// AgentState is what has been done to one agent's installation.
type AgentState struct {
	HookInstalled bool `json:"hookInstalled"`
}

// HookScope controls how much data an automatic SessionEnd push includes.
//
// Session is the safe default and preserves the historical hook behaviour:
// the session and its filtered environment are uploaded. Workspace adds the
// project files and Git state to that automatic push.
type HookScope string

const (
	HookScopeSession   HookScope = "session"
	HookScopeWorkspace HookScope = "workspace"
)

// Effective returns the backward-compatible value for configurations written
// before hookScope existed.
func (s HookScope) Effective() HookScope {
	if s == HookScopeWorkspace {
		return HookScopeWorkspace
	}
	return HookScopeSession
}

// Validate rejects a value that this build cannot use.
func (s HookScope) Validate() error {
	if s == "" {
		return nil
	}
	if s != HookScopeSession && s != HookScopeWorkspace {
		return fmt.Errorf("config: unsupported hook scope %q; expected session or workspace", s)
	}
	return nil
}

// ConfigSyncMode controls whether an adapter's filtered Agent configuration
// components are included with a Session push. This is not a raw config.toml
// switch: adapters still decide which settings and MCP transport intents are
// safe to capture, and sensitive values are rejected before encryption.
type ConfigSyncMode string

const (
	ConfigSyncEnabled  ConfigSyncMode = "enabled"
	ConfigSyncDisabled ConfigSyncMode = "disabled"
)

// Effective returns the backward-compatible value for configurations written
// before the config sync choice existed. An omitted value keeps the historical
// behaviour and includes filtered Agent configuration.
func (m ConfigSyncMode) Effective() ConfigSyncMode {
	if m == ConfigSyncDisabled {
		return ConfigSyncDisabled
	}
	return ConfigSyncEnabled
}

// Validate rejects a value that this build cannot use.
func (m ConfigSyncMode) Validate() error {
	if m == "" {
		return nil
	}
	if m != ConfigSyncEnabled && m != ConfigSyncDisabled {
		return fmt.Errorf("config: unsupported config sync mode %q; expected enabled or disabled", m)
	}
	return nil
}

// SyncConfigEnabled reports whether filtered Agent configuration should be
// included in the next push.
func (c *Config) SyncConfigEnabled() bool {
	return c != nil && c.SyncConfig.Effective() == ConfigSyncEnabled
}

// Config is everything that survives between runs, minus the secrets.
type Config struct {
	Version int    `json:"version"`
	Device  Device `json:"device"`
	Remote  Remote `json:"remote"`
	// DomainFingerprint binds this installation to the normalized Remote
	// namespace and the pinned encryption identity recorded during init. It is
	// non-secret and lets commands fail closed when a configuration is pointed
	// at a different storage namespace by hand.
	DomainFingerprint string `json:"domainFingerprint,omitempty"`
	// IdentityPublic is the key this machine encrypts to, pinned at init.
	//
	// Pinned rather than read from storage each time: the keyfile lives in the
	// user's bucket, so anyone holding it could swap the advertised key and
	// every later push would be encrypted to them and unreadable by its owner
	// (crypto-spec §3.4).
	IdentityPublic   []byte                `json:"identityPublic"`
	DomainGeneration uint64                `json:"domainGeneration,omitempty"`
	Projects         Projects              `json:"projects"`
	Agents           map[string]AgentState `json:"agents,omitempty"`
	HookScope        HookScope             `json:"hookScope,omitempty"`
	SyncConfig       ConfigSyncMode        `json:"syncConfig,omitempty"`
}

// Load reads the configuration from dir.
//
// A configuration that cannot be parsed is an error and nothing else. Rebuilding
// it would silently discard every binding, exclusion and device name the user
// set - the file is small and hand-editable, so a syntax error is a likely way
// to arrive here, and "restored to defaults" would be the worst possible answer
// (BR-12, spec §4.1).
func Load(dir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		if migrateErr := migrateLegacyIfNeeded(dir); migrateErr != nil {
			return nil, migrateErr
		}
		data, err = os.ReadFile(filepath.Join(dir, configFile))
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotInitialised
		}
		return nil, fmt.Errorf("read the configuration: %w", pathSafe(err))
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("the configuration file is not valid JSON; fix or remove it: %w", err)
	}
	if err := c.check(); err != nil {
		return nil, err
	}
	return &c, nil
}

// check refuses a configuration this build cannot use.
func (c *Config) check() error {
	if c == nil {
		return errors.New("config: no configuration")
	}
	// Only a higher version is "too new"; anything else is damage. The two call
	// for different remedies, and telling a user to upgrade over a zero would
	// send them after a release that does not exist (crypto-spec §9).
	switch {
	case c.Version > configVersion:
		return fmt.Errorf("%w: version %d", ErrUnsupportedVersion, c.Version)
	case c.Version != configVersion:
		return fmt.Errorf("config: unknown configuration version %d; the file is damaged", c.Version)
	}
	if err := c.Device.Mode.Validate(); err != nil {
		return err
	}
	if err := c.HookScope.Validate(); err != nil {
		return err
	}
	if err := c.SyncConfig.Validate(); err != nil {
		return err
	}
	if c.Remote.Type == "" {
		return errors.New("config: the configuration names no storage backend; run 'ctxhop init'")
	}
	return nil
}

// Save writes the configuration to dir, creating it if needed.
//
// The write is atomic, so an interruption leaves either the previous
// configuration or the new one. A half-written config.json would fail to parse,
// and by the rule above that is a hard stop for every later command
// (code_style §3.1).
func (c *Config) Save(dir string) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the configuration directory: %w", pathSafe(err))
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the configuration: %w", err)
	}
	if err := atomicfile.WriteBytes(filepath.Join(dir, configFile), append(data, '\n')); err != nil {
		return fmt.Errorf("write the configuration: %w", err)
	}
	return nil
}

// New returns a configuration with this build's version set.
func New() *Config {
	return &Config{
		Version:    configVersion,
		Agents:     map[string]AgentState{},
		HookScope:  HookScopeSession,
		SyncConfig: ConfigSyncEnabled,
	}
}
