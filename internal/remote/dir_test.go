package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirContract(t *testing.T) {
	runContract(t, func(t *testing.T) Remote {
		t.Helper()
		d, err := NewDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}

func newTestDir(t *testing.T) *Dir {
	t.Helper()
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewDirRequiresARoot(t *testing.T) {
	if _, err := NewDir("   "); err == nil {
		t.Error("expected an error for an empty root")
	}
}

func TestNewDirResolvesRelativeRoots(t *testing.T) {
	// A relative root would follow whatever directory the process happens to
	// be in, so the same configuration would name different stores depending
	// on where the command was run.
	d, err := NewDir("relative-store")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(d.Root) {
		t.Errorf("Root = %q, want an absolute path", d.Root)
	}
}

func TestDirWritesAtomically(t *testing.T) {
	// The directory may be watched by a third-party sync tool, which must
	// never carry half a shard to another machine.
	d := newTestDir(t)
	if err := d.Put(context.Background(), "v1/a", strings.NewReader("body"), 4); err != nil {
		t.Fatal(err)
	}

	var leftovers []string
	err := filepath.Walk(d.Root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".tmp") {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestDirRefusesAShortBody(t *testing.T) {
	// A body shorter than promised would land as a complete-looking object.
	// For a shard that means silently losing its tail.
	d := newTestDir(t)
	err := d.Put(context.Background(), "v1/short", strings.NewReader("abc"), 10)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if _, err := d.Stat(context.Background(), "v1/short"); !errors.Is(err, ErrNotFound) {
		t.Error("a refused write left an object behind")
	}
}

func TestDirTemporaryFilesAreNotListed(t *testing.T) {
	d := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(d.Root, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A write in progress, or one abandoned by a crash.
	if err := os.WriteFile(filepath.Join(d.Root, "v1", "a.1234.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(context.Background(), "v1/a", strings.NewReader("real"), 4); err != nil {
		t.Fatal(err)
	}

	if got := keysOf(t, d, ""); len(got) != 1 || got[0] != "v1/a" {
		t.Errorf("got %v, want just the real object", got)
	}
}

func TestDirDoesNotFollowSymlinksOutOfTheRoot(t *testing.T) {
	d := newTestDir(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.Root, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(d.Root, "v1", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Following the link would list, and later read, files outside the store.
	for _, key := range keysOf(t, d, "") {
		if strings.Contains(key, "escape") {
			t.Errorf("listing followed a symlink out of the root: %q", key)
		}
	}
}

func TestDirStatRejectsADirectory(t *testing.T) {
	// Reporting a directory as an object would let a caller believe a shard
	// exists where none does.
	d := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(d.Root, "v1", "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Stat(context.Background(), "v1/adir"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDirListRejectsUnsafePrefixes(t *testing.T) {
	d := newTestDir(t)
	for _, prefix := range []string{"../escape", "/absolute", `v1\back`, "C:/drive"} {
		if _, err := d.List(context.Background(), prefix); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("List(%q): got %v, want ErrInvalidKey", prefix, err)
		}
	}
}

func TestDirProbe(t *testing.T) {
	d, err := NewDir(filepath.Join(t.TempDir(), "not-yet-created"))
	if err != nil {
		t.Fatal(err)
	}

	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// The probe cleans up after itself, so a fresh store is left empty.
	if got := keysOf(t, d, ""); len(got) != 0 {
		t.Errorf("probe left objects behind: %v", got)
	}
}

func TestDirProbeReportsAnUnusableRoot(t *testing.T) {
	// A file where the root belongs cannot be turned into a directory. The
	// failure has to surface at setup, not at the first sync (§9.1).
	base := t.TempDir()
	blocker := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := NewDir(blocker)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Probe(context.Background())
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	// The message has to tell the user what to do about it.
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("error should say what to check, got %v", err)
	}
}

func TestDirGetReturnsTheBody(t *testing.T) {
	d := newTestDir(t)
	if err := d.Put(context.Background(), "v1/a", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	r, err := d.Get(context.Background(), "v1/a")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := readAllString(t, r); got != "hello" {
		t.Errorf("got %q", got)
	}
}
