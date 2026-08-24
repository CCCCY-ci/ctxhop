package main

import (
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
)

// syncDomainNamespace is a length-prefixed, non-secret representation of the
// configured storage namespace. Credentials and passphrases are intentionally
// excluded. Length prefixes keep different field boundaries distinct without
// relying on a delimiter that a user may type into a configuration value.
func syncDomainNamespace(remote config.Remote) (string, error) {
	typeName := strings.ToLower(strings.TrimSpace(remote.Type))
	var parts []string
	switch typeName {
	case "dir":
		root := strings.TrimSpace(remote.Path)
		if root == "" {
			return "", errors.New("domain: directory namespace is not configured")
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return "", errors.New("domain: directory namespace cannot be normalized")
		}
		parts = []string{typeName, filepath.ToSlash(filepath.Clean(absolute))}
	case "s3":
		endpoint, err := normalizeDomainEndpoint(remote.Endpoint)
		if err != nil {
			return "", err
		}
		bucket := strings.TrimSpace(remote.Bucket)
		if bucket == "" {
			return "", errors.New("domain: S3 bucket is not configured")
		}
		region := strings.TrimSpace(remote.Region)
		if region == "" {
			region = "us-east-1"
		}
		prefix := strings.TrimRight(strings.TrimSpace(remote.Prefix), "/")
		parts = []string{typeName, endpoint, bucket, region, prefix}
	default:
		return "", errors.New("domain: storage backend is not supported")
	}

	var builder strings.Builder
	for _, part := range parts {
		if strings.ContainsRune(part, 0) {
			return "", errors.New("domain: namespace contains an invalid character")
		}
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
	}
	return builder.String(), nil
}

func normalizeDomainEndpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("domain: S3 endpoint is not configured correctly")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func syncDomainFingerprint(c *config.Config) (string, error) {
	if c == nil {
		return "", errors.New("domain: configuration is unavailable")
	}
	return syncDomainFingerprintFor(c.Remote, c.IdentityPublic)
}

func syncDomainFingerprintFor(remote config.Remote, identityPublic []byte) (string, error) {
	namespace, err := syncDomainNamespace(remote)
	if err != nil {
		return "", err
	}
	return crypto.DomainFingerprint(namespace, identityPublic)
}

func normalizeExpectedDomainFingerprint(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) != 26 {
		return "", errors.New("init: expected domain fingerprint must be 26 characters")
	}
	return value, nil
}

func domainBindingState(c *config.Config, current string) string {
	if c == nil || strings.TrimSpace(c.DomainFingerprint) == "" {
		return "unbound"
	}
	if current == "" {
		return "invalid"
	}
	if strings.EqualFold(strings.TrimSpace(c.DomainFingerprint), current) {
		return "bound"
	}
	return "mismatch"
}
