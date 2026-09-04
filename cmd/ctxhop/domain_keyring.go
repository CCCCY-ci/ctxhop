package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

var errDomainGenerationRollback = errors.New("remote key generation is older than this device's accepted generation")

// domainAccess is the authenticated read/write view of one sync domain.
type domainAccess struct {
	Store      remote.Remote
	Keyfile    *crypto.Keyfile
	Ring       *crypto.KeyRing
	Public     *ecdh.PublicKey
	Identities []*ecdh.PrivateKey
	Managed    bool
}

// close releases key material held by an access object.
func (a *domainAccess) allowedDevices() map[string]struct{} {
	if a == nil || !a.Managed || a.Ring == nil {
		return nil
	}
	return a.Ring.ActiveDeviceIDs()
}

func (a *domainAccess) close() {
	if a != nil && a.Ring != nil {
		a.Ring.Close()
		a.Ring = nil
		a.Identities = nil
	}
}

// openAuthorizedDomain opens a domain for unattended writes. Managed domains
// must use the local device private key; there is deliberately no passphrase
// fallback here because a revoked device must fail closed in push.
func openAuthorizedDomain(ctx context.Context, c *config.Config, configDir, command string) (*domainAccess, error) {
	store, keyfile, err := openDeviceRemote(ctx, c, configDir, command)
	if err != nil {
		return nil, err
	}
	if !keyfile.IsManaged() {
		public, err := ecdh.X25519().NewPublicKey(c.IdentityPublic)
		if err != nil {
			return nil, fmt.Errorf("%s: validate encryption identity: %w", command, err)
		}
		return &domainAccess{Store: store, Keyfile: keyfile, Public: public}, nil
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return nil, fmt.Errorf("%s: load local device authorization: %w", command, err)
	}
	if len(secrets.DevicePrivateKey) == 0 {
		return nil, fmt.Errorf("%s: managed sync domain requires a local device grant; initialize this device with an invitation", command)
	}
	private, err := crypto.ParseDevicePrivateKey(secrets.DevicePrivateKey)
	if err != nil {
		return nil, fmt.Errorf("%s: parse local device authorization: %w", command, err)
	}
	ring, err := keyfile.UnlockKeyRingForDevice(c.Device.ID, private)
	if err != nil {
		return nil, fmt.Errorf("%s: authorize local device: %w", command, err)
	}
	if err := acceptManagedDomainState(c, configDir, keyfile, ring, command); err != nil {
		ring.Close()
		return nil, err
	}
	current := ring.Current()
	if current == nil {
		ring.Close()
		return nil, fmt.Errorf("%s: managed keyfile has no active generation", command)
	}
	public, err := ecdh.X25519().NewPublicKey(append([]byte(nil), current.IdentityPublic.Bytes()...))
	if err != nil {
		ring.Close()
		return nil, fmt.Errorf("%s: validate active encryption identity: %w", command, err)
	}
	return &domainAccess{
		Store:      store,
		Keyfile:    keyfile,
		Ring:       ring,
		Public:     public,
		Identities: ring.Identities(),
		Managed:    true,
	}, nil
}

// openDomainForRead prefers the local grant. Legacy/v2 bootstrap configs
// without a device private key fall back to an explicitly typed passphrase.
func openDomainForRead(ctx context.Context, c *config.Config, configDir string, input io.Reader, prompt io.Writer, command string) (*domainAccess, error) {
	return openDomainForReadWithSecretReader(ctx, c, configDir, newCommandSecretReader(input), prompt, command)
}

