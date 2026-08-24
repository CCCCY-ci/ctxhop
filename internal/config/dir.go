// Package config reads and writes what has to survive between runs: which
// backend to use, the credentials for it, the public key to encrypt to, and
// which projects the user wants synced.
//
// It decides nothing. It knows nothing about sessions, git or encryption
// algorithms - it moves structured settings to and from disk.
//
// It exists as its own package because it is the only place that decides what
// lands on the filesystem. Getting that wrong means either a leak or a lockout,
// so it is kept in one place rather than spread across the commands that happen
// to need a value (spec §1.1).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// dirEnv overrides the platform location.
//
// Not only for tests. An agent's hook inherits whatever environment the agent
// was started with, which need not be the user's shell - the adapter layer
// found exactly this with CLAUDE_CONFIG_DIR - so there has to be one explicit
// way to say where the configuration lives (spec §2).
const dirEnv = "CTXHOP_CONFIG_DIR"

// appDir is the directory name under the user's home directory.
const appDir = ".ctxhop"

// ErrNotInitialised reports that this machine has no configuration yet.
//
// It is a sentinel because the answer for a caller is a specific instruction -
// run init - rather than a failure to report.
var ErrNotInitialised = errors.New("config: this machine is not set up yet; run 'ctxhop init'")

// Dir returns the configuration directory.
//
// The default is intentionally visible and easy to back up: ~/.ctxhop on
// every platform. CTXHOP_CONFIG_DIR remains an explicit override for tests,
// portable installations and service accounts.
func Dir() (string, error) {
	if dir := os.Getenv(dirEnv); dir != "" {
		return filepath.Clean(dir), nil
	}

	root, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the user home directory: %w; set %s to choose one", err, dirEnv)
	}
	return filepath.Join(root, appDir), nil
}
