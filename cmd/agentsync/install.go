package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type installOptions struct {
	dir    string
	noPath bool
}

type installResult struct {
	sourcePath string
	targetPath string
	pathReady  bool
	pathHint   string
}

func init() {
	for i := range commands {
		if commands[i].name == "install" {
			commands[i].run = runInstall
		}
	}
}

func runInstall(args []string) error {
	options, err := parseInstallArgs(args)
	if err != nil {
		return err
	}

	result, err := installCurrentExecutable(options)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(os.Stdout, "agentsync: installed command at %s\n", result.targetPath); err != nil {
		return err
	}
	if options.noPath {
		_, err := fmt.Fprintln(os.Stdout, "agentsync: PATH was not changed because --no-path was supplied")
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, result.pathHint)
	return err
}

func parseInstallArgs(args []string) (installOptions, error) {
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "installation directory")
	noPath := flags.Bool("no-path", false, "do not update the user PATH")
	if err := flags.Parse(args); err != nil {
		return installOptions{}, fmt.Errorf("install: %w", err)
	}
	if flags.NArg() != 0 {
		return installOptions{}, fmt.Errorf("install: unexpected argument %q", flags.Arg(0))
	}
	return installOptions{dir: strings.TrimSpace(*dir), noPath: *noPath}, nil
}

func installCurrentExecutable(options installOptions) (installResult, error) {
	sourcePath, err := os.Executable()
	if err != nil {
		return installResult{}, fmt.Errorf("install: locate the current executable: %w", err)
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return installResult{}, fmt.Errorf("install: resolve the current executable: %w", err)
	}

	installDir := options.dir
	if installDir == "" {
		installDir, err = defaultInstallDir()
		if err != nil {
			return installResult{}, fmt.Errorf("install: choose the installation directory: %w", err)
		}
	}
	installDir, err = filepath.Abs(installDir)
	if err != nil {
		return installResult{}, fmt.Errorf("install: resolve the installation directory: %w", err)
	}
	targetPath := filepath.Join(installDir, installedExecutableName())
	if err := installExecutableFile(sourcePath, targetPath); err != nil {
		return installResult{}, fmt.Errorf("install: copy executable: %w", err)
	}

	result := installResult{sourcePath: sourcePath, targetPath: targetPath}
	if options.noPath {
		return result, nil
	}
	result.pathReady, err = persistUserPath(installDir)
	if err != nil {
		return result, fmt.Errorf("install: update user PATH: %w", err)
	}
	result.pathHint = installPathMessage(installDir, result.pathReady)
	return result, nil
}

func installExecutableFile(sourcePath, targetPath string) error {
	if sameInstallPath(sourcePath, targetPath) {
		return nil
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("source executable is a directory")
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(targetPath), ".agentsync-install-*")
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

	if _, err := io.Copy(temporary, source); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(info.Mode().Perm() | 0o111); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func sameInstallPath(first, second string) bool {
	first, _ = filepath.Abs(first)
	second, _ = filepath.Abs(second)
	return strings.EqualFold(filepath.Clean(first), filepath.Clean(second))
}

func pathListContains(pathList, target string) bool {
	target = cleanInstallPath(target)
	for _, entry := range strings.Split(pathList, string(os.PathListSeparator)) {
		cleaned := cleanInstallPath(entry)
		if cleaned == target || strings.EqualFold(cleaned, target) {
			return true
		}
	}
	return false
}

func cleanInstallPath(path string) string {
	path = strings.Trim(strings.TrimSpace(path), `"`)
	path = os.ExpandEnv(path)
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	return filepath.Clean(path)
}
