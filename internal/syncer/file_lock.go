package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalFileLock is an advisory lock shared by CtxHop processes on one device.
// The lock file contains no application data and can remain on disk after the
// process exits; the operating system releases the lock with the file handle.
type LocalFileLock struct {
	file *os.File
}

// AcquireLocalFileLock opens path and waits until this process owns its
// exclusive advisory lock. The wait observes ctx so a cancelled push does not
// remain blocked behind another process forever.
func AcquireLocalFileLock(ctx context.Context, path string) (*LocalFileLock, error) {
	if ctx == nil {
		return nil, errors.New("syncer: file lock context is required")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("syncer: file lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("syncer: create file lock directory: %w", statePathSafe(err))
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("syncer: open file lock: %w", statePathSafe(err))
	}
	if err := lockLocalFile(ctx, file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("syncer: acquire file lock: %w", err)
	}
	return &LocalFileLock{file: file}, nil
}

// Close releases the operating-system lock and closes the lock file.
func (l *LocalFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockLocalFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
