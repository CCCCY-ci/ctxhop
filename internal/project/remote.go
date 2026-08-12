// Package project answers what project a directory belongs to, where that
// project lives on this machine, and whether its working tree still looks the
// way a session left it.
//
// It is the only package that knows git exists. The adapter knows only the
// agent's data on disk, and the remote layer only moves opaque bytes; neither
// may learn about repositories, branches or commits (spec §1).
//
// Nothing here produces an identifier that leaves the machine. This package
// yields the plaintext canonical remote; crypto.ProjectID is what turns it into
// something irreversible (P6, PRD §8.3).
package project

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrNoRemoteIdentity reports a remote this build will not derive an identity
// from. It is not a failure of the program - a repository without a usable
// remote is an ordinary thing to have.
var ErrNoRemoteIdentity = errors.New("project: remote cannot be canonicalized")

// CanonicalizeRemote reduces a git remote URL to the form every device agrees
// on, so that git@github.com:user/example.git and
// https://github.com/user/example.git identify one project (PRD §9.3).
//
// The result is host/path: the protocol is dropped because switching from SSH
// to HTTPS is not a change of project.
func CanonicalizeRemote(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: empty", ErrNoRemoteIdentity)
	}

	// Local paths are not a cross-device identity: /home/me/repo means nothing
	// on another machine, which is the whole reason this layer exists.
	if isLocalPath(trimmed) {
		return "", fmt.Errorf("%w: local path", ErrNoRemoteIdentity)
	}

	host, path, err := splitRemote(trimmed)
	if err != nil {
		return "", err
	}

	host = canonicalHost(host)
	path = canonicalPath(path)
	if host == "" || path == "" {
		return "", fmt.Errorf("%w: no host or path", ErrNoRemoteIdentity)
	}
	return host + "/" + path, nil
}

// splitRemote reduces the several spellings git accepts to a host and a path.
func splitRemote(raw string) (host, path string, err error) {
	if scp, ok := splitSCP(raw); ok {
		return scp.host, scp.path, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("%w: unparseable", ErrNoRemoteIdentity)
	}
	switch u.Scheme {
	case "ssh", "git+ssh", "https", "http", "git", "ftp", "ftps":
	case "":
		return "", "", fmt.Errorf("%w: no scheme", ErrNoRemoteIdentity)
	default:
		// file:// and anything unrecognised. Refusing beats guessing (BR-12).
		return "", "", fmt.Errorf("%w: unsupported scheme", ErrNoRemoteIdentity)
	}

	// u.Host carries any userinfo separately, so reading Hostname here is
	// already the credential-free form.
	return u.Hostname() + portSuffix(u.Port()), u.Path, nil
}

type scpRemote struct{ host, path string }

// splitSCP recognises git's scp-like shorthand, user@host:path.
//
// It has to be told apart from a URL by hand: "git@github.com:user/repo.git"
// has no scheme, and url.Parse reads "git@github.com" as the scheme of an
// opaque URL.
func splitSCP(raw string) (scpRemote, bool) {
	if strings.Contains(raw, "://") {
		return scpRemote{}, false
	}
	colon := strings.Index(raw, ":")
	if colon < 0 {
		return scpRemote{}, false
	}

	hostPart, path := raw[:colon], raw[colon+1:]
	if hostPart == "" || path == "" {
		return scpRemote{}, false
	}
	// A Windows drive letter is a path, not a host: "C:/src/repo".
	if len(hostPart) == 1 {
		return scpRemote{}, false
	}
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	if hostPart == "" || strings.Contains(hostPart, "/") {
		return scpRemote{}, false
	}
	return scpRemote{host: hostPart, path: path}, true
}

// isLocalPath reports remotes that name a directory on this machine.
func isLocalPath(raw string) bool {
	switch {
	case strings.HasPrefix(raw, "/"), strings.HasPrefix(raw, "./"), strings.HasPrefix(raw, "../"):
		return true
	case strings.HasPrefix(raw, `\\`): // UNC
		return true
	case len(raw) >= 2 && raw[1] == ':' && isDriveLetter(raw[0]):
		return true
	}
	return false
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// canonicalHost lowercases the host. DNS is case-insensitive, so this one
// carries no risk of merging two different repositories (spec §2.2).
func canonicalHost(host string) string {
	return strings.ToLower(strings.Trim(host, "[]"))
}

// portSuffix drops the port when it is the default for the protocol, so that
// ssh://host:22/x and ssh://host/x are one project.
func portSuffix(port string) string {
	switch port {
	case "", "22", "443", "80", "9418":
		return ""
	}
	return ":" + port
}

// canonicalPath trims the decoration around a repository path.
//
// The case of the path is deliberately preserved. Lowercasing would merge two
// genuinely different repositories on a case-sensitive forge - other people's
// sessions appearing inside your project - whereas leaving it alone merely
// fails to match, which manual binding can repair. Getting it wrong in the
// direction that mixes data has no such remedy (spec §2.2, §7.2).
func canonicalPath(path string) string {
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	return strings.Trim(path, "/")
}
