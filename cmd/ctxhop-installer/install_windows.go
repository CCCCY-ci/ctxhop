//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func installPayload(payload []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	installDir := filepath.Join(home, ".ctxhop", "bin")
	targetPath := filepath.Join(installDir, "ctxhop.exe")
	if err := installPayloadFile(payload, targetPath); err != nil {
		return "", err
	}
	if _, err := persistInstallerPath(installDir); err != nil {
		return targetPath, fmt.Errorf("update user PATH: %w; CtxHop was installed at %s", err, targetPath)
	}
	return targetPath, nil
}

func installPayloadFile(payload []byte, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".ctxhop-installer-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	backupPath := ""
	targetMoved := false
	if _, err := os.Lstat(targetPath); err == nil {
		backup, createErr := os.CreateTemp(filepath.Dir(targetPath), ".ctxhop-installer-backup-*")
		if createErr != nil {
			return createErr
		}
		backupPath = backup.Name()
		if closeErr := backup.Close(); closeErr != nil {
			_ = os.Remove(backupPath)
			return closeErr
		}
		if err := retryInstallerFile(func() error { return os.Remove(backupPath) }); err != nil {
			return err
		}
		if err := retryInstallerFile(func() error { return os.Rename(targetPath, backupPath) }); err != nil {
			return err
		}
		targetMoved = true
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := retryInstallerFile(func() error { return os.Rename(temporaryPath, targetPath) }); err != nil {
		if targetMoved {
			if restoreErr := retryInstallerFile(func() error { return os.Rename(backupPath, targetPath) }); restoreErr != nil {
				return fmt.Errorf("replace executable: %w; rollback failed: %v", err, restoreErr)
			}
		}
		return err
	}
	removeTemporary = false
	if targetMoved {
		if err := retryInstallerFile(func() error { return os.Remove(backupPath) }); err != nil {
			rollbackErr := retryInstallerFile(func() error { return os.Remove(targetPath) })
			if rollbackErr == nil || os.IsNotExist(rollbackErr) {
				rollbackErr = retryInstallerFile(func() error { return os.Rename(backupPath, targetPath) })
			}
			if rollbackErr != nil {
				return fmt.Errorf("remove install backup: %w; rollback failed: %v", err, rollbackErr)
			}
			return fmt.Errorf("remove install backup: %w; installation rolled back", err)
		}
	}
	return nil
}

func retryInstallerFile(operation func() error) error {
	err := operation()
	if err == nil {
		return nil
	}
	for _, delay := range []time.Duration{50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond} {
		time.Sleep(delay)
		err = operation()
		if err == nil {
			return nil
		}
	}
	return err
}

func persistInstallerPath(dir string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer key.Close()

	current, _, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, err
	}
	if installerPathListContains(current, dir) {
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

func installerPathListContains(pathList, target string) bool {
	target = filepath.Clean(target)
	for _, entry := range strings.Split(pathList, string(os.PathListSeparator)) {
		entry = strings.Trim(strings.TrimSpace(entry), `"`)
		if strings.EqualFold(filepath.Clean(entry), target) {
			return true
		}
	}
	return false
}

func reportInstallerFailure(err error) {
	message := fmt.Sprintf("CtxHop could not be installed.\n\n%s", err)
	showInstallerMessage("CtxHop installation failed", message, true)
}

func reportInstallerSuccess(targetPath string) {
	if err := launchInstallerWelcome(targetPath); err == nil {
		return
	}
	message := fmt.Sprintf("CtxHop was installed for the current user at:\n%s\n\nOpen a new terminal, then run:\nctxhop version", targetPath)
	showInstallerMessage("CtxHop installation complete", message, false)
}

func launchInstallerWelcome(targetPath string) error {
	if strings.TrimSpace(targetPath) == "" {
		return fmt.Errorf("installed executable path is empty")
	}
	commandInterpreter := strings.TrimSpace(os.Getenv("COMSPEC"))
	if commandInterpreter == "" {
		systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
		if systemRoot == "" {
			commandInterpreter = "cmd.exe"
		} else {
			commandInterpreter = filepath.Join(systemRoot, "System32", "cmd.exe")
		}
	}
	scriptPath, err := createInstallerWelcomeScript(targetPath)
	if err != nil {
		return err
	}
	process, err := startInstallerWelcomeCommand(commandInterpreter, targetPath, scriptPath)
	if err != nil {
		return withInstallerWelcomeCleanup(err, scriptPath)
	}
	result, err := windows.WaitForSingleObject(process, 2_000)
	var exitCode uint32
	var exitCodeErr error
	if err == nil && result == windows.WAIT_OBJECT_0 {
		exitCodeErr = windows.GetExitCodeProcess(process, &exitCode)
	}
	closeErr := windows.CloseHandle(process)
	if err != nil {
		return withInstallerWelcomeCleanup(fmt.Errorf("wait for welcome command: %w", err), scriptPath)
	}
	if closeErr != nil {
		return fmt.Errorf("close welcome command handle: %w", closeErr)
	}
	switch result {
	case uint32(windows.WAIT_TIMEOUT):
		// The command line removes the script after the welcome command returns.
		// Keep it in place while cmd.exe may still be starting or reading it.
		return nil
	case windows.WAIT_OBJECT_0:
		if exitCodeErr != nil {
			return withInstallerWelcomeCleanup(fmt.Errorf("welcome command exited immediately: %w", exitCodeErr), scriptPath)
		}
		return withInstallerWelcomeCleanup(fmt.Errorf("welcome command exited immediately with code %d", exitCode), scriptPath)
	default:
		return withInstallerWelcomeCleanup(fmt.Errorf("wait for welcome command returned unexpected status %#x", result), scriptPath)
	}
}

func withInstallerWelcomeCleanup(primary error, scriptPath string) error {
	if err := os.Remove(scriptPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; remove welcome script: %v", primary, err)
	}
	return primary
}

func createInstallerWelcomeScript(targetPath string) (string, error) {
	script, err := os.CreateTemp(filepath.Dir(targetPath), ".ctxhop-welcome-*.cmd")
	if err != nil {
		return "", fmt.Errorf("create welcome script: %w", err)
	}
	scriptPath := script.Name()
	if _, err := io.WriteString(script, installerWelcomeScript()); err != nil {
		closeErr := script.Close()
		removeErr := os.Remove(scriptPath)
		if closeErr != nil {
			err = fmt.Errorf("%w; close welcome script: %v", err, closeErr)
		}
		if removeErr != nil && !os.IsNotExist(removeErr) {
			err = fmt.Errorf("%w; remove welcome script: %v", err, removeErr)
		}
		return "", fmt.Errorf("write welcome script: %w", err)
	}
	if err := script.Close(); err != nil {
		if removeErr := os.Remove(scriptPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return "", fmt.Errorf("close welcome script: %w; remove welcome script: %v", err, removeErr)
		}
		return "", fmt.Errorf("close welcome script: %w", err)
	}
	return scriptPath, nil
}

func installerWelcomeScript() string {
	return "@echo off\r\n" +
		"mode con: cols=120 lines=32 >nul 2>&1\r\n" +
		".\\ctxhop.exe --installer-welcome\r\n"
}

func startInstallerWelcomeCommand(commandInterpreter, targetPath, scriptPath string) (windows.Handle, error) {
	application, err := windows.UTF16PtrFromString(commandInterpreter)
	if err != nil {
		return 0, err
	}
	commandLine, err := windows.UTF16FromString(installerWelcomeCommandLine(commandInterpreter, scriptPath))
	if err != nil {
		return 0, err
	}
	workingDirectory, err := windows.UTF16PtrFromString(filepath.Dir(targetPath))
	if err != nil {
		return 0, err
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var process windows.ProcessInformation
	if err := windows.CreateProcess(
		application,
		&commandLine[0],
		nil,
		nil,
		false,
		windows.CREATE_NEW_CONSOLE,
		nil,
		workingDirectory,
		&startup,
		&process,
	); err != nil {
		return 0, err
	}
	if err := windows.CloseHandle(process.Thread); err != nil {
		if closeProcessErr := windows.CloseHandle(process.Process); closeProcessErr != nil {
			return 0, fmt.Errorf("close welcome thread handle: %w; close process handle: %v", err, closeProcessErr)
		}
		return 0, fmt.Errorf("close welcome thread handle: %w", err)
	}
	return process.Process, nil
}

func installerWelcomeCommandLine(commandInterpreter, scriptPath string) string {
	scriptName := filepath.Base(scriptPath)
	cleanup := fmt.Sprintf("del /f /q %s >nul 2>&1", scriptName)
	return fmt.Sprintf("%s /D /K \"call %s & %s\"", syscall.EscapeArg(commandInterpreter), scriptName, cleanup)
}

func showInstallerMessage(title, message string, failure bool) {
	titlePtr, titleErr := windows.UTF16PtrFromString(title)
	messagePtr, messageErr := windows.UTF16PtrFromString(message)
	if titleErr != nil || messageErr != nil {
		return
	}
	boxType := uint32(windows.MB_OK | windows.MB_ICONINFORMATION)
	if failure {
		boxType = windows.MB_OK | windows.MB_ICONERROR
	}
	_, _ = windows.MessageBox(0, messagePtr, titlePtr, boxType)
}
