package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const initBackendTimeout = 15 * time.Second

type initOptions struct {
	backend                   string
	path                      string
	endpoint                  string
	bucket                    string
	region                    string
	prefix                    string
	pathStyle                 bool
	device                    string
	deviceMode                string
	noHook                    bool
	expectedDomainFingerprint string
	invitePath                string
	invite                    *deviceInvite
}

type initPrompter struct {
	input       *bufio.Reader
	secretInput io.Reader
	output      io.Writer
}

func init() {
	for i := range commands {
		if commands[i].name == "init" {
			commands[i].run = runInit
		}
	}
}

func runInit(args []string) error {
	return runInitWithIO(args, os.Stdin, os.Stdout, "")
}

func runInitWithIO(args []string, input io.Reader, output io.Writer, executable string) error {
	options, err := parseInitOptions(args)
	if err != nil {
		return err
	}
	if strings.TrimSpace(options.invitePath) != "" {
		invite, err := loadDeviceInvite(options.invitePath)
		if err != nil {
			return err
		}
		if err := options.applyDeviceInvite(invite); err != nil {
			return err
		}
	}
	if input == nil {
		return errors.New("init: input is required")
	}
	if output == nil {
		return errors.New("init: output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}

	prompter := &initPrompter{
		input:       bufio.NewReader(input),
		secretInput: input,
		output:      output,
	}
	if err := prepareInitConfig(configDir, prompter); err != nil {
		return err
	}
	if err := options.complete(prompter); err != nil {
		return err
	}
	credentials, store, err := prepareInitBackend(options, configDir, prompter)
	if err != nil {
		return err
	}
	if directory, ok := store.(*remote.Dir); ok {
		options.path = directory.Root
	}

	c := config.New()
	c.Device.Name = options.device
	c.Device.Mode = config.DeviceMode(options.deviceMode)
	c.Remote = config.Remote{
		Type:      options.backend,
		Endpoint:  options.endpoint,
		Bucket:    options.bucket,
		Region:    options.region,
		Prefix:    options.prefix,
		Path:      options.path,
		PathStyle: options.pathStyle,
	}
	namespace, err := syncDomainNamespace(c.Remote)
	if err != nil {
		return fmt.Errorf("init: derive sync domain namespace: %w", err)
	}

	passphrase, err := prompter.secret("Encryption password: ")
	if err != nil {
		return err
	}
	confirmation, err := prompter.secret("Repeat encryption password: ")
	if err != nil {
		return err
	}
	if passphrase == "" {
		return errors.New("init: encryption password cannot be empty")
	}
	if passphrase != confirmation {
		return errors.New("init: encryption passwords do not match; run init again")
	}

	probeCtx, cancelProbe := context.WithTimeout(context.Background(), initBackendTimeout)
	prober, ok := store.(remote.Prober)
	if !ok {
		cancelProbe()
		return errors.New("init: the selected backend cannot verify read and write access")
	}
	probeErr := prober.Probe(probeCtx)
	cancelProbe()
	if probeErr != nil {
		return fmt.Errorf("init: backend probe failed: %s", safeBackendProbeError(probeErr))
	}

	keyfileCtx, cancelKeyfile := context.WithTimeout(context.Background(), initBackendTimeout)
	public, identifierKey, created, err := prepareKeyMaterial(keyfileCtx, store, passphrase, prompter, namespace, options.expectedDomainFingerprint, options.invite != nil)
	cancelKeyfile()
	if err != nil {
		return err
	}
	defer zeroInitBytes(identifierKey)

	c.IdentityPublic = public
	devicePrivate, err := crypto.NewDevicePrivateKey()
	if err != nil {
		return fmt.Errorf("init: create local device authorization: %w", err)
	}
	deviceID, err := config.GenerateDeviceID(identifierKey)
	if err != nil {
		return fmt.Errorf("init: create device identity: %w", err)
	}
	c.Device.ID = deviceID

	readKeyfileCtx, cancelReadKeyfile := context.WithTimeout(context.Background(), initBackendTimeout)
	keyfile, err := syncer.FetchKeyfile(readKeyfileCtx, store)
	cancelReadKeyfile()
	if err != nil {
		return fmt.Errorf("init: re-read remote keyfile: %w", err)
	}
	if options.invite != nil {
		if options.invite.Generation != 0 && keyfile.IsManaged() && options.invite.Generation != keyfile.Generation {
			return fmt.Errorf("init: invitation belongs to key generation %d, remote is at generation %d; create a new invitation", options.invite.Generation, keyfile.Generation)
		}
		if err := verifyDeviceInvite(options.invite, c, identifierKey); err != nil {
			return fmt.Errorf("init: verify device invite: %w", err)
		}
		if keyfile.IsManaged() {
			issuer, found := keyfileMember(keyfile, options.invite.Issuer.DeviceID)
			if !found || issuer.RevokedAtGeneration != 0 {
				return errors.New("init: invitation issuer is not an active member of this sync domain")
			}
		}
	}

	keyfileChanged := false
	if keyfile.IsManaged() {
		if err := crypto.RegisterManagedDevice(keyfile, passphrase, deviceID, devicePrivate.PublicKey()); err != nil {
			return fmt.Errorf("init: register local device grant: %w", err)
		}
		keyfileChanged = true
	} else {
		if err := crypto.MigrateKeyfile(keyfile, passphrase, deviceID, devicePrivate.PublicKey()); err != nil {
			return fmt.Errorf("init: migrate remote keyfile to device authorization: %w", err)
		}
		keyfileChanged = true
	}
	if keyfileChanged {
		publishDeviceCtx, cancelPublishDevice := context.WithTimeout(context.Background(), initBackendTimeout)
		replaceErr := syncer.ReplaceKeyfile(publishDeviceCtx, store, keyfile)
		cancelPublishDevice()
		if replaceErr != nil {
			return fmt.Errorf("init: publish device authorization: %w", replaceErr)
		}
	}
	activePublic, err := keyfile.IdentityPublicKey()
	if err != nil {
		return fmt.Errorf("init: read active remote identity: %w", err)
	}
	c.IdentityPublic = activePublic.Bytes()
	c.DomainGeneration = keyfile.Generation
	domainFingerprint, err := syncDomainFingerprint(c)
	if err != nil {
		return fmt.Errorf("init: derive sync domain fingerprint: %w", err)
	}
	c.DomainFingerprint = domainFingerprint

	secrets := &config.Secrets{
		Credentials:      credentials,
		IdentifierKey:    identifierKey,
		DevicePrivateKey: append([]byte(nil), devicePrivate.Bytes()...),
	}
	if err := config.SaveSecrets(configDir, secrets); err != nil {
		return fmt.Errorf("init: save local secrets: %w", err)
	}
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: save local configuration: %w", err)
	}

	if !options.noHook {
		if err := maybeInstallInitHook(c, configDir, prompter, executable); err != nil {
			return err
		}
	}

	// Ask about filtered Agent configuration only after the Hook step. The
	// question is relevant when Claude Code or Codex is present; an installation
	// without either supported Agent should not ask the user to configure data
	// that cannot be discovered yet.
	configSyncStatus := "skipped (no supported Agent detected)"
	supportedAgent, err := supportedAgentInstalled()
	if err != nil {
		return fmt.Errorf("init: inspect supported Agents: %w", err)
	}
	if supportedAgent {
		syncConfig, err := promptConfigSync(c, prompter)
		if err != nil {
			return err
		}
		c.SyncConfig = syncConfig
		if err := c.Save(configDir); err != nil {
			return fmt.Errorf("init: save configuration sync preference: %w", err)
		}
		configSyncStatus = configSyncLabel(c.SyncConfig)
	}

	if created {
		if _, err := fmt.Fprintln(output, "remote keyfile: created"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(output, "remote keyfile: opened"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "sync domain fingerprint:", domainFingerprint); err != nil {
		return err
	}
	if options.invite != nil {
		if _, err := fmt.Fprintln(output, "sync domain joined via invitation from:", safeListText(options.invite.Issuer.Name)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(output, "config directory:", configDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "filtered Agent configuration sync:", configSyncStatus); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "ctxhop initialization complete")
	return err
}

func parseInitOptions(args []string) (initOptions, error) {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options initOptions
	flags.StringVar(&options.backend, "backend", "", "storage backend: dir or s3")
	flags.StringVar(&options.path, "path", "", "directory backend path")
	flags.StringVar(&options.endpoint, "endpoint", "", "S3 endpoint")
	flags.StringVar(&options.bucket, "bucket", "", "S3 bucket")
	flags.StringVar(&options.region, "region", "", "S3 signing region")
	flags.StringVar(&options.prefix, "prefix", "", "S3 object prefix")
	flags.BoolVar(&options.pathStyle, "path-style", false, "use URI path-style S3 addressing")
	flags.StringVar(&options.device, "device-name", "", "display name for this device")
	flags.StringVar(&options.deviceMode, "device-mode", "", "device mode: normal, push-only, or disabled")
	flags.BoolVar(&options.noHook, "no-hook", false, "do not offer Agent hook installation")
	flags.StringVar(&options.expectedDomainFingerprint, "expect-domain-fingerprint", "", "require this sync domain fingerprint")
	flags.StringVar(&options.invitePath, "invite", "", "join using a CtxHop device invitation file")
	if err := flags.Parse(args); err != nil {
		return initOptions{}, fmt.Errorf("init: %w", err)
	}
	if flags.NArg() != 0 {
		return initOptions{}, fmt.Errorf("init: unexpected argument %q", flags.Arg(0))
	}
	expectedFingerprint, err := normalizeExpectedDomainFingerprint(options.expectedDomainFingerprint)
	if err != nil {
		return initOptions{}, err
	}
	options.expectedDomainFingerprint = expectedFingerprint

	if options.backend != "" {
		options.backend = strings.ToLower(strings.TrimSpace(options.backend))
		if options.backend != "dir" && options.backend != "s3" {
			return initOptions{}, errors.New("init: backend must be dir or s3")
		}
	}
	mode, err := config.ParseDeviceMode(options.deviceMode)
	if err != nil {
		return initOptions{}, fmt.Errorf("init: %w", err)
	}
	options.deviceMode = string(mode)
	return options, nil
}

func (o *initOptions) complete(p *initPrompter) error {
	if o.backend == "" {
		value, err := p.line("Backend [dir/s3] (dir): ", "dir")
		if err != nil {
			return err
		}
		o.backend = strings.ToLower(value)
		if o.backend != "dir" && o.backend != "s3" {
			return errors.New("init: backend must be dir or s3")
		}
	}

	switch o.backend {
	case "dir":
		if o.path == "" {
			value, err := p.line("Storage directory (ctxhop-store): ", "ctxhop-store")
			if err != nil {
				return err
			}
			o.path = value
		}
	case "s3":
		var err error
		if o.endpoint, err = p.lineIfEmpty(o.endpoint, "S3 endpoint: ", ""); err != nil {
			return err
		}
		if o.bucket, err = p.lineIfEmpty(o.bucket, "S3 bucket: ", ""); err != nil {
			return err
		}
		if o.region == "" {
			o.region, err = p.line("S3 region (us-east-1): ", "us-east-1")
			if err != nil {
				return err
			}
		}
		if o.prefix == "" {
			o.prefix, err = p.line("S3 prefix (optional): ", "")
			if err != nil {
				return err
			}
		}
	}

	if o.device == "" {
		defaultName := "this-device"
		if name, err := os.Hostname(); err == nil && strings.TrimSpace(name) != "" {
			defaultName = name
		}
		value, err := p.line("Device name ("+defaultName+"): ", defaultName)
		if err != nil {
			return err
		}
		o.device = value
	}
	if strings.TrimSpace(o.device) == "" {
		return errors.New("init: device name cannot be empty")
	}
	return nil
}

func prepareInitBackend(options initOptions, configDir string, p *initPrompter) (config.Credentials, remote.Remote, error) {
	switch options.backend {
	case "dir":
		store, err := remote.NewDir(options.path)
		if err != nil {
			return config.Credentials{}, nil, fmt.Errorf("init: invalid directory backend: %s", safeBackendSetupError(err))
		}
		return config.Credentials{}, store, nil
	case "s3":
		credentials, err := initCredentials(configDir, p)
		if err != nil {
			return config.Credentials{}, nil, err
		}
		store, err := remote.NewS3(remote.S3Config{
			Endpoint:     options.endpoint,
			Region:       options.region,
			Bucket:       options.bucket,
			Prefix:       options.prefix,
			AccessKey:    credentials.AccessKeyID,
			SecretKey:    credentials.SecretAccessKey,
			SessionToken: credentials.SessionToken,
			PathStyle:    options.pathStyle,
		})
		if err != nil {
			return config.Credentials{}, nil, fmt.Errorf("init: invalid S3 backend: %s", safeBackendSetupError(err))
		}
		return credentials, store, nil
	default:
		return config.Credentials{}, nil, errors.New("init: backend must be dir or s3")
	}
}

func initCredentials(configDir string, p *initPrompter) (config.Credentials, error) {
	secrets, err := config.LoadSecrets(configDir)
	if err == nil {
		return secrets.Credentials, nil
	}
	if !errors.Is(err, config.ErrNoSecrets) {
		if errors.Is(err, config.ErrPartialEnvironment) {
			return config.Credentials{}, errors.New("init: both backend credential environment variables are required")
		}
		return config.Credentials{}, fmt.Errorf("init: read backend credentials: %w", err)
	}

	access, err := p.secret("Access key ID: ")
	if err != nil {
		return config.Credentials{}, err
	}
	secret, err := p.secret("Secret access key: ")
	if err != nil {
		return config.Credentials{}, err
	}
	if access == "" || secret == "" {
		return config.Credentials{}, errors.New("init: both access key ID and secret access key are required")
	}
	token, err := p.optionalSecret("Session token (optional): ")
	if err != nil {
		return config.Credentials{}, err
	}
	return config.Credentials{AccessKeyID: access, SecretAccessKey: secret, SessionToken: token}, nil
}

func prepareKeyMaterial(ctx context.Context, store remote.Remote, passphrase string, p *initPrompter, namespace, expectedFingerprint string, requireExisting bool) ([]byte, []byte, bool, error) {
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	created := false
	var recovery string
	if errors.Is(err, syncer.ErrNoRemoteKeyfile) {
		if requireExisting {
			return nil, nil, false, errors.New("init: device invite requires an existing remote keyfile")
		}
		keyfile, recovery, err = crypto.NewKeyfile(passphrase)
		created = true
	} else if err != nil {
		return nil, nil, false, fmt.Errorf("init: read remote keyfile: %w", err)
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("init: create keyfile: %w", err)
	}

	var identifierKey []byte
	if keyfile.IsManaged() {
		ring, err := keyfile.UnlockKeyRingWithPassphrase(passphrase)
		if err != nil {
			return nil, nil, false, fmt.Errorf("init: unlock managed remote keyfile: %w", err)
		}
		identifierKey = append([]byte(nil), ring.IdentifierKey...)
		ring.Close()
	} else {
		dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
		if err != nil {
			return nil, nil, false, fmt.Errorf("init: unlock remote keyfile: %w", err)
		}
		identifierKey, err = dataKey.IdentifierKey()
		dataKey.Close()
		if err != nil {
			return nil, nil, false, fmt.Errorf("init: derive identifier key: %w", err)
		}
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		zeroInitBytes(identifierKey)
		return nil, nil, false, fmt.Errorf("init: read remote identity: %w", err)
	}

	fingerprint, err := crypto.DomainFingerprint(namespace, public.Bytes())
	if err != nil {
		zeroInitBytes(identifierKey)
		return nil, nil, false, fmt.Errorf("init: derive sync domain fingerprint: %w", err)
	}
	if expectedFingerprint != "" && !strings.EqualFold(expectedFingerprint, fingerprint) {
		zeroInitBytes(identifierKey)
		return nil, nil, false, errors.New("init: expected domain fingerprint does not match the configured remote")
	}

	if created {
		if _, err := fmt.Fprintln(p.output, "Recovery Key (save it before continuing):"); err != nil {
			zeroInitBytes(identifierKey)
			return nil, nil, false, err
		}
		if _, err := fmt.Fprintln(p.output, recovery); err != nil {
			zeroInitBytes(identifierKey)
			return nil, nil, false, err
		}
		saved, err := p.confirmSaved()
		if err != nil {
			zeroInitBytes(identifierKey)
			return nil, nil, false, err
		}
		if !saved {
			zeroInitBytes(identifierKey)
			return nil, nil, false, errors.New("init: Recovery Key was not confirmed as saved; no keyfile was published")
		}
		publishCtx, cancelPublish := context.WithTimeout(context.Background(), initBackendTimeout)
		publishErr := syncer.PublishKeyfile(publishCtx, store, keyfile)
		cancelPublish()
		if publishErr != nil {
			zeroInitBytes(identifierKey)
			return nil, nil, false, fmt.Errorf("init: publish remote keyfile: %w", publishErr)
		}
	}
	return public.Bytes(), identifierKey, created, nil
}

func maybeInstallInitHook(c *config.Config, configDir string, p *initPrompter, executable string) error {
	if c == nil || p == nil {
		return errors.New("init: hook setup received no configuration")
	}
	if _, err := os.Stat(configDir); err != nil {
		return fmt.Errorf("init: configuration directory is unavailable after saving: %w", err)
	}

	home, err := adapter.DefaultHome()
	if err != nil {
		return fmt.Errorf("init: locate Claude Code: %w", err)
	}
	layout := adapter.Layout{Home: home}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, installationErr := layout.Detect(ctx)

	if installationErr != nil && !errors.Is(installationErr, adapter.ErrNotInstalled) {
		return fmt.Errorf("init: inspect Claude Code: %w", installationErr)
	}
	claudeInstalled := installationErr == nil
	codexInstalled, err := codexInstallationAvailable()
	if err != nil {
		return fmt.Errorf("init: inspect Codex: %w", err)
	}
	if !claudeInstalled && !codexInstalled {
		return nil
	}

	hookScope, err := promptHookScope(c, p)
	if err != nil {
		return err
	}
	c.HookScope = hookScope
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: save automatic hook scope: %w", err)
	}

	if claudeInstalled {
		if err := installClaudeHook(c, configDir, p, executable, layout, hookScope); err != nil {
			return err
		}
	}
	if !codexInstalled {
		return nil
	}
	return installCodexHook(c, configDir, p, executable, hookScope)
}

func installClaudeHook(c *config.Config, configDir string, p *initPrompter, executable string, layout adapter.Layout, hookScope config.HookScope) error {
	if c.Agents == nil {
		c.Agents = map[string]config.AgentState{}
	}
	if !p.confirm("Install the Claude Code SessionEnd hook for automatic ctxhop push? [y/N]: ") {
		_, err := fmt.Fprintln(p.output, "Claude Code SessionEnd hook: skipped")
		return err
	}
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return errors.New("init: cannot locate the ctxhop executable for the hook")
		}
	}
	if err := layout.InstallHook(executable, hookScope == config.HookScopeWorkspace); err != nil {
		return fmt.Errorf("init: configuration is complete but Claude Code hook installation failed: %w", err)
	}
	c.Agents["claude-code"] = config.AgentState{HookInstalled: true}
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: Claude Code hook installed but configuration update failed: %w", err)
	}
	_, err := fmt.Fprintf(p.output, "Claude Code SessionEnd hook: installed (mode=%s)\n", hookScopeLabel(hookScope))
	return err
}

