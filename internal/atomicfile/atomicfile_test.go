package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestWriteBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")

	if err := WriteBytes(path, []byte("hello")); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q", data)
	}
	if got := leftovers(t, dir); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")

	if err := WriteBytes(path, []byte("a much longer original value")); err != nil {
		t.Fatal(err)
	}
	if err := WriteBytes(path, []byte("short")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A partial overwrite would leave a mixture of the two.
	if string(data) != "short" {
		t.Errorf("got %q", data)
	}
}

func TestWriteUsesTheDestinationDirectory(t *testing.T) {
	// Rename is only atomic within a filesystem, so the temporary file has to
	// be a sibling of the destination rather than in a system temp directory
	// that may be on another volume.
	dir := t.TempDir()
	path := filepath.Join(dir, "file")

	var seen string
	err := Write(path, func(w io.Writer) error {
		if f, ok := w.(*os.File); ok {
			seen = filepath.Dir(f.Name())
		}
		_, err := w.Write([]byte("x"))
		return err
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if seen != dir {
		t.Errorf("temporary file was in %q, want %q", seen, dir)
	}
}

func TestWriteLeavesNothingWhenTheWriterFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	sentinel := errors.New("writer failed")

	err := Write(path, func(w io.Writer) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want the writer's error", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failed write published the file")
	}
	if got := leftovers(t, dir); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteLeavesTheOriginalWhenTheWriterFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")

	if err := WriteBytes(path, []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, func(w io.Writer) error { return errors.New("nope") }); err == nil {
		t.Fatal("expected an error")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted write leaves the previous state, never a partial one.
	if string(data) != "original" {
		t.Errorf("the original was damaged: %q", data)
	}
	if got := leftovers(t, dir); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteReportsAFailedPublish(t *testing.T) {
	// A directory where the file belongs makes the rename fail after the
	// contents are written and synced - the interrupted-publish case.
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	err := WriteBytes(path, []byte("x"))
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "install file") {
		t.Errorf("the error should name the failing step, got %v", err)
	}
	if got := leftovers(t, dir); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteReportsAnUnusableDirectory(t *testing.T) {
	// The parent is a file, so no temporary file can be created there.
	base := t.TempDir()
	notADir := filepath.Join(base, "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteBytes(filepath.Join(notADir, "file"), []byte("x")); err == nil {
		t.Error("expected an error, got none")
	}
}
