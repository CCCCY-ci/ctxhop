package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
)

const (
	secretsFile   = "secrets"
	deviceKeyFile = "device.key"

	// secretsLabel is authenticated, so one sealed file cannot be presented as
	// another.
	secretsLabel = "ctxhop/secrets"

	deviceKeyLen = 32
)

// readRandom is a variable so the entropy failure path can be exercised by
// the package tests. Production code keeps the crypto/rand implementation.
var readRandom = rand.Read

// Environment variables supplying credentials instead of the file.
//
// Provided for CI and for anyone who would rather not have credentials on disk
// at all (PRD §9.1). What they supply is never written down.
const (
	envAccessKeyID     = "CTXHOP_ACCESS_KEY_ID"
	envSecretAccessKey = "CTXHOP_SECRET_ACCESS_KEY"
	envSessionToken    = "CTXHOP_SESSION_TOKEN"
)

// ErrNoSecrets reports that this machine has no stored credentials.
var ErrNoSecrets = errors.New("config: no stored credentials; run 'ctxhop init' or set " + envAccessKeyID)

// ErrPartialEnvironment reports credentials supplied half by environment.
var ErrPartialEnvironment = fmt.Errorf("config: %s and %s must be set together", envAccessKeyID, envSecretAccessKey)

// Credentials authenticate to the storage backend.
type Credentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// Secrets is everything on this machine worth protecting.
//
// IdentifierKey is enough for unattended path derivation and push. Managed
// domains additionally persist DevicePrivateKey so this installation can unwrap
// its encrypted device grants; it must be protected like the backend secrets.
// The content identity private keys remain derived in memory from an unlocked
// key ring and are never written here.
type Secrets struct {
	Credentials      Credentials `json:"credentials"`
	IdentifierKey    []byte      `json:"identifierKey"`
	DevicePrivateKey []byte      `json:"devicePrivateKey,omitempty"`
}

// LoadSecrets reads the credentials this machine should use.
//
// The environment wins over the file, and is taken whole or not at all. Falling
// back to disk for the half that was missing would run under a mixture of two
// credential sets, which fails in a way nobody can diagnose from the error the
// backend returns.
func LoadSecrets(dir string) (*Secrets, error) {
	if err := migrateLegacyIfNeeded(dir); err != nil {
		return nil, err
	}
	stored, storedErr := readSecrets(dir)

	env, ok, err := credentialsFromEnvironment()
	if err != nil {
		return nil, err
	}
	if ok {
		s := &Secrets{Credentials: env}
		if stored != nil {
			// The identifier key is not a credential and has no environment
			// form: it is derived from the user's own key material, so the
			// stored copy is the only one there is.
			s.IdentifierKey = stored.IdentifierKey
			s.DevicePrivateKey = stored.DevicePrivateKey
		}
		return s, nil
	}

	if storedErr != nil {
		return nil, storedErr
	}
	return stored, nil
}

func credentialsFromEnvironment() (Credentials, bool, error) {
	id := os.Getenv(envAccessKeyID)
	secret := os.Getenv(envSecretAccessKey)

	switch {
	case id == "" && secret == "":
		return Credentials{}, false, nil
	case id == "" || secret == "":
		return Credentials{}, false, ErrPartialEnvironment
	}
	return Credentials{
		AccessKeyID:     id,
		SecretAccessKey: secret,
		SessionToken:    os.Getenv(envSessionToken),
	}, true, nil
}

func readSecrets(dir string) (*Secrets, error) {
	sealed, err := os.ReadFile(filepath.Join(dir, secretsFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoSecrets
		}
		return nil, fmt.Errorf("read the stored credentials: %w", pathSafe(err))
	}

	key, err := readDeviceKey(dir)
	if err != nil {
		return nil, err
	}
	defer zero(key)

	data, err := crypto.OpenLocal(key, secretsLabel, sealed)
	if err != nil {
		return nil, fmt.Errorf("the stored credentials cannot be opened with this machine's key; re-enter them with 'ctxhop init': %w", err)
	}
	defer zero(data)

	var s Secrets
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("the stored credentials are damaged; re-enter them with 'ctxhop init': %w", err)
	}
	return &s, nil
}

// SaveSecrets seals the credentials under this machine's key.
//
// What the sealing is worth is narrow and should be described narrowly: this
// file stays unreadable if it is copied on its own, or pasted into an issue,
// which people do while asking for help. It does nothing against someone who
// can read this directory, because device.key is in it. PRD §12 already puts a
// compromised machine outside the threat model (spec §2.3).
func SaveSecrets(dir string, s *Secrets) error {
	if s == nil {
		return errors.New("config: no secrets to save")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the configuration directory: %w", pathSafe(err))
	}

	key, err := deviceKey(dir)
	if err != nil {
		return err
	}
	defer zero(key)

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode the credentials: %w", err)
	}
	defer zero(data)

	sealed, err := crypto.SealLocal(key, secretsLabel, data)
	if err != nil {
		return fmt.Errorf("seal the credentials: %w", err)
	}
	if err := atomicfile.WriteBytes(filepath.Join(dir, secretsFile), sealed); err != nil {
		return fmt.Errorf("write the credentials: %w", err)
	}
	return nil
}

// deviceKey returns this machine's key, creating it on first use.
func deviceKey(dir string) ([]byte, error) {
	key, err := readDeviceKey(dir)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key = make([]byte, deviceKeyLen)
	if _, err := readRandom(key); err != nil {
		return nil, fmt.Errorf("generate this machine's key: %w", err)
	}

	if err := atomicfile.WriteBytes(filepath.Join(dir, deviceKeyFile), key); err != nil {
		return nil, fmt.Errorf("write this machine's key: %w", err)
	}
	return key, nil
}

func readDeviceKey(dir string) ([]byte, error) {
	key, err := os.ReadFile(filepath.Join(dir, deviceKeyFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read this machine's key: %w", pathSafe(err))
	}
	if len(key) != deviceKeyLen {
		// Truncated or replaced. Refusing beats deriving something from it and
		// producing a failure that looks like wrong credentials (BR-12).
		return nil, fmt.Errorf("this machine's key is %d bytes rather than %d; it is damaged", len(key), deviceKeyLen)
	}
	return key, nil
}

// zero overwrites material that need not outlive the call. Best effort, as in
// the crypto layer: the runtime may already have copied it.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
