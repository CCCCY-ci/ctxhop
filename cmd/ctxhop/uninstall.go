package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type uninstallOptions struct {
	dir string
}

type uninstallResult struct {
	targetPath       string
	existed          bool
	removalScheduled bool
	pathRemoved      bool
}

func init() {
	for i := range commands {
		if commands[i].name == "uninstall" {
			commands[i].run = runUninstall
		}
	}
}

func runUninstall(args []string) error {
	options, err := parseUninstallArgs(args)
	if err != nil {
		return err
	}

	result, err := uninstallInstalledExecutable(options)
	if err != nil {
		return err
	}
	if result.existed {
		if result.removalScheduled {
			if _, err := fmt.Fprintf(os.Stdout, "%s: removal scheduled for %s\n", cliName, result.targetPath); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(os.Stdout, "%s: removed command at %s\n", cliName, result.targetPath); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(os.Stdout, "%s: installed command was not found at %s\n", cliName, result.targetPath); err != nil {
		return err
	}
	if result.pathRemoved {
		if _, err := fmt.Fprintf(os.Stdout, "%s: removed the installation directory from the user PATH; open a new terminal\n", cliName); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(os.Stdout, "%s: configuration, device keys, sync state, and remote data were kept\n", cliName)
	return err
}

func parseUninstallArgs(args []string) (uninstallOptions, error) {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "installation directory")
	if err := flags.Parse(args); err != nil {
		return uninstallOptions{}, fmt.Errorf("uninstall: %w", err)
	}
	if flags.NArg() != 0 {
		return uninstallOptions{}, fmt.Errorf("uninstall: unexpected argument %q", flags.Arg(0))
	}
	return uninstallOptions{dir: strings.TrimSpace(*dir)}, nil
}

func uninstallInstalledExecutable(options uninstallOptions) (uninstallResult, error) {
	installDir := options.dir
	if installDir == "" {
		var err error
		installDir, err = defaultInstallDir()
		if err != nil {
			return uninstallResult{}, fmt.Errorf("uninstall: choose the installation directory: %w", err)
		}
	}
	installDir, err := filepath.Abs(installDir)
	if err != nil {
		return uninstallResult{}, fmt.Errorf("uninstall: resolve the installation directory: %w", err)
	}
	targetPath := filepath.Join(installDir, installedExecutableName())
	result := uninstallResult{targetPath: targetPath}

	if info, statErr := os.Lstat(targetPath); statErr == nil {
		if info.IsDir() {
			return result, fmt.Errorf("uninstall: refusing to remove directory %s", targetPath)
		}
		result.existed = true
		currentPath, currentErr := os.Executable()
		if currentErr == nil {
			currentPath, currentErr = filepath.Abs(currentPath)
		}
		running := currentErr == nil && sameInstallPath(currentPath, targetPath)
		result.removalScheduled, err = removeInstalledExecutable(targetPath, running)
		if err != nil {
			return result, fmt.Errorf("uninstall: remove executable: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return result, fmt.Errorf("uninstall: inspect executable: %w", statErr)
	}

	result.pathRemoved, err = removeUserPath(installDir)
	if err != nil {
		return result, fmt.Errorf("uninstall: update user PATH: %w", err)
	}
	return result, nil
}