func maybeInstallCodexHook(c *config.Config, configDir string, p *initPrompter, executable string) error {
	available, err := codexInstallationAvailable()
	if err != nil {
		return fmt.Errorf("init: inspect Codex: %w", err)
	}
	if !available {
		return nil
	}
	return installCodexHook(c, configDir, p, executable, c.HookScope.Effective())
}

func supportedAgentInstalled() (bool, error) {
	layouts, err := adapter.DefaultLayouts()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, layout := range layouts {
		_, err := layout.Detect(ctx)
		switch {
		case errors.Is(err, adapter.ErrNotInstalled):
			continue
		case err != nil:
			return false, fmt.Errorf("inspect %s: %w", layout.Name(), err)
		default:
			return true, nil
		}
	}
	return false, nil
}

func installCodexHook(c *config.Config, configDir string, p *initPrompter, executable string, hookScope config.HookScope) error {
	home, err := adapter.DefaultCodexHome()
	if err != nil {
		return nil
	}
	layout := adapter.CodexLayout{Home: home}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := layout.Detect(ctx); errors.Is(err, adapter.ErrNotInstalled) {
		return nil
	} else if err != nil {
		return fmt.Errorf("init: inspect Codex: %w", err)
	}

	if c.Agents == nil {
		c.Agents = map[string]config.AgentState{}
	}
	if !p.confirm("Install the Codex SessionEnd hook for automatic ctxhop push? [y/N]: ") {
		_, err := fmt.Fprintln(p.output, "Codex SessionEnd hook: skipped")
		return err
	}
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return errors.New("init: cannot locate the ctxhop executable for the hook")
		}
	}
	if err := layout.InstallHook(executable, hookScope == config.HookScopeWorkspace); err != nil {
		return fmt.Errorf("init: configuration is complete but Codex hook installation failed: %w", err)
	}
	c.Agents["codex"] = config.AgentState{HookInstalled: true}
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: Codex hook installed but configuration update failed: %w", err)
	}
	_, err = fmt.Fprintf(p.output, "Codex SessionEnd hook: installed (mode=%s); restart Codex and trust it in /hooks\n", hookScopeLabel(hookScope))
	return err
}

