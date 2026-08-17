package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

const legacyAppDir = "agentsync"

// migrateLegacyIfNeeded copies a previous AppData configuration into the new
// home-directory location. It only runs when the new config does not exist and
// only for the implicit default; an explicit AGENTSYNC_CONFIG_DIR is never
// changed behind the caller's back. The old directory is retained as a
// recoverable backup.
func migrateLegacyIfNeeded(dir string) error {
	if os.Getenv(dirEnv) != "" {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate the user home directory for migration: %w", err)
	}
	current, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve the current configuration directory: %w", err)
	}
	expected, err := filepath.Abs(filepath.Join(home, appDir))
	if err != nil {
		return fmt.Errorf("resolve the new configuration directory: %w", err)
	}
	if filepath.Clean(current) != filepath.Clean(expected) {
		return nil
	}

	if _, err := os.Stat(filepath.Join(current, configFile)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect the new configuration directory: %w", pathSafe(err))
	}

	configRoot, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate the legacy configuration directory: %w", err)
	}
	legacy := filepath.Join(configRoot, legacyAppDir)
	if _, err := os.Stat(filepath.Join(legacy, configFile)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect the legacy configuration directory: %w", pathSafe(err))
	}

	if err := os.MkdirAll(current, 0o700); err != nil {
		return fmt.Errorf("create the new configuration directory: %w", pathSafe(err))
	}
	if err := copyLegacyFiles(legacy, current); err != nil {
		return fmt.Errorf("migrate the legacy configuration: %w", err)
	}
	if err := copyLegacyDirectory(filepath.Join(legacy, "state"), filepath.Join(current, "state")); err != nil {
		return fmt.Errorf("migrate the legacy sync state: %w", err)
	}
	return nil
}

func copyLegacyDirectory(source, target string) error {
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", source, pathSafe(err))
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy state path %s is not a directory", source)
	}
	if info, err := os.Stat(target); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("target state path %s is not a directory", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", target, pathSafe(err))
	} else if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", target, pathSafe(err))
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read %s: %w", source, pathSafe(err))
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to migrate symbolic link %s", filepath.Join(source, entry.Name()))
		}
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyLegacyDirectory(sourcePath, targetPath); err != nil {
				return err
			}
			continue
		}
		data, err := os.ReadFile(sourcePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", sourcePath, pathSafe(err))
		}
		if _, err := os.Stat(targetPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", targetPath, pathSafe(err))
		}
		if err := atomicfile.WriteBytes(targetPath, data); err != nil {
			return fmt.Errorf("write %s: %w", targetPath, err)
		}
	}
	return nil
}

func copyLegacyFiles(source, target string) error {
	for _, name := range []string{configFile, secretsFile, deviceKeyFile} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", name, pathSafe(err))
		}

		destination := filepath.Join(target, name)
		if _, err := os.Stat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect %s: %w", name, pathSafe(err))
		}
		if err := atomicfile.WriteBytes(destination, data); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
