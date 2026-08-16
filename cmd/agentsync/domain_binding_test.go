package main

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestValidateConfiguredDomainRejectsChangedNamespace(t *testing.T) {
	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: t.TempDir()}
	c.IdentityPublic = []byte("public-identity")
	fingerprint, err := syncDomainFingerprint(c)
	if err != nil {
		t.Fatal(err)
	}
	c.DomainFingerprint = fingerprint

	if err := validateConfiguredDomain(c, "test"); err != nil {
		t.Fatalf("validateConfiguredDomain for original namespace: %v", err)
	}
	c.Remote.Path = t.TempDir()
	err = validateConfiguredDomain(c, "test")
	if !errors.Is(err, errDomainBindingMismatch) {
		t.Fatalf("changed namespace error = %v, want errDomainBindingMismatch", err)
	}
}

func TestFetchValidatedRemoteKeyfileChecksDomainBinding(t *testing.T) {
	root := t.TempDir()
	store, err := remote.NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	keyfile, _, err := crypto.NewKeyfile("passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.PublishKeyfile(context.Background(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}

	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: root}
	c.IdentityPublic = public.Bytes()
	c.DomainFingerprint, err = syncDomainFingerprint(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchValidatedRemoteKeyfile(context.Background(), c, store, "test"); err != nil {
		t.Fatalf("fetchValidatedRemoteKeyfile: %v", err)
	}

	c.Remote.Path = t.TempDir()
	if _, err := fetchValidatedRemoteKeyfile(context.Background(), c, store, "test"); !errors.Is(err, errDomainBindingMismatch) {
		t.Fatalf("changed namespace fetch error = %v, want errDomainBindingMismatch", err)
	}
}

func TestDomainBindingState(t *testing.T) {
	c := config.New()
	if got := domainBindingState(c, "fingerprint"); got != "unbound" {
		t.Fatalf("empty binding state = %q, want unbound", got)
	}
	c.DomainFingerprint = "fingerprint"
	if got := domainBindingState(c, "fingerprint"); got != "bound" {
		t.Fatalf("matching binding state = %q, want bound", got)
	}
	if got := domainBindingState(c, "other"); got != "mismatch" {
		t.Fatalf("mismatching binding state = %q, want mismatch", got)
	}
	if got := domainBindingState(c, ""); got != "invalid" {
		t.Fatalf("invalid binding state = %q, want invalid", got)
	}
}