func codexInstallationAvailable() (bool, error) {
	home, err := adapter.DefaultCodexHome()
	if err != nil {
		return false, err
	}
	layout := adapter.CodexLayout{Home: home}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = layout.Detect(ctx)
	switch {
	case errors.Is(err, adapter.ErrNotInstalled):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

func promptHookScope(c *config.Config, p *initPrompter) (config.HookScope, error) {
	defaultScope := config.HookScopeSession
	if c != nil {
		defaultScope = c.HookScope.Effective()
	}
	fallback := "1"
	if defaultScope == config.HookScopeWorkspace {
		fallback = "2"
	}
	value, err := p.line("Automatic hook sync scope [1=session, 2=session+workspace] ("+fallback+"): ", fallback)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "session", "sessions":
		return config.HookScopeSession, nil
	case "2", "workspace", "workspaces", "session+workspace", "session-and-workspace":
		return config.HookScopeWorkspace, nil
	default:
		return "", errors.New("init: hook mode must be 1 (session) or 2 (session+workspace)")
	}
}

func promptConfigSync(c *config.Config, p *initPrompter) (config.ConfigSyncMode, error) {
	defaultMode := config.ConfigSyncEnabled
	if c != nil {
		defaultMode = c.SyncConfig.Effective()
	}
	fallback := "yes"
	if defaultMode == config.ConfigSyncDisabled {
		fallback = "no"
	}
	value, err := p.line("Sync filtered Agent configuration for detected Agent(s)? [Y/n]: ", fallback)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "y", "yes", "on", "enabled":
		return config.ConfigSyncEnabled, nil
	case "n", "no", "off", "disabled":
		return config.ConfigSyncDisabled, nil
	default:
		return "", errors.New("init: configuration sync must be yes or no")
	}
}

