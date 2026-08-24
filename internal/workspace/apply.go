package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

const (
	StateUnchanged   = "unchanged"
	StateMissing     = "missing"
	StateChanged     = "changed"
	StateConflict    = "conflict"
	StateUnavailable = "unavailable"
	StateManual      = "manual"
	StateApplied     = "applied"
	StateFailed      = "failed"
)

var ErrUnsafeApplyPath = errors.New("workspace: unsafe apply path")

// LocalFileState is the target-device view of one remote snapshot entry.
// Path and Backup are local-only output and are never stored in the snapshot.
type LocalFileState struct {
	Path   string
	State  string
	Backup string
	Reason string
}

// InspectFile compares one remote entry with the current project root. It
// never writes files and never runs Git commands.
func InspectFile(source File, root string) LocalFileState {
	if err := source.validate(); err != nil {
		return LocalFileState{State: StateUnavailable, Reason: "remote workspace entry is invalid"}
	}
	path, err := targetPath(source.Path, root)
	if err != nil {
		return LocalFileState{Path: path, State: StateFailed, Reason: err.Error()}
	}
	state := LocalFileState{Path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		switch source.Kind {
		case KindAbsent:
			state.State = StateUnchanged
		case KindDirectory, KindFile:
			state.State = StateMissing
		}
		if source.Kind == KindFile && !source.Available {
			state.State = StateManual
			state.Reason = source.Reason
		}
		return state
	}
	if err != nil {
		state.State = StateUnavailable
		state.Reason = "the local path could not be inspected"
		return state
	}
	if info.Mode()&os.ModeSymlink != 0 {
		state.State = StateFailed
		state.Reason = "symbolic links are not overwritten"
		return state
	}
	switch source.Kind {
	case KindAbsent:
		state.State = StateConflict
		state.Reason = "the local entry exists; apply will not delete it"
	case KindDirectory:
		if info.IsDir() {
			state.State = StateUnchanged
		} else {
			state.State = StateConflict
			state.Reason = "the local entry is not a directory"
		}
	case KindFile:
		if !source.Available {
			state.State = StateManual
			state.Reason = source.Reason
			return state
		}
		if info.IsDir() {
			state.State = StateConflict
			state.Reason = "the local entry is a directory"
			return state
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			state.State = StateUnavailable
			state.Reason = "the local file could not be read"
			return state
		}
		if blobDigest(content) == source.Digest {
			state.State = StateUnchanged
		} else {
			state.State = StateChanged
		}
	}
	return state
}

// ApplyFile writes one safe file or creates one missing directory after the
// caller has explicitly confirmed the operation. Existing files are backed up
// before replacement. Deletes, Git commands and process execution are never
// implicit.
func ApplyFile(source File, root, backupRoot string) (LocalFileState, error) {
	state := InspectFile(source, root)
	switch state.State {
	case StateUnchanged, StateManual, StateConflict, StateUnavailable:
		return state, nil
	case StateFailed:
		return state, fmt.Errorf("%w: %s", ErrUnsafeApplyPath, state.Reason)
	}
	if source.Kind == KindDirectory {
		if state.State != StateMissing {
			return state, nil
		}
		if _, err := targetPath(source.Path, root); err != nil {
			state.State = StateFailed
			state.Reason = err.Error()
			return state, err
		}
		if err := os.MkdirAll(state.Path, 0o700); err != nil {
			state.State = StateFailed
			state.Reason = "create workspace directory failed"
			return state, err
		}
		state.State = StateApplied
		return state, nil
	}
	if source.Kind != KindFile || !source.Available {
		return state, nil
	}

	var existing []byte
	if state.State == StateChanged {
		var err error
		existing, err = os.ReadFile(state.Path)
		if err != nil {
			state.State = StateFailed
			state.Reason = "read existing workspace file failed"
			return state, err
		}
		if strings.TrimSpace(backupRoot) == "" {
			state.State = StateFailed
			state.Reason = "backup directory is required before replacement"
			return state, errors.New(state.Reason)
		}
		if err := os.MkdirAll(backupRoot, 0o700); err != nil {
			state.State = StateFailed
			state.Reason = "create workspace backup directory failed"
			return state, err
		}
		state.Backup = filepath.Join(backupRoot, backupFileName(source.Path))
		if err := atomicfile.WriteBytes(state.Backup, existing); err != nil {
			state.State = StateFailed
			state.Reason = "write workspace backup failed"
			return state, err
		}
	}
	if _, err := targetPath(source.Path, root); err != nil {
		state.State = StateFailed
		state.Reason = err.Error()
		return state, err
	}
	if err := os.MkdirAll(filepath.Dir(state.Path), 0o700); err != nil {
		state.State = StateFailed
		state.Reason = "create workspace parent directory failed"
		return state, err
	}
	if _, err := targetPath(source.Path, root); err != nil {
		state.State = StateFailed
		state.Reason = err.Error()
		return state, err
	}
	if err := atomicfile.WriteBytes(state.Path, source.Content); err != nil {
		state.State = StateFailed
		state.Reason = "write workspace file failed"
		return state, err
	}
	state.State = StateApplied
	state.Reason = ""
	return state, nil
}

func targetPath(relative, root string) (string, error) {
	if !validRelativePath(relative) || unsafeFilePath(relative) {
		return "", ErrUnsafeApplyPath
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workspace root is not a directory")
	}
	target, err := filepath.Abs(filepath.Join(absoluteRoot, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("resolve workspace file: %w", err)
	}
	if !pathWithin(absoluteRoot, target) {
		return "", ErrUnsafeApplyPath
	}
	if err := resolvedPathWithin(absoluteRoot, target); err != nil {
		return "", err
	}
	return target, nil
}

func resolvedPathWithin(root, target string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	current := target
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			canonicalCurrent, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return fmt.Errorf("resolve workspace path: %w", resolveErr)
			}
			if !pathWithin(canonicalRoot, canonicalCurrent) {
				return ErrUnsafeApplyPath
			}
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect workspace path: %w", statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ErrUnsafeApplyPath
		}
		current = parent
	}
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func backupFileName(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:8]) + ".bak"
}
