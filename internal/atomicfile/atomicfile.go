// Package atomicfile publishes a file's contents in one step, so a reader or
// an interrupted process sees either the previous state or the new one and
// never a partial write.
//
// It exists as one package rather than a copy in each caller because the
// guarantee is only as good as its weakest implementation, and two copies drift.
// It is a utility, not a layer: nothing here knows anything about agents,
// sessions or storage.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write creates path with whatever write produces.
//
// The content goes to a temporary file in the destination's own directory,
// which is then synced and renamed over the target. The directory has to be
// shared: rename is only atomic within a filesystem, and a system temp
// directory is easily on another volume.
//
// Syncing before the rename is what makes the guarantee survive a power cut.
// Without it the rename can publish a name whose contents never reached disk,
// leaving a truncated file under the real filename - exactly the state this
// exists to prevent. The parent directory is deliberately not synced: if the
// rename itself is lost, the previous file is still intact, and either outcome
// is one a reader can use.
//
// Every failure path removes the temporary file.
func Write(path string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	closed := false

	defer func() {
		if err == nil {
			return
		}
		if !closed {
			// Reported rather than dropped: a close that fails can mean the
			// written bytes never landed, which is worth knowing even though
			// the file is about to be removed.
			if closeErr := tmp.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close temporary file: %w", closeErr))
			}
		}
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary file: %w", rmErr))
		}
	}()

	if err = write(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	closed = true

	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install file: %w", err)
	}
	return nil
}

// WriteBytes is Write for content already in memory.
func WriteBytes(path string, data []byte) error {
	return Write(path, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}