func configSyncLabel(mode config.ConfigSyncMode) string {
	if mode.Effective() == config.ConfigSyncDisabled {
		return "disabled"
	}
	return "enabled"
}

func hookScopeLabel(scope config.HookScope) string {
	if scope.Effective() == config.HookScopeWorkspace {
		return "session+workspace"
	}
	return "session"
}
func prepareInitConfig(dir string, p *initPrompter) error {
	current, err := config.Load(dir)
	switch {
	case err == nil:
		if p == nil {
			return errors.New("init: cannot ask whether to leave the current sync domain")
		}
		if !p.confirm("Current sync domain is configured. Leave it and initialize a new one? This removes local configuration, device keys and CtxHop hooks; remote data is kept. [y/N]: ") {
			return errors.New("init: current sync domain was kept; initialization cancelled")
		}
		if err := leaveCurrentDomain(dir, current); err != nil {
			return fmt.Errorf("init: leave current sync domain: %w", err)
		}
		return nil
	case errors.Is(err, config.ErrNotInitialised):
		return nil
	default:
		return fmt.Errorf("init: existing configuration cannot be replaced: %w", err)
	}
}

func leaveCurrentDomain(configDir string, c *config.Config) error {
	if c == nil {
		return errors.New("configuration is unavailable")
	}
	if configuredRemotePathOverlaps(configDir, c) {
		return fmt.Errorf("the configured local sync directory overlaps %s; move the sync directory before leaving the current domain", configDir)
	}
	if err := removeInstalledAgentHooks(); err != nil {
		return err
	}
	if err := removeInstallDirectory(configDir); err != nil {
		return fmt.Errorf("remove local configuration directory: %w", err)
	}
	return nil
}

