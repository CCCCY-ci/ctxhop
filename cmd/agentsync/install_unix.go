//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func installedExecutableName() string {
	return "agentsync"
}

func defaultInstallDir() (string, error) {
	if dir := os.Getenv("XDG_BIN_HOME"); dir != "" {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func persistUserPath(dir string) (bool, error) {
	return pathListContains(os.Getenv("PATH"), dir), nil
}

func installPathMessage(dir string, pathReady bool) string {
	if pathReady {
		return fmt.Sprintf("agentsync: %s is already on PATH; run 'agentsync version'", dir)
	}
	return fmt.Sprintf("agentsync: add %s to PATH for future shells, for example: export PATH=%q:$PATH", dir, dir)
}
