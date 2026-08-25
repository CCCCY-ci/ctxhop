package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

type uninstallOptions struct {
	dir string
}

type uninstallResult struct {
	targetPath       string
	configDir        string
	existed          bool
	configExisted    bool
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

	result, err := uninstallLocalFiles(options)
	if err != nil {
		return err
	}
	// The local configuration directory is removed by this command. Logging
	// after runUninstall would recreate ~/.ctxhop/logs in the just-removed
	// directory, undoing the uninstall on the way out of main.
	commandLogger = nil
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
	if result.removalScheduled {
		if _, err := fmt.Fprintf(os.Stdout, "%s: local configuration removal scheduled for %s\n", cliName, result.configDir); err != nil {
			return err
		}
	} else if result.configExisted {
		if _, err := fmt.Fprintf(os.Stdout, "%s: removed local configuration directory %s\n", cliName, result.configDir); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(os.Stdout, "%s: local configuration directory was not found at %s\n", cliName, result.configDir); err != nil {
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s: remote S3 objects and local directory-backend data were kept\n", cliName)
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

// uninstallLocalFiles removes the local executable and CtxHop state while
// deliberately leaving every remote or user-owned sync directory alone.
func uninstallLocalFiles(options uninstallOptions) (uninstallResult, error) {
	configDir, err := config.Dir()
	if err != nil {
		return uninstallResult{}, fmt.Errorf("uninstall: locate the local configuration directory: %w", err)
	}
	configDir, err = filepath.Abs(configDir)
	if err != nil {
		return uninstallResult{}, fmt.Errorf("uninstall: resolve the local configuration directory: %w", err)
	}
	if err := validateUninstallConfigDir(configDir); err != nil {
		return uninstallResult{}, err
	}
	configured, err := loadUninstallConfig(configDir)
	if err != nil {
		return uninstallResult{}, err
	}
	if configured != nil {
		if err := ensureRemoteDataPreserved(configDir, configured); err != nil {
			return uninstallResult{}, err
		}
	}
	if err := removeInstalledAgentHooks(); err != nil {
		return uninstallResult{}, fmt.Errorf("uninstall: %w", err)
	}

	installDir := options.dir
	if installDir == "" {
		installDir, err = defaultInstallDir()
		if err != nil {
			return uninstallResult{}, fmt.Errorf("uninstall: choose the installation directory: %w", err)
		}
	}
	installDir, err = filepath.Abs(installDir)
	if err != nil {
		return uninstallResult{}, fmt.Errorf("uninstall: resolve the installation directory: %w", err)
	}
	targetPath := filepath.Join(installDir, installedExecutableName())
	result := uninstallResult{targetPath: targetPath, configDir: configDir}
	if info, statErr := os.Lstat(configDir); statErr == nil {
		if !info.IsDir() {
			return result, fmt.Errorf("uninstall: refusing to remove local configuration path that is not a directory: %s", configDir)
		}
		result.configExisted = true
	} else if !os.IsNotExist(statErr) {
		return result, fmt.Errorf("uninstall: inspect local configuration directory: %w", statErr)
	}

	if info, statErr := os.Lstat(targetPath); statErr == nil {
		if info.IsDir() {
			return result, fmt.Errorf("uninstall: refusing to remove directory %s", targetPath)
		}
		result.existed = true
		currentPath, currentErr := os.Executable()
		if currentErr == nil {
			currentPath, currentErr = filepath.Abs(currentPath)
		}
		running := currentErr == nil && (sameInstallPath(currentPath, targetPath) || pathWithin(configDir, currentPath))
		result.removalScheduled, err = removeInstalledExecutable(targetPath, configDir, running)
	} else if !os.IsNotExist(statErr) {
		return result, fmt.Errorf("uninstall: inspect executable: %w", statErr)
	} else {
		result.removalScheduled, err = removeInstalledExecutable(targetPath, configDir, false)
	}
	if err != nil {
		return result, fmt.Errorf("uninstall: remove local files: %w", err)
	}

	result.pathRemoved, err = removeUserPath(installDir)
	if err != nil {
		return result, fmt.Errorf("uninstall: update user PATH: %w", err)
	}
	return result, nil
}

func loadUninstallConfig(configDir string) (*config.Config, error) {
	c, err := config.Load(configDir)
	if errors.Is(err, config.ErrNotInitialised) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("uninstall: cannot verify the configured sync directory: %w", err)
	}
	return c, nil
}

func ensureRemoteDataPreserved(configDir string, c *config.Config) error {
	overlaps, err := configuredRemotePathOverlaps(configDir, c)
	if err != nil {
		return fmt.Errorf("uninstall: cannot verify the configured local sync directory: %w", err)
	}
	if overlaps {
		return fmt.Errorf("uninstall: the configured local sync directory overlaps %s; move the sync directory before uninstalling so it is not deleted", configDir)
	}
	return nil
}

func configuredRemotePathOverlaps(configDir string, c *config.Config) (bool, error) {
	if c == nil || !strings.EqualFold(strings.TrimSpace(c.Remote.Type), "dir") {
		return false, nil
	}
	remotePath := strings.TrimSpace(c.Remote.Path)
	if remotePath == "" {
		return false, nil
	}
	resolvedConfigDir, err := resolvePathForSafety(configDir)
	if err != nil {
		return false, err
	}
	resolvedRemotePath, err := resolvePathForSafety(remotePath)
	if err != nil {
		return false, err
	}
	return pathWithin(resolvedConfigDir, resolvedRemotePath) || pathWithin(resolvedRemotePath, resolvedConfigDir), nil
}

func resolvePathForSafety(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", errors.New("path cannot be resolved safely")
	}
	absolute = filepath.Clean(absolute)

	// EvalSymlinks requires the final path to exist. Resolve the nearest
	// existing ancestor so a configured-but-not-created directory is still
	// checked through any symlink or Windows Junction in its parents.
	missing := make([]string, 0, 4)
	current := absolute
	for {
		_, statErr := os.Lstat(current)
		if statErr == nil {
			resolved, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil {
				return "", errors.New("path cannot be resolved safely")
			}
			resolved, absErr := filepath.Abs(resolved)
			if absErr != nil {
				return "", errors.New("path cannot be resolved safely")
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", errors.New("path cannot be resolved safely")
		}
		parent := filepath.Dir(current)
		if sameInstallPath(parent, current) {
			return "", errors.New("path cannot be resolved safely")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func removeInstalledAgentHooks() error {
	layouts, err := adapter.DefaultLayouts()
	if err != nil {
		return fmt.Errorf("locate Agent hook settings: %w", err)
	}
	for _, layout := range layouts {
		installer, ok := layout.(adapter.HookInstaller)
		if !ok {
			continue
		}
		if err := installer.RemoveHook(); err != nil {
			return fmt.Errorf("remove %s hook: %w", layout.Name(), err)
		}
	}
	return nil
}

func validateUninstallConfigDir(configDir string) error {
	if strings.TrimSpace(configDir) == "" {
		return errors.New("uninstall: local configuration directory is empty")
	}
	root := filepath.VolumeName(configDir) + string(os.PathSeparator)
	if sameInstallPath(configDir, root) {
		return fmt.Errorf("uninstall: refusing to remove filesystem root %s", configDir)
	}
	if home, err := os.UserHomeDir(); err == nil && sameInstallPath(configDir, home) {
		return fmt.Errorf("uninstall: refusing to remove the user home directory %s", configDir)
	}
	return nil
}

func pathWithin(parent, child string) bool {
	parent, parentErr := filepath.Abs(parent)
	child, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	if sameInstallPath(parent, child) {
		return true
	}
	relative, err := filepath.Rel(parent, child)
	if err != nil || filepath.IsAbs(relative) || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
