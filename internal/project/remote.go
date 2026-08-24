package project

import (
	"errors"
	"net"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
)

var errInvalidRemote = errors.New("project: remote is not a usable repository address")

// CanonicalizeRemote turns the forms Git accepts for a repository remote into
// one stable, credential-free identity. The result is deliberately not a URL:
// it is an identity value, not an address that the program will contact.
func CanonicalizeRemote(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsRune(value, 0) {
		return "", errInvalidRemote
	}
	if looksLikeLocalPath(value) {
		return "", errInvalidRemote
	}

	if strings.Contains(value, "://") {
		return canonicalURL(value)
	}
	return canonicalSCP(value)
}

func canonicalURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "", errInvalidRemote
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "ssh", "git":
	default:
		return "", errInvalidRemote
	}
	if u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errInvalidRemote
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errInvalidRemote
	}

	host := strings.ToLower(u.Hostname())
	if !validHost(host) {
		return "", errInvalidRemote
	}

	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errInvalidRemote
		}
		if n != defaultPort(scheme) {
			host = formatHostPort(host, n)
		}
	} else if strings.Contains(u.Host, ":") && net.ParseIP(host) == nil {
		// A colon in a non-IPv6 host without a port is not a valid authority.
		return "", errInvalidRemote
	}

	repository, err := canonicalRepositoryPath(u.EscapedPath())
	if err != nil {
		return "", err
	}
	return host + "/" + repository, nil
}

func canonicalSCP(raw string) (string, error) {
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || colon == len(raw)-1 {
		return "", errInvalidRemote
	}

	hostPart := raw[:colon]
	pathPart := raw[colon+1:]
	if strings.ContainsAny(hostPart, `/\\`) {
		return "", errInvalidRemote
	}
	if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	host := strings.ToLower(hostPart)
	if !validHost(host) || strings.Contains(host, ":") {
		return "", errInvalidRemote
	}

	repository, err := canonicalRepositoryPath(pathPart)
	if err != nil {
		return "", err
	}
	return host + "/" + repository, nil
}

func looksLikeLocalPath(value string) bool {
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\\`) {
		return true
	}
	if len(value) >= 2 && isASCIIAlpha(value[0]) && value[1] == ':' {
		return true
	}
	if value == "." || value == ".." || strings.HasPrefix(value, `./`) || strings.HasPrefix(value, `../`) || strings.HasPrefix(value, `.\`) || strings.HasPrefix(value, `..\`) {
		return true
	}
	return false
}

func validHost(host string) bool {
	if host == "" || strings.ContainsAny(host, `/\\`) {
		return false
	}
	for _, r := range host {
		if r == 0 || r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			return false
		}
	}
	return true
}

func formatHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func defaultPort(scheme string) int {
	switch scheme {
	case "http":
		return 80
	case "https":
		return 443
	case "ssh":
		return 22
	default:
		return -1
	}
}

func canonicalRepositoryPath(escaped string) (string, error) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil || decoded == "" {
		return "", errInvalidRemote
	}
	decoded = strings.Trim(decoded, "/")
	if decoded == "" || strings.ContainsAny(decoded, `\\`) {
		return "", errInvalidRemote
	}

	parts := strings.Split(decoded, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errInvalidRemote
		}
	}
	decoded = strings.Join(parts, "/")
	if decoded = strings.TrimSuffix(decoded, ".git"); decoded == "" {
		return "", errInvalidRemote
	}
	if pathpkg.Clean(decoded) != decoded {
		return "", errInvalidRemote
	}
	return decoded, nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
