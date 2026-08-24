package syncer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

const (
	remoteDeleteWorkers  = 8
	remoteDeleteAttempts = 3
	remoteDeleteBackoff  = 250 * time.Millisecond
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

	return deleteRemoteObjects(ctx, store, ordered)
}

func deleteRemoteObjects(ctx context.Context, store remote.Remote, keys []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("syncer: delete remote objects: %w", err)
	}
	if len(keys) == 0 {
		return 0, nil
	}

	deleteCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := remoteDeleteWorkers
	if len(keys) < workers {
		workers = len(keys)
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var stateMu sync.Mutex
	removed := 0
	var firstErr error

	worker := func() {
		defer wg.Done()
		for {
			select {
			case <-deleteCtx.Done():
				return
			case key, ok := <-jobs:
				if !ok {
					return
				}
				if err := deleteRemoteObject(deleteCtx, store, key); err != nil {
					stateMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("syncer: delete remote object %q: %w", key, err)
						cancel()
					}
					stateMu.Unlock()
					continue
				}
				stateMu.Lock()
				removed++
				stateMu.Unlock()
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker()
	}

	for _, key := range keys {
		select {
		case jobs <- key:
		case <-deleteCtx.Done():
			break
		}
		if deleteCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()

	stateMu.Lock()
	defer stateMu.Unlock()
	if firstErr != nil {
		return removed, firstErr
	}
	if err := ctx.Err(); err != nil {
		return removed, fmt.Errorf("syncer: delete remote objects: %w", err)
	}
	return removed, nil
}

func deleteRemoteObject(ctx context.Context, store remote.Remote, key string) error {
	var lastErr error
	for attempt := 0; attempt < remoteDeleteAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := store.Delete(ctx, key)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt+1 == remoteDeleteAttempts || !retryableDeleteError(ctx, err) {
			return err
		}
		if err := waitForDeleteRetry(ctx, remoteDeleteBackoff*time.Duration(1<<attempt)); err != nil {
			return err
		}
	}
	return lastErr
}

func retryableDeleteError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return errors.Is(err, remote.ErrNetwork) ||
		errors.Is(err, remote.ErrTransient) ||
		errors.Is(err, context.DeadlineExceeded)
}

func waitForDeleteRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
