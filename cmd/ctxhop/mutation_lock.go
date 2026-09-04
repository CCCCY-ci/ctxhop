package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// acquireLocalMutationLock serializes CtxHop commands that can mutate an
// Agent session, the local Session Hub index, or the device-owned cursor.
// push and watch use the same lock, so a local reader never observes one of
// those mutations halfway through its operation.
func acquireLocalMutationLock(ctx context.Context, configDir, operation string) (*syncer.LocalFileLock, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%s: context is required", operation)
	}
	if strings.TrimSpace(configDir) == "" {
		return nil, fmt.Errorf("%s: configuration directory is required", operation)
	}
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return nil, errors.New("local mutation lock: operation is required")
	}
	lock, err := syncer.AcquireLocalFileLock(ctx, filepath.Join(configDir, "push.lock"))
	if err != nil {
		return nil, fmt.Errorf("%s: acquire local mutation lock: %w", operation, err)
	}
	return lock, nil
}
