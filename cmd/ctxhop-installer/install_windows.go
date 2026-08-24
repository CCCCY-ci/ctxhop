//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
		commandInterpreter = "cmd.exe"
	}
	commandLine := installerWelcomeCommandLine(targetPath)
	command := exec.Command(commandInterpreter, "/K", commandLine)
	command.Dir = filepath.Dir(targetPath)
	command.Env = installerWelcomeEnvironment(targetPath)
	return command.Start()
}

func installerWelcomeCommandLine(targetPath string) string {
	return fmt.Sprintf(`""%s" --installer-welcome"`, targetPath)
}

func installerWelcomeEnvironment(targetPath string) []string {
	pathValue := filepath.Dir(targetPath) + string(os.PathListSeparator) + os.Getenv("PATH")
	return append(os.Environ(), "PATH="+pathValue)
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
