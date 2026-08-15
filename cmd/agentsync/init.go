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

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

const initBackendTimeout = 15 * time.Second

type initOptions struct {
	backend  string
	path     string
	endpoint string
	bucket   string
	region   string
	prefix   string
	device   string
	noHook   bool
}

type initPrompter struct {
	input  *bufio.Reader
	output io.Writer
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
	if err := refuseExistingConfig(configDir); err != nil {
		return err
	}

	prompter := &initPrompter{input: bufio.NewReader(input), output: output}
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

	passphrase, err := prompter.secret("Passphrase: ")
	if err != nil {
		return err
	}
	confirmation, err := prompter.secret("Repeat passphrase: ")
	if err != nil {
		return err
	}
	if passphrase == "" {
		return errors.New("init: passphrase cannot be empty")
	}
	if passphrase != confirmation {
		return errors.New("init: passphrases do not match; run init again")
	}

	ctx, cancel := context.WithTimeout(context.Background(), initBackendTimeout)
	defer cancel()
	prober, ok := store.(remote.Prober)
	if !ok {
		return errors.New("init: the selected backend cannot verify read and write access")
	}
	if err := prober.Probe(ctx); err != nil {
		return fmt.Errorf("init: backend probe failed: %s", safeBackendProbeError(err))
	}

	public, identifierKey, created, err := prepareKeyMaterial(ctx, store, passphrase, prompter)
	if err != nil {
		return err
	}
	defer zeroInitBytes(identifierKey)

	c := config.New()
	c.Device.Name = options.device
	c.Remote = config.Remote{
		Type:     options.backend,
		Endpoint: options.endpoint,
		Bucket:   options.bucket,
		Region:   options.region,
		Prefix:   options.prefix,
		Path:     options.path,
	}
	c.IdentityPublic = public

	secrets := &config.Secrets{
		Credentials:   credentials,
		IdentifierKey: identifierKey,
	}
	if err := config.SaveSecrets(configDir, secrets); err != nil {
		return fmt.Errorf("init: save local secrets: %w", err)
	}
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: save local configuration: %w", err)
	}
	if _, err := config.EnsureDeviceID(configDir, c, identifierKey); err != nil {
		return fmt.Errorf("init: create device identity: %w", err)
	}

	if !options.noHook {
		if err := maybeInstallInitHook(c, configDir, prompter, executable); err != nil {
			return err
		}
	}

	if created {
		if _, err := fmt.Fprintln(output, "remote keyfile: created"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(output, "remote keyfile: opened"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, "agentsync initialization complete")
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
	flags.StringVar(&options.device, "device-name", "", "display name for this device")
	flags.BoolVar(&options.noHook, "no-hook", false, "do not offer Agent hook installation")
	if err := flags.Parse(args); err != nil {
		return initOptions{}, fmt.Errorf("init: %w", err)
	}
	if flags.NArg() != 0 {
		return initOptions{}, fmt.Errorf("init: unexpected argument %q", flags.Arg(0))
	}
	if options.backend != "" {
		options.backend = strings.ToLower(strings.TrimSpace(options.backend))
		if options.backend != "dir" && options.backend != "s3" {
			return initOptions{}, errors.New("init: backend must be dir or s3")
		}
	}
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
			value, err := p.line("Storage directory (agentsync-store): ", "agentsync-store")
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
			Endpoint:  options.endpoint,
			Region:    options.region,
			Bucket:    options.bucket,
			Prefix:    options.prefix,
			AccessKey: credentials.AccessKeyID,
			SecretKey: credentials.SecretAccessKey,
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
	token, err := p.secret("Session token (optional): ")
	if err != nil {
		return config.Credentials{}, err
	}
	return config.Credentials{AccessKeyID: access, SecretAccessKey: secret, SessionToken: token}, nil
}

func prepareKeyMaterial(ctx context.Context, store remote.Remote, passphrase string, p *initPrompter) ([]byte, []byte, bool, error) {
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	created := false
	var recovery string
	if errors.Is(err, syncer.ErrNoRemoteKeyfile) {
		keyfile, recovery, err = crypto.NewKeyfile(passphrase)
		created = true
	} else if err != nil {
		return nil, nil, false, fmt.Errorf("init: read remote keyfile: %w", err)
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("init: create keyfile: %w", err)
	}

	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return nil, nil, false, fmt.Errorf("init: unlock remote keyfile: %w", err)
	}
	identifierKey, idErr := dataKey.IdentifierKey()
	dataKey.Close()
	if idErr != nil {
		return nil, nil, false, fmt.Errorf("init: derive identifier key: %w", idErr)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		zeroInitBytes(identifierKey)
		return nil, nil, false, fmt.Errorf("init: read remote identity: %w", err)
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
		if err := syncer.PublishKeyfile(ctx, store, keyfile); err != nil {
			zeroInitBytes(identifierKey)
			return nil, nil, false, fmt.Errorf("init: publish remote keyfile: %w", err)
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
	if _, err := layout.Detect(ctx); errors.Is(err, adapter.ErrNotInstalled) {
		return nil
	} else if err != nil {
		return fmt.Errorf("init: inspect Claude Code: %w", err)
	}
	if c.Agents == nil {
		c.Agents = map[string]config.AgentState{}
	}
	if !p.confirm("Register the Claude Code SessionEnd hook? [y/N]: ") {
		return nil
	}
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return errors.New("init: cannot locate the agentsync executable for the hook")
		}
	}
	if err := layout.InstallHook(executable); err != nil {
		return fmt.Errorf("init: configuration is complete but hook installation failed: %w", err)
	}
	c.Agents["claude-code"] = config.AgentState{HookInstalled: true}
	if err := c.Save(configDir); err != nil {
		return fmt.Errorf("init: hook installed but configuration update failed: %w", err)
	}
	return nil
}

func refuseExistingConfig(dir string) error {
	_, err := config.Load(dir)
	switch {
	case err == nil:
		return errors.New("init: this machine is already configured; refusing to replace it")
	case errors.Is(err, config.ErrNotInitialised):
		return nil
	default:
		return fmt.Errorf("init: existing configuration cannot be replaced: %w", err)
	}
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
	if _, err := fmt.Fprint(p.output, prompt); err != nil {
		return "", err
	}
	value, err := p.read()
	if err != nil {
		return "", fmt.Errorf("init: read secret: %w", err)
	}
	return value, nil
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
