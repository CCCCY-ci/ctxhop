package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestDeleteRemoteSessionReportsPartialFailureAndStopsInOrder(t *testing.T) {
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
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if err == nil {
		t.Fatal("delete unexpectedly succeeded")
	}
	if errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("delete failure was misclassified as missing: %v", err)
	}
	wantCalls := []string{first, second}
	if !equalStrings(store.deleted, wantCalls) {
		t.Fatalf("delete order = %v, want %v", store.deleted, wantCalls)
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
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if !equalStrings(store.deleted, []string{first}) {
		t.Fatalf("delete calls = %v, want only %s", store.deleted, first)
	}
}

type deleteFailureRemote struct {
	objects          []remote.ObjectInfo
	failKey          string
	deleted          []string
	cancelAfterFirst context.CancelFunc
}

func (r *deleteFailureRemote) Name() string { return "delete-failure-test" }

func (r *deleteFailureRemote) List(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	objects := make([]remote.ObjectInfo, 0, len(r.objects))
	for _, object := range r.objects {
		if strings.HasPrefix(object.Key, prefix) {
			objects = append(objects, object)
		}
	}
	return objects, nil
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
	r.deleted = append(r.deleted, key)
	if key == r.failKey {
		return errors.New("injected delete failure")
	}
	if len(r.deleted) == 1 && r.cancelAfterFirst != nil {
		r.cancelAfterFirst()
	}
	return nil
}

func (r *deleteFailureRemote) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, fmt.Errorf("stat is not used by this test Remote")
}

var _ remote.Remote = (*deleteFailureRemote)(nil)
