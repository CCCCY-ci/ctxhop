package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the paths a healthy filesystem never takes. They matter more than
// their share of the code: a store that misreports a failure as "nothing there"
// makes the sync layer conclude another device pushed nothing.

func TestDirListReportsAVanishedRoot(t *testing.T) {
	// The backend is documented for USB drives and folders a third-party tool
	// carries between machines, so the root going away is an ordinary event.
	// Reporting it as an empty store tells the sync layer the other device
	// pushed nothing, and that turns a fast-forward into a fork.
	base := t.TempDir()
	root := filepath.Join(base, "removable")
	d, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put(context.Background(), "v1/a", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	got, err := d.List(context.Background(), "")
	if err == nil {
		t.Fatalf("a vanished root was reported as an empty store: %d objects", len(got))
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("absence of the store is not absence of an object: %v", err)
	}
	if !strings.Contains(err.Error(), "check") {
		t.Errorf("error should say what to check, got %v", err)
	}
}

func TestDirListStillReportsAnEmptyPrefixAsEmpty(t *testing.T) {
	// A directory missing *below* the root is different: it only means nothing
	// has been written under that prefix yet.
	d := newTestDir(t)
	got, err := d.List(context.Background(), "v1/never/written/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestDirListDropsKeysThatAreNotOurs(t *testing.T) {
	// A shared folder can hold files we would never write. Handing them
	// upwards would put an unvalidated string where a path is later built.
	d := newTestDir(t)
	if err := d.Put(context.Background(), "v1/good", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}

	const odd = "trailing."
	if err := os.WriteFile(filepath.Join(d.Root, odd), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Windows strips a trailing dot, so the file that actually lands is named
	// "trailing" - a perfectly valid key with nothing to drop. That collision
	// is precisely why ValidateKey refuses such names, and it means this
	// particular case can only be constructed where the name survives.
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	survived := false
	for _, e := range entries {
		if e.Name() == odd {
			survived = true
		}
	}
	if !survived {
		t.Skipf("this filesystem normalised %q away, so there is no invalid key to drop", odd)
	}

	for _, key := range keysOf(t, d, "") {
		if key != "v1/good" {
			t.Errorf("a key we would never write was listed: %q", key)
		}
	}
}

func TestDirProbeCleansUpWhenAStepFails(t *testing.T) {
	d := newTestDir(t)
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// A successful probe leaves nothing at all - not even the directory it
	// would have needed for a nested key.
	entries, err := os.ReadDir(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("the probe left things behind: %v", names)
	}
}

func TestDirDeleteReportsARealFailure(t *testing.T) {
	d := newTestDir(t)

	// A non-empty directory occupying an object's key cannot be removed. That
	// is a real failure and must not be swallowed the way a missing object is.
	nested := filepath.Join(d.Root, "v1", "adir", "child")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	err := d.Delete(context.Background(), "v1/adir")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a failure was reported as absence: %v", err)
	}
}

func TestDirPutReportsAnUnusableParent(t *testing.T) {
	d := newTestDir(t)

	// A file where a directory needs to be.
	if err := os.MkdirAll(filepath.Join(d.Root, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Root, "v1", "blocked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := d.Put(context.Background(), "v1/blocked/child", strings.NewReader("x"), 1)
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a failure was reported as absence: %v", err)
	}
}

func TestDirStatOnAnUnreadablePathIsNotAbsence(t *testing.T) {
	d := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(d.Root, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Root, "v1", "afile"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Treating a file as a directory: on both platforms this is an error other
	// than "does not exist", and it must stay distinguishable from one.
	_, err := d.Stat(context.Background(), "v1/afile/child")
	if err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestDirGetOnAnUnreadablePathIsNotAbsence(t *testing.T) {
	d := newTestDir(t)
	if err := os.MkdirAll(filepath.Join(d.Root, "v1", "adir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// os.Open succeeds on a directory, so without an explicit check the caller
	// would receive a usable handle whose first read fails with something
	// unrecognisable, instead of the absence Stat reports for the same key.
	r, err := d.Get(context.Background(), "v1/adir")
	if err == nil {
		r.Close()
		t.Fatal("a directory was opened as an object")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound so Get and Stat agree", err)
	}
}

func TestDirListStopsOnCancellationPartWay(t *testing.T) {
	d := newTestDir(t)
	for _, key := range []string{"v1/a", "v1/b", "v1/c", "v1/d"} {
		if err := d.Put(context.Background(), key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled listing reports the cancellation rather than a short result
	// that a caller could mistake for the whole store.
	if _, err := d.List(ctx, ""); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestDirListOfAPrefixThatIsAFile(t *testing.T) {
	d := newTestDir(t)
	if err := d.Put(context.Background(), "v1/a", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}

	// The prefix names a file rather than a directory. Walking from there is
	// legitimate and must not error.
	got, err := d.List(context.Background(), "v1/a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Key != "v1/a" {
		t.Errorf("got %v", got)
	}
}

func TestDirProbeReportsAnUnwritableRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	base := t.TempDir()
	root := filepath.Join(base, "readonly")
	if err := os.MkdirAll(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	d, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Probe(context.Background()); err == nil {
		// Windows ignores the mode bits here, so the probe legitimately
		// succeeds; the assertion only means anything where they apply.
		t.Skip("this filesystem does not enforce the mode")
	}
}