func (p *initPrompter) line(prompt, fallback string) (string, error) {
	if _, err := fmt.Fprint(p.output, prompt); err != nil {
		return "", err
	}
	value, err := p.read()
	if err != nil {
		return "", fmt.Errorf("init: read input: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

func (p *initPrompter) lineIfEmpty(value, prompt, fallback string) (string, error) {
	if value != "" {
		return value, nil
	}
	return p.line(prompt, fallback)
}

func (p *initPrompter) secret(prompt string) (string, error) {
	return p.readSecret(prompt, false)
}

func (p *initPrompter) optionalSecret(prompt string) (string, error) {
	return p.readSecret(prompt, true)
}

func (p *initPrompter) readSecret(prompt string, allowEmpty bool) (string, error) {
	if p.secretInput != nil {
		if _, ok := terminalInput(p.secretInput); ok {
			if allowEmpty {
				return readCommandOptionalSecret(p.secretInput, p.output, "init", prompt)
			}
			return readCommandSecret(p.secretInput, p.output, "init", prompt)
		}
	}
	if allowEmpty {
		return readCommandOptionalSecretReader(p.input, p.output, "init", prompt)
	}
	return readCommandSecretReader(p.input, p.output, "init", prompt)
}

func (p *initPrompter) confirm(prompt string) bool {
	value, err := p.line(prompt, "")
	return err == nil && (strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"))
}

func (p *initPrompter) confirmSaved() (bool, error) {
	value, err := p.line("Type 'saved' after storing the Recovery Key: ", "")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(value, "saved"), nil
}

func (p *initPrompter) read() (string, error) {
	value, err := p.input.ReadString('\n')
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.EOF
	}
	return value, nil
}

func zeroInitBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
