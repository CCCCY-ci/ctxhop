//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

func installedExecutableName() string {
	return "agentsync.exe"
}

func defaultInstallDir() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "AgentSync", "bin"), nil
}

func persistUserPath(dir string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	current, _, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, err
	}
	if pathListContains(current, dir) {
		return true, nil
	}
	if current == "" {
		current = dir
	} else {
		current += string(os.PathListSeparator) + dir
	}
	if err := key.SetExpandStringValue("Path", current); err != nil {
		return false, err
	}
	return true, nil
}

func installPathMessage(dir string, pathReady bool) string {
	if pathReady {
		return fmt.Sprintf("agentsync: %s is configured on the user PATH; open a new terminal, then run 'agentsync version'", dir)
	}
	return fmt.Sprintf("agentsync: add %s to the user PATH, then open a new terminal", dir)
}
