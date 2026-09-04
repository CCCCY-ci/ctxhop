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
)

type updateReplacementResult struct {
	targetPath  string
	scheduled   bool
	keepWorkDir bool
}

func replaceCurrentExecutable(sourcePath, workDir string) (updateReplacementResult, error) {
	targetPath, err := currentExecutablePath()
	if err != nil {
		return updateReplacementResult{}, err
	}
	if err := scheduleWindowsExecutableReplacement(sourcePath, targetPath, workDir); err != nil {
		return updateReplacementResult{}, err
	}
	return updateReplacementResult{
		targetPath:  targetPath,
		scheduled:   true,
		keepWorkDir: true,
	}, nil
}

func scheduleWindowsExecutableReplacement(sourcePath, targetPath, workDir string) error {
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(targetPath) == "" || strings.TrimSpace(workDir) == "" {
		return fmt.Errorf("update paths cannot be empty")
	}
	scriptPath := filepath.Join(workDir, "replace.cmd")
	if err := os.WriteFile(scriptPath, []byte(windowsUpdateScript()), 0o600); err != nil {
		return fmt.Errorf("write update helper: %w", err)
	}
	commandInterpreter := strings.TrimSpace(os.Getenv("COMSPEC"))
	if commandInterpreter == "" {
		commandInterpreter = "cmd.exe"
	}

	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		upper := strings.ToUpper(entry)
		if strings.HasPrefix(upper, "CTXHOP_UPDATE_SOURCE=") ||
			strings.HasPrefix(upper, "CTXHOP_UPDATE_TARGET=") ||
			strings.HasPrefix(upper, "CTXHOP_UPDATE_DIR=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"CTXHOP_UPDATE_SOURCE="+sourcePath,
		"CTXHOP_UPDATE_TARGET="+targetPath,
		"CTXHOP_UPDATE_DIR="+workDir,
	)

	command := exec.Command(commandInterpreter, "/d", "/c", "call", scriptPath)
	command.Env = env
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start update helper: %w", err)
	}
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("detach update helper: %w", err)
	}
	return nil
}

func windowsUpdateScript() string {
	return "@echo off\r\n" +
		"setlocal EnableExtensions DisableDelayedExpansion\r\n" +
		"set /a attempts=0\r\n" +
		":retry\r\n" +
		"if exist \"%CTXHOP_UPDATE_TARGET%\" move /y \"%CTXHOP_UPDATE_TARGET%\" \"%CTXHOP_UPDATE_DIR%\\previous.exe\" >nul 2>&1\r\n" +
		"if exist \"%CTXHOP_UPDATE_TARGET%\" goto wait\r\n" +
		"move /y \"%CTXHOP_UPDATE_SOURCE%\" \"%CTXHOP_UPDATE_TARGET%\" >nul 2>&1\r\n" +
		"if exist \"%CTXHOP_UPDATE_TARGET%\" if not exist \"%CTXHOP_UPDATE_SOURCE%\" goto success\r\n" +
		"if exist \"%CTXHOP_UPDATE_DIR%\\previous.exe\" move /y \"%CTXHOP_UPDATE_DIR%\\previous.exe\" \"%CTXHOP_UPDATE_TARGET%\" >nul 2>&1\r\n" +
		":wait\r\n" +
		"set /a attempts+=1\r\n" +
		"if %attempts% GEQ 60 goto fail\r\n" +
		"ping 127.0.0.1 -n 2 >nul\r\n" +
		"goto retry\r\n" +
		":success\r\n" +
		"del /f /q \"%CTXHOP_UPDATE_DIR%\\previous.exe\" >nul 2>&1\r\n" +
		"cd /d \"%TEMP%\" >nul 2>&1\r\n" +
		"del /f /q \"%~f0\" >nul 2>&1\r\n" +
		"rmdir /s /q \"%CTXHOP_UPDATE_DIR%\" >nul 2>&1\r\n" +
		"exit /b 0\r\n" +
		":fail\r\n" +
		"exit /b 1\r\n"
}
