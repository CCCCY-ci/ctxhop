//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

func installedExecutableName() string {
	return cliName + ".exe"
}

func defaultInstallDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ctxhop", "bin"), nil
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

func removeUserPath(dir string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	current, _, err := key.GetStringValue("Path")
	if err == registry.ErrNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	target := cleanInstallPath(dir)
	entries := strings.Split(current, string(os.PathListSeparator))
	kept := make([]string, 0, len(entries))
	removed := false
	for _, entry := range entries {
		cleaned := cleanInstallPath(entry)
		if cleaned == target || strings.EqualFold(cleaned, target) {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return false, nil
	}
	if err := key.SetExpandStringValue("Path", strings.Join(kept, string(os.PathListSeparator))); err != nil {
		return false, err
	}
	return true, nil
}

func removeInstalledExecutable(path, configDir string, running bool) (bool, error) {
	if !running {
		if err := retryInstallFile(func() error { return os.Remove(path) }); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		return false, removeInstallDirectory(configDir)
	}

	// Windows keeps the running executable open. Start a detached cmd helper
	// that waits for this process to exit, removes the exact executable path,
	// removes the entire local CtxHop directory, and then removes its own
	// temporary script.
	script := strings.Join([]string{
		"@echo off",
		"setlocal EnableExtensions DisableDelayedExpansion",
		"set /a attempts=0",
		":retry",
		"del /f /q \"%CTXHOP_UNINSTALL_TARGET%\" >nul 2>&1",
		"rmdir /s /q \"%CTXHOP_UNINSTALL_CONFIG_DIR%\" >nul 2>&1",
		"if not exist \"%CTXHOP_UNINSTALL_TARGET%\" if not exist \"%CTXHOP_UNINSTALL_CONFIG_DIR%\" goto cleanup",
		"set /a attempts+=1",
		"if %attempts% GEQ 20 goto cleanup",
		"ping 127.0.0.1 -n 2 >nul",
		"goto retry",
		":cleanup",
		"del /f /q \"%~f0\" >nul 2>&1",
		"exit /b 0",
	}, "\r\n") + "\r\n"
	temporary, err := os.CreateTemp("", "ctxhop-uninstall-*.cmd")
	if err != nil {
		return false, err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.WriteString(script); err != nil {
		_ = temporary.Close()
		return false, err
	}
	if err := temporary.Close(); err != nil {
		return false, err
	}

	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		upper := strings.ToUpper(entry)
		if strings.HasPrefix(upper, "CTXHOP_UNINSTALL_TARGET=") || strings.HasPrefix(upper, "CTXHOP_UNINSTALL_CONFIG_DIR=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "CTXHOP_UNINSTALL_TARGET="+path)
	env = append(env, "CTXHOP_UNINSTALL_CONFIG_DIR="+configDir)
	command := exec.Command("cmd.exe", "/d", "/c", "call", temporaryPath)
	command.Env = env
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	if err := command.Start(); err != nil {
		return false, err
	}
	if err := command.Process.Release(); err != nil {
		return false, err
	}
	removeTemporary = false
	return true, nil
}

func installPathMessage(dir string, pathReady bool) string {
	if pathReady {
		return fmt.Sprintf("%s: %s is configured on the user PATH; open a new terminal, then run '%s version'", cliName, dir, cliName)
	}
	return fmt.Sprintf("%s: add %s to the user PATH, then open a new terminal", cliName, dir)
}