// openDomainForReadWithSecretReader keeps the buffered input alive across the
// complete authenticated read operation. This avoids losing buffered input
// when a caller performs more than one secret read.
func openDomainForReadWithSecretReader(ctx context.Context, c *config.Config, configDir string, secretReader *commandSecretReader, prompt io.Writer, command string) (*domainAccess, error) {
	store, keyfile, err := openDeviceRemote(ctx, c, configDir, command)
	if err != nil {
		return nil, err
	}
	if keyfile.IsManaged() {
		secrets, secretErr := config.LoadSecrets(configDir)
		if secretErr == nil && len(secrets.DevicePrivateKey) != 0 {
			private, parseErr := crypto.ParseDevicePrivateKey(secrets.DevicePrivateKey)
			if parseErr != nil {
				return nil, fmt.Errorf("%s: parse local device authorization: %w", command, parseErr)
			}
			ring, unlockErr := keyfile.UnlockKeyRingForDevice(c.Device.ID, private)
			if unlockErr != nil {
				return nil, fmt.Errorf("%s: authorize local device: %w", command, unlockErr)
			}
			if err := acceptManagedDomainState(c, configDir, keyfile, ring, command); err != nil {
				ring.Close()
				return nil, err
			}
			return newDomainAccess(store, keyfile, ring, true, command)
		}
	}
	if secretReader == nil || secretReader.raw == nil {
		return nil, fmt.Errorf("%s: input is required", command)
	}
	if prompt == nil {
		return nil, fmt.Errorf("%s: prompt output is required", command)
	}
	passphrase, err := secretReader.read(command, prompt, "Encryption password: ")
	if err != nil {
		return nil, err
	}
	ring, err := keyfile.UnlockKeyRingWithPassphrase(passphrase)
	if err != nil {
		return nil, fmt.Errorf("%s: unlock remote keyfile: %w", command, err)
	}
	if keyfile.IsManaged() {
		if err := acceptManagedDomainState(c, configDir, keyfile, ring, command); err != nil {
			ring.Close()
			return nil, err
		}
	}
	return newDomainAccess(store, keyfile, ring, keyfile.IsManaged(), command)
}

func newDomainAccess(store remote.Remote, keyfile *crypto.Keyfile, ring *crypto.KeyRing, managed bool, command string) (*domainAccess, error) {
	if ring == nil {
		return nil, fmt.Errorf("%s: no unlocked key ring", command)
	}
	current := ring.Current()
	if current == nil {
		ring.Close()
		return nil, fmt.Errorf("%s: unlocked keyfile has no active identity", command)
	}
	return &domainAccess{
		Store:      store,
		Keyfile:    keyfile,
		Ring:       ring,
		Public:     current.IdentityPublic,
		Identities: ring.Identities(),
		Managed:    managed,
	}, nil
}

func acceptManagedDomainState(c *config.Config, configDir string, keyfile *crypto.Keyfile, ring *crypto.KeyRing, command string) error {
	if c == nil || keyfile == nil || ring == nil {
		return fmt.Errorf("%s: managed domain state is unavailable", command)
	}
	if c.DomainGeneration != 0 && keyfile.Generation < c.DomainGeneration {
		return fmt.Errorf("%s: %w: local generation %d, remote generation %d", command, errDomainGenerationRollback, c.DomainGeneration, keyfile.Generation)
	}
	current := ring.Current()
	if current == nil || keyfile.Generation != ring.Generation {
		return fmt.Errorf("%s: active key generation is inconsistent", command)
	}
	changed := c.DomainGeneration != keyfile.Generation || !bytes.Equal(c.IdentityPublic, current.IdentityPublic.Bytes())
	c.IdentityPublic = append([]byte(nil), current.IdentityPublic.Bytes()...)
	c.DomainGeneration = keyfile.Generation
	if changed && c.DomainFingerprint != "" {
		fingerprint, err := syncDomainFingerprint(c)
		if err != nil {
			return fmt.Errorf("%s: refresh sync domain fingerprint: %w", command, err)
		}
		c.DomainFingerprint = fingerprint
	}
	if changed {
		if err := c.Save(configDir); err != nil {
			return fmt.Errorf("%s: save accepted key generation: %w", command, err)
		}
	}
	return nil
}

func keyfileMember(keyfile *crypto.Keyfile, deviceID string) (crypto.KeyfileMember, bool) {
	if keyfile == nil {
		return crypto.KeyfileMember{}, false
	}
	for _, member := range keyfile.Members {
		if member.DeviceID == deviceID {
			return member, true
		}
	}
	return crypto.KeyfileMember{}, false
}
