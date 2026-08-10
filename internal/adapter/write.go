package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrSessionExists reports that the target session is already present.
//
// Whether an existing session may be replaced is a sync-layer decision - a
// fast-forward legitimately replaces it, a fork must not - so the adapter
// refuses by default and offers ReplaceSession for the case where the caller
// has established that replacing is correct (spec §5).
var ErrSessionExists = errors.New("adapter: session already exists")

// ErrInvalidRecord reports a record that would break the file's line structure.
var ErrInvalidRecord = errors.New("adapter: record is not a single line")

// WriteSession installs a new session, failing if one with that id is already
// present.
func (l Layout) WriteSession(projectRoot, sessionID string, records [][]byte) error {
	path := l.SessionFile(projectRoot, sessionID)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrSessionExists, sessionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check existing session: %w", err)
	}
	return l.writeSession(path, records)
}

// ReplaceSession installs a session over any existing one. The caller is
// asserting that replacing is correct, which for a session log means the new
// content extends the old rather than diverging from it (BR-03).
func (l Layout) ReplaceSession(projectRoot, sessionID string, records [][]byte) error {
	return l.writeSession(l.SessionFile(projectRoot, sessionID), records)
}

// writeSession writes records to path atomically.
//
// Everything is validated before anything is created, so a rejected write
// leaves the filesystem exactly as it was.
func (l Layout) writeSession(path string, records [][]byte) error {
	if len(records) == 0 {
		return errors.New("adapter: refusing to write a session with no records")
	}
	for i, rec := range records {
		if bytes.ContainsAny(rec, "\r\n") {
			return fmt.Errorf("%w: record %d", ErrInvalidRecord, i+1)
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}

	return writeFileAtomic(dir, path, records)
}

// writeRecords renders records as JSONL: one record per line, every line
// terminated.
//
// The terminator matters as much as the content. ReadRecords only trusts a
// record once its newline has landed, so a final record written without one
// would be silently dropped by our own reader on the next pass.
//
// Kept separate from the atomic publish so that the formatting can be tested
// against a writer that fails partway, which no real filesystem will do on
// demand.
func writeRecords(w io.Writer, records [][]byte) error {
	bw := bufio.NewWriter(w)
	for i, rec := range records {
		if _, err := bw.Write(rec); err != nil {
			return fmt.Errorf("write record %d: %w", i+1, err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("terminate record %d: %w", i+1, err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush session: %w", err)
	}
	return nil
}

// writeFileAtomic writes records through a temporary file in the same
// directory, then renames it into place.
//
// The temporary file must share the destination's directory: rename is only
// atomic within a filesystem, and a temp directory can easily be on another
// volume. The suffix keeps it out of the way of the agent, which only reads
// `.jsonl` files (BR-11).
func writeFileAtomic(dir, path string, records [][]byte) (err error) {
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()

	// Any failure from here on must leave nothing behind. The agent's data
	// directory is not ours to litter.
	defer func() {
		if err != nil {
			tmp.Close()
			if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary file: %w", rmErr))
			}
		}
	}()

	if err = writeRecords(tmp, records); err != nil {
		return err
	}

	// Durability before visibility. Without this the rename could publish a
	// name whose contents have not reached disk, so a power cut would leave a
	// truncated session under the real filename - the exact state BR-11 exists
	// to prevent. Skipping the parent directory's own fsync is deliberate: if
	// the rename itself is lost, the previous file is still intact, and either
	// outcome is a state the agent can use.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync session: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close session: %w", err)
	}

	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install session: %w", err)
	}
	return nil
}
