package remote

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidKey reports a key that must not be used.
var ErrInvalidKey = errors.New("remote: invalid object key")

// maxKeyLen bounds a key so it cannot exceed what a filesystem-backed driver
// can represent. S3 allows 1024 bytes; Windows paths are the tighter limit in
// practice, so keys stay well below both.
const maxKeyLen = 512

// ValidateKey rejects keys that are unsafe or that the drivers cannot
// represent identically.
//
// Every driver validates before use. A key eventually becomes a path on disk
// for the directory backend, and a key containing `..` or an absolute prefix
// would escape the configured root - the same class of mistake as joining an
// unchecked session id onto a path. Refusing here means no driver has to
// remember to.
//
// Keys are always slash-separated, whatever the host platform. A backslash is
// rejected rather than translated: on Windows it would silently become a
// separator, so the same logical key would name different objects on different
// machines and a session pushed from one would be invisible to the other.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	case len(key) > maxKeyLen:
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidKey, maxKeyLen)
	case strings.ContainsRune(key, '\\'):
		return fmt.Errorf("%w: contains a backslash", ErrInvalidKey)
	case strings.HasPrefix(key, "/"):
		return fmt.Errorf("%w: absolute", ErrInvalidKey)
	case strings.HasSuffix(key, "/"):
		return fmt.Errorf("%w: ends with a separator", ErrInvalidKey)
	}

	// A drive letter would be absolute on Windows even without a leading
	// separator.
	if len(key) >= 2 && key[1] == ':' {
		return fmt.Errorf("%w: looks like a drive-qualified path", ErrInvalidKey)
	}

	for _, part := range strings.Split(key, "/") {
		switch part {
		case "":
			return fmt.Errorf("%w: empty path segment", ErrInvalidKey)
		case ".", "..":
			return fmt.Errorf("%w: contains %q", ErrInvalidKey, part)
		}
		// Trailing dots and spaces are silently stripped by Windows, so two
		// distinct keys would collide there and nowhere else.
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.HasPrefix(part, " ") {
			return fmt.Errorf("%w: segment %q has leading or trailing space or dot", ErrInvalidKey, part)
		}
	}

	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: contains a control character", ErrInvalidKey)
		}
	}
	return nil
}

// ValidatePrefix rejects prefixes that cannot be used for listing.
//
// A prefix is looser than a key: it may be empty, meaning everything, and it
// may end at a partial segment. It must still not escape the root.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	switch {
	case len(prefix) > maxKeyLen:
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidKey, maxKeyLen)
	case strings.ContainsRune(prefix, '\\'):
		return fmt.Errorf("%w: contains a backslash", ErrInvalidKey)
	case strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("%w: absolute", ErrInvalidKey)
	}

	if len(prefix) >= 2 && prefix[1] == ':' {
		return fmt.Errorf("%w: looks like a drive-qualified path", ErrInvalidKey)
	}

	for _, part := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("%w: contains %q", ErrInvalidKey, part)
		}
	}

	for _, r := range prefix {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: contains a control character", ErrInvalidKey)
		}
	}
	return nil
}
