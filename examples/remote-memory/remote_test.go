package remoteexample

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := New()
	body := []byte("encrypted example")

	if err := store.Put(ctx, "project/meta", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	info, err := store.Stat(ctx, "project/meta")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Fatalf("Stat().Size = %d, want %d", info.Size, len(body))
	}

	reader, err := store.Get(ctx, "project/meta")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	} else if closeErr != nil {
		t.Fatalf("Close() error = %v", closeErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("Get() = %q, want %q", got, body)
	}

	objects, err := store.List(ctx, "project/")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(objects) != 1 || objects[0].Key != "project/meta" {
		t.Fatalf("List() = %#v, want one project/meta object", objects)
	}

	if err := store.Delete(ctx, "project/meta"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Stat(ctx, "project/meta"); !errors.Is(err, remote.ErrNotFound) {
		t.Fatalf("Stat() after Delete() error = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsBadSizeAndCancellation(t *testing.T) {
	store := New()
	if err := store.Put(context.Background(), "bad", bytes.NewReader([]byte("abc")), 2); err == nil {
		t.Fatal("Put() accepted a body larger than declared")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("List() error = %v, want context.Canceled", err)
	}
}
