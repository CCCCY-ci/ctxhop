package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
)

func TestSyncDomainFingerprintNormalizesRemoteNamespace(t *testing.T) {
	public := []byte("public-identity")
	root := t.TempDir()
	dirA, err := syncDomainFingerprintFor(config.Remote{Type: "dir", Path: filepath.Join(root, ".")}, public)
	if err != nil {
		t.Fatal(err)
	}
	dirB, err := syncDomainFingerprintFor(config.Remote{Type: "DIR", Path: root}, public)
	if err != nil {
		t.Fatal(err)
	}
	if dirA != dirB {
		t.Fatalf("equivalent directory namespaces differ: %q vs %q", dirA, dirB)
	}
	if strings.Contains(dirA, root) {
		t.Fatalf("directory path leaked into fingerprint: %q", dirA)
	}

	s3A, err := syncDomainFingerprintFor(config.Remote{
		Type:     "s3",
		Endpoint: "HTTPS://Storage.Example.invalid/base/",
		Bucket:   "private-bucket",
		Prefix:   "team/",
	}, public)
	if err != nil {
		t.Fatal(err)
	}
	s3B, err := syncDomainFingerprintFor(config.Remote{
		Type:     "S3",
		Endpoint: "https://storage.example.invalid/base",
		Bucket:   "private-bucket",
		Region:   "us-east-1",
		Prefix:   "team",
	}, public)
	if err != nil {
		t.Fatal(err)
	}
	if s3A != s3B {
		t.Fatalf("equivalent S3 namespaces differ: %q vs %q", s3A, s3B)
	}
	otherPrefix, err := syncDomainFingerprintFor(config.Remote{
		Type:     "s3",
		Endpoint: "https://storage.example.invalid/base",
		Bucket:   "private-bucket",
		Prefix:   "other",
	}, public)
	if err != nil {
		t.Fatal(err)
	}
	if s3A == otherPrefix {
		t.Fatal("different S3 prefixes produced the same fingerprint")
	}
}

func TestNormalizeExpectedDomainFingerprint(t *testing.T) {
	value, err := normalizeExpectedDomainFingerprint(strings.ToUpper(strings.Repeat("a", 26)))
	if err != nil {
		t.Fatal(err)
	}
	if value != strings.Repeat("a", 26) {
		t.Fatalf("normalized fingerprint = %q", value)
	}
	if _, err := normalizeExpectedDomainFingerprint("too-short"); err == nil {
		t.Fatal("short expected fingerprint was accepted")
	}
}

func TestStatusDisplaysDomainFingerprint(t *testing.T) {
	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: t.TempDir()}
	c.IdentityPublic = []byte("public-identity")

	report, err := collectStatus(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expected, err := syncDomainFingerprint(c)
	if err != nil {
		t.Fatal(err)
	}
	if report.Configuration.DomainFingerprint != expected {
		t.Fatalf("status fingerprint = %q, want %q", report.Configuration.DomainFingerprint, expected)
	}
	var output strings.Builder
	if err := writeStatusText(&output, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "domain fingerprint: "+expected) {
		t.Fatalf("status output omits fingerprint %q: %s", expected, output.String())
	}
}
