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
const dirEnv = "AGENTSYNC_CONFIG_DIR"

// appDir is the directory name under the platform's configuration root.
const appDir = "agentsync"

// ErrNotInitialised reports that this machine has no configuration yet.
//
// It is a sentinel because the answer for a caller is a specific instruction -
// run init - rather than a failure to report.
var ErrNotInitialised = errors.New("config: this machine is not set up yet; run 'agentsync init'")

// Dir returns the configuration directory, following platform convention.
//
// os.UserConfigDir already encodes those conventions: %AppData% on Windows,
// ~/Library/Application Support on macOS, and $XDG_CONFIG_HOME or ~/.config
// elsewhere. Reimplementing them would only be a chance to disagree with the
// rest of the system.
func Dir() (string, error) {
	if dir := os.Getenv(dirEnv); dir != "" {
		return filepath.Clean(dir), nil
	}

	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the configuration directory: %w; set %s to choose one", err, dirEnv)
	}
	return filepath.Join(root, appDir), nil
}
