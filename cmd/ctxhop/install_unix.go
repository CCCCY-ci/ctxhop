//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func installedExecutableName() string {
	return cliName
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

func removeUserPath(string) (bool, error) {
	// The Unix installer does not edit shell startup files. There is no
	// CtxHop-owned PATH entry to remove here.
	return false, nil
}

func removeInstalledExecutable(path string, _ bool) (bool, error) {
	return false, os.Remove(path)
}

func installPathMessage(dir string, pathReady bool) string {
	if pathReady {
		return fmt.Sprintf("%s: %s is already on PATH; run '%s version'", cliName, dir, cliName)
	}
	return fmt.Sprintf("%s: add %s to PATH for future shells, for example: export PATH=%q:$PATH", cliName, dir, dir)
}
