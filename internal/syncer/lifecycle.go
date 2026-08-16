package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

// ProjectRemotePrefix returns the slash-terminated namespace for one project.
// The trailing separator is intentional: a project ID must not match another
// identifier that merely starts with the same bytes.
func ProjectRemotePrefix(projectID string) (string, error) {
	if err := validateIdentifier(projectID); err != nil {
		return "", fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	return objectPrefix + "/" + projectID + "/", nil
}

// SessionRemotePrefix returns the slash-terminated namespace for one session.
// It contains every device branch, metadata object, and immutable shard for the
// session.
func SessionRemotePrefix(projectID, sessionID string) (string, error) {
	layout, err := NewSessionLayout(projectID, sessionID)
	if err != nil {
		return "", err
	}
	prefix, err := layout.Prefix()
	if err != nil {
		return "", err
	}
	return prefix + "/", nil
}

// DeleteRemoteSession removes every object belonging to one project session.
// It does not touch the project's other sessions, device records, or keyfile.
func DeleteRemoteSession(ctx context.Context, store remote.Remote, projectID, sessionID string) (int, error) {
	prefix, err := SessionRemotePrefix(projectID, sessionID)
	if err != nil {
		return 0, err
	}
	return deleteRemotePrefix(ctx, store, prefix)
}

// DeleteRemoteProject removes every session and project-scoped object for one
// project. Device records and the global keyfile remain untouched.
func DeleteRemoteProject(ctx context.Context, store remote.Remote, projectID string) (int, error) {
	prefix, err := ProjectRemotePrefix(projectID)
	if err != nil {
		return 0, err
	}
	return deleteRemotePrefix(ctx, store, prefix)
}

// DeleteRemoteDeviceBranch removes one device-owned branch, including its
// mutable metadata and immutable shards. It leaves every other device branch in
// the session untouched.
func DeleteRemoteDeviceBranch(ctx context.Context, store remote.Remote, projectID, sessionID, deviceID string) (int, error) {
	layout, err := NewObjectLayout(projectID, sessionID, deviceID)
	if err != nil {
		return 0, err
	}
	prefix, err := layout.DevicePrefix()
	if err != nil {
		return 0, err
	}
	return deleteRemotePrefix(ctx, store, prefix+"/")
}

// DeleteRemoteAll removes every valid object visible in the configured Remote,
// including the keyfile and device records. This operation is intentionally
// separate from the scoped deletion helpers because it cannot be undone by
// retaining the keyfile.
func DeleteRemoteAll(ctx context.Context, store remote.Remote) (int, error) {
	return deleteRemotePrefix(ctx, store, "")
}

func deleteRemotePrefix(ctx context.Context, store remote.Remote, prefix string) (int, error) {
	if ctx == nil {
		return 0, errors.New("syncer: context is required")
	}
	if store == nil {
		return 0, errors.New("syncer: remote store is required")
	}
	if err := remote.ValidatePrefix(prefix); err != nil {
		return 0, fmt.Errorf("syncer: invalid deletion prefix: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("syncer: list objects for deletion: %w", err)
	}

	objects, err := store.List(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("syncer: list objects for deletion: %w", err)
	}
	keys := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		if !strings.HasPrefix(object.Key, prefix) {
			continue
		}
		// A backend must return valid object keys, but skip malformed entries
		// rather than handing an unsafe key to a driver during cleanup.
		if remote.ValidateKey(object.Key) != nil {
			continue
		}
		keys[object.Key] = struct{}{}
	}

	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	removed := 0
	for _, key := range ordered {
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("syncer: delete remote objects: %w", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			return removed, fmt.Errorf("syncer: delete remote object %q: %w", key, err)
		}
		removed++
	}
	return removed, nil
}
