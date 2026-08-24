package syncer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCursorStoreRoundTripsAtomicallyAndUsesOpaqueLocalKeys(t *testing.T) {
	root := t.TempDir()
	layout, err := NewObjectLayout("projectid", "sessionid", "deviceid")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCursorStore(root, layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); !errors.Is(err, ErrNoPushCursor) {
		t.Fatalf("missing Load error = %v, want ErrNoPushCursor", err)
	}

	initial := NewPushCursor()
	if err := store.Save(context.Background(), initial); err != nil {
		t.Fatalf("Save initial: %v", err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"version":1,"nextShard":1,"recordCount":0,"headDigest":"` + hexDigest(EmptyDigest()) + `"}` + "\n")
	if !bytes.Equal(data, want) {
		t.Fatalf("cursor file = %q, want %q", data, want)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load initial: %v", err)
	}
	if loaded != initial {
		t.Fatalf("loaded cursor = %+v, want %+v", loaded, initial)
	}
	if !strings.Contains(path, filepath.Join("projectid", "sessions", "sessionid", "deviceid")) {
		t.Fatalf("cursor path does not contain the opaque layout: %q", path)
	}

	advanced, err := initial.Advance(ShardPart{Number: 1, Shard: mustShard(t, 0, EmptyDigest(), [][]byte{[]byte(`{"n":1}`)})})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), advanced); err != nil {
		t.Fatalf("Save advanced: %v", err)
	}
	loaded, err = store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load advanced: %v", err)
	}
	if loaded != advanced {
		t.Fatalf("loaded advanced cursor = %+v, want %+v", loaded, advanced)
	}
	if leftovers, err := filepath.Glob(path + ".*.tmp"); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary cursor files = %v, glob error = %v", leftovers, err)
	}
}

func TestCursorStoreRejectsDamagedVersionsAndUnknownFields(t *testing.T) {
	root := t.TempDir()
	layout, err := NewObjectLayout("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewCursorStore(root, layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), NewPushCursor()); err != nil {
		t.Fatal(err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"version":1,"nextShard":1,"recordCount":0,"headDigest":"` + hexDigest(EmptyDigest()) + `"}`
	for name, content := range map[string]string{
		"invalid json":     "not json",
		"unknown field":    valid[:len(valid)-1] + `,"extra":true}`,
		"trailing value":   valid + ` {}`,
		"future version":   strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"old version":      strings.Replace(valid, `"version":1`, `"version":0`, 1),
		"bad digest":       strings.Replace(valid, hexDigest(EmptyDigest()), "ABC", 1),
		"zero next shard":  strings.Replace(valid, `"nextShard":1`, `"nextShard":0`, 1),
		"wrong empty hash": strings.Replace(valid, hexDigest(EmptyDigest()), strings.Repeat("0", 64), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load unexpectedly succeeded")
			}
			switch name {
			case "future version":
				if !errors.Is(err, ErrUnsupportedCursorState) {
					t.Fatalf("error = %v, want ErrUnsupportedCursorState", err)
				}
			default:
				if !errors.Is(err, ErrInvalidCursorState) {
					t.Fatalf("error = %v, want ErrInvalidCursorState", err)
				}
			}
		})
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCursorStoreValidatesArgumentsCancellationAndFilesystemErrors(t *testing.T) {
	layout, err := NewObjectLayout("p", "s", "d")
	if err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{
		"empty root": " ",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCursorStore(root, layout); err == nil {
				t.Fatal("NewCursorStore unexpectedly succeeded")
			}
		})
	}
	if _, err := NewCursorStore(t.TempDir(), ObjectLayout{}); err == nil {
		t.Fatal("NewCursorStore accepted an invalid layout")
	}
	var zero CursorStore
	if _, err := zero.Load(context.Background()); err == nil {
		t.Fatal("zero store Load unexpectedly succeeded")
	}
	if err := zero.Save(context.Background(), NewPushCursor()); err == nil {
		t.Fatal("zero store Save unexpectedly succeeded")
	}

	store, err := NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(cancelled); err == nil {
		t.Fatal("cancelled Load unexpectedly succeeded")
	}
	if err := store.Save(cancelled, NewPushCursor()); err == nil {
		t.Fatal("cancelled Save unexpectedly succeeded")
	}
	if err := store.Save(context.Background(), PushCursor{}); !errors.Is(err, ErrInvalidPushCursor) {
		t.Fatalf("invalid cursor Save error = %v, want ErrInvalidPushCursor", err)
	}

	brokenRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(brokenRoot, "state"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := NewCursorStore(brokenRoot, layout)
	if err != nil {
		t.Fatal(err)
	}
	err = broken.Save(context.Background(), NewPushCursor())
	if err == nil || strings.Contains(err.Error(), brokenRoot) {
		t.Fatalf("filesystem error = %v, expected a redacted failure", err)
	}
}
