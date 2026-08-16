package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCursorStoreCoversCancellationReadAndTrailingDataBranches(t *testing.T) {
	layout, err := NewObjectLayout("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(nil); err == nil {
		t.Fatal("nil Load context unexpectedly succeeded")
	}
	if err := store.Save(nil, NewPushCursor()); err == nil {
		t.Fatal("nil Save context unexpectedly succeeded")
	}

	if err := store.Save(context.Background(), NewPushCursor()); err != nil {
		t.Fatal(err)
	}
	readCancelled := &cancelAfterFirstCheckContext{}
	if _, err := store.Load(readCancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-read cancellation error = %v, want context.Canceled", err)
	}
	writeCancelled := &cancelAfterFirstCheckContext{}
	if err := store.Save(writeCancelled, NewPushCursor()); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-write cancellation error = %v, want context.Canceled", err)
	}

	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(valid, []byte(" {")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrInvalidCursorState) {
		t.Fatalf("malformed trailing data error = %v, want ErrInvalidCursorState", err)
	}
	if got := statePathSafe(errors.New("plain error")); got.Error() != "plain error" {
		t.Fatalf("statePathSafe changed a plain error to %v", got)
	}
	if _, err := (CursorStore{root: "root"}).filePath(); err == nil {
		t.Fatal("filePath accepted an invalid layout")
	}
}

func TestCursorStoreLoadReportsUnavailableStateDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCursorStore(root, layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Load unexpectedly succeeded with a file in the state path")
	}
}
