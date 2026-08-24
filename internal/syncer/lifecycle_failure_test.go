package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestDeleteRemoteSessionReportsPartialFailure(t *testing.T) {
	first := "v1/projects/p/sessions/s/devicea/000001"
	second := "v1/projects/p/sessions/s/deviceb/000001"
	third := "v1/projects/p/sessions/s/devicec/000001"
	store := &deleteFailureRemote{
		objects: []remote.ObjectInfo{
			{Key: third},
			{Key: second},
			{Key: first},
			{Key: first},
		},
		failKey: second,
	}

	removed, err := DeleteRemoteSession(context.Background(), store, "p", "s")
	if removed < 1 || removed > 2 {
		t.Fatalf("removed = %d, want one or two successful deletes", removed)
	}
	if err == nil {
		t.Fatal("delete unexpectedly succeeded")
	}
	if errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("delete failure was misclassified as missing: %v", err)
	}
	deleted := store.deletedKeys()
	if len(deleted) == 0 || !containsString(deleted, second) {
		t.Fatalf("delete calls = %v, want the failing key %s", deleted, second)
	}
}

func TestDeleteRemoteSessionStopsAfterContextCancellation(t *testing.T) {
	first := "v1/projects/p/sessions/s/devicea/000001"
	second := "v1/projects/p/sessions/s/deviceb/000001"
	ctx, cancel := context.WithCancel(context.Background())
	store := &deleteFailureRemote{
		objects:          []remote.ObjectInfo{{Key: second}, {Key: first}},
		cancelAfterFirst: cancel,
	}

	removed, err := DeleteRemoteSession(ctx, store, "p", "s")
	if removed < 1 || removed > 2 {
		t.Fatalf("removed = %d, want one or two successful deletes", removed)
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	deleted := store.deletedKeys()
	if len(deleted) == 0 || !containsString(deleted, first) {
		t.Fatalf("delete calls = %v, want %s", deleted, first)
	}
}

func TestDeleteRemoteSessionRetriesTransientFailure(t *testing.T) {
	key := "v1/projects/p/sessions/s/devicea/000001"
	store := &deleteFailureRemote{
		objects:           []remote.ObjectInfo{{Key: key}},
		transientFailures: map[string]int{key: 2},
	}

	removed, err := DeleteRemoteSession(context.Background(), store, "p", "s")
	if err != nil {
		t.Fatalf("delete returned an error after transient retries: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if got := store.deleteAttempts(key); got != 3 {
		t.Fatalf("delete attempts = %d, want 3", got)
	}
}

type deleteFailureRemote struct {
	mu                sync.Mutex
	objects           []remote.ObjectInfo
	failKey           string
	deleted           []string
	deleteCounts      map[string]int
	transientFailures map[string]int
	cancelAfterFirst  context.CancelFunc
}

func (r *deleteFailureRemote) Name() string { return "delete-failure-test" }

func (r *deleteFailureRemote) List(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	objects := append([]remote.ObjectInfo(nil), r.objects...)
	r.mu.Unlock()
	filtered := make([]remote.ObjectInfo, 0, len(objects))
	for _, object := range objects {
		if strings.HasPrefix(object.Key, prefix) {
			filtered = append(filtered, object)
		}
	}
	return filtered, nil
}

func (r *deleteFailureRemote) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("get is not used by this test Remote")
}

func (r *deleteFailureRemote) Put(context.Context, string, io.Reader, int64) error {
	return fmt.Errorf("put is not used by this test Remote")
}

func (r *deleteFailureRemote) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.deleted = append(r.deleted, key)
	if r.deleteCounts == nil {
		r.deleteCounts = make(map[string]int)
	}
	r.deleteCounts[key]++
	attempt := r.deleteCounts[key]
	fail := key == r.failKey
	transient := attempt <= r.transientFailures[key]
	shouldCancel := len(r.deleted) == 1 && r.cancelAfterFirst != nil
	cancel := r.cancelAfterFirst
	r.mu.Unlock()
	if fail {
		return errors.New("injected delete failure")
	}
	if transient {
		return fmt.Errorf("injected transient delete failure: %w", remote.ErrTransient)
	}
	if shouldCancel {
		cancel()
	}
	return nil
}

func (r *deleteFailureRemote) deletedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

func (r *deleteFailureRemote) deleteAttempts(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deleteCounts[key]
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (r *deleteFailureRemote) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, fmt.Errorf("stat is not used by this test Remote")
}

var _ remote.Remote = (*deleteFailureRemote)(nil)
