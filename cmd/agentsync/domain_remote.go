package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

// validateConfiguredDomain rejects a local configuration whose Remote
// namespace no longer matches the fingerprint recorded during init. Legacy
// configurations without a stored fingerprint remain readable and continue to
// use the existing public-identity check.
func validateConfiguredDomain(c *config.Config, command string) error {
	if c == nil {
		return fmt.Errorf("%s: configuration is unavailable", command)
	}
	expected := strings.ToLower(strings.TrimSpace(c.DomainFingerprint))
	if expected == "" {
		return nil
	}
	current, err := syncDomainFingerprint(c)
	if err != nil {
		return fmt.Errorf("%s: derive configured sync domain fingerprint: %w", command, err)
	}
	if expected != current {
		return fmt.Errorf("%s: %w: configured sync domain fingerprint does not match this Remote namespace (expected %s, current %s); restore the original Remote settings or re-initialize", command, errDomainBindingMismatch, expected, current)
	}
	return nil
}

// fetchValidatedRemoteKeyfile reads the keyfile and checks both identity pins:
// the public encryption identity and, for new configurations, the normalized
// Remote namespace captured at init. Keeping this at one boundary prevents a
// command from accidentally skipping one half of the domain check.
func fetchValidatedRemoteKeyfile(ctx context.Context, c *config.Config, store remote.Remote, command string) (*crypto.Keyfile, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%s: context is required", command)
	}
	if c == nil {
		return nil, fmt.Errorf("%s: configuration is unavailable", command)
	}
	if store == nil {
		return nil, fmt.Errorf("%s: remote store is unavailable", command)
	}
	if err := validateConfiguredDomain(c, command); err != nil {
		return nil, err
	}

	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return nil, fmt.Errorf("%s: read remote keyfile: %w", command, err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return nil, fmt.Errorf("%s: validate remote identity: %w", command, err)
	}
	if len(c.IdentityPublic) == 0 || !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return nil, fmt.Errorf("%s: remote encryption identity does not match this configuration", command)
	}
	if strings.TrimSpace(c.DomainFingerprint) != "" {
		remoteFingerprint, err := syncDomainFingerprintFor(c.Remote, public.Bytes())
		if err != nil {
			return nil, fmt.Errorf("%s: derive remote sync domain fingerprint: %w", command, err)
		}
		if !strings.EqualFold(c.DomainFingerprint, remoteFingerprint) {
			return nil, fmt.Errorf("%s: %w: remote sync domain fingerprint does not match this configuration (expected %s, remote %s)", command, errDomainBindingMismatch, strings.ToLower(strings.TrimSpace(c.DomainFingerprint)), remoteFingerprint)
		}
	}
	return keyfile, nil
}

var errDomainBindingMismatch = errors.New("sync domain binding mismatch")
