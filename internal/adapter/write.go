package adapter

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
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

// ErrInvalidSessionID reports an identifier that must not be joined onto a path.
var ErrInvalidSessionID = errors.New("adapter: session id is not a safe filename")

// checkSessionID rejects identifiers that would escape the project's directory.
//
// A native id arrives as metadata from another device. Joined onto a path
// unchecked, one containing a separator or `..` walks straight out of
// ~/.claude/projects and MkdirAll obligingly builds the way. This is the only
// code path that writes into the agent's data directory, so it refuses anything
// it cannot confirm is a plain filename (BR-12).
func checkSessionID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: %q", ErrInvalidSessionID, id)
		}
	}
	return nil
}

// WriteSession installs a new session, failing if one with that id is already
// present.
func (l Layout) WriteSession(projectRoot, sessionID string, records [][]byte) error {
	if err := checkSessionID(sessionID); err != nil {
		return err
	}
	path := l.SessionFile(projectRoot, sessionID)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s", ErrSessionExists, sessionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check existing session: %w", err)
	}
	return writeSessionAt(path, records)
}

// ReplaceSession installs a session over any existing one. The caller is
// asserting that replacing is correct, which for a session log means the new
// content extends the old rather than diverging from it (BR-03).
func (l Layout) ReplaceSession(projectRoot, sessionID string, records [][]byte) error {
	if err := checkSessionID(sessionID); err != nil {
		return err
	}
	return writeSessionAt(l.SessionFile(projectRoot, sessionID), records)
}

// RemoveSession removes one native session by its already validated ID. It is
// intentionally a narrow operation used by materialization rollback after a
// newly created target fails read-back validation; callers must never use it
// to replace or clean up an existing session.
func (l Layout) RemoveSession(projectRoot, sessionID string) error {
	if err := checkSessionID(sessionID); err != nil {
		return err
	}
	if err := os.Remove(l.SessionFile(projectRoot, sessionID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove session: %w", err)
	}
	return nil
}

// writeSession writes records to path atomically.
//
// Everything is validated before anything is created, so a rejected write
// leaves the filesystem exactly as it was.
func writeSessionAt(path string, records [][]byte) error {
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

	return atomicfile.Write(path, func(w io.Writer) error {
		return writeRecords(w, records)
	})
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
