package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// hookMarker identifies the hook entry belonging to this tool.
//
// Matching on a flag we always pass, rather than on the executable's path or
// name, means the entry is still recognised after the binary moves or is
// renamed - and it never mistakes a hook the user wrote for one of ours. The
// flag is also meaningful: it tells `push` it was invoked automatically, so it
// can stay quiet.
const hookMarker = "--agentsync-hook"

// hookEvent is the agent's session lifecycle event we attach to. Confirmed
// against the agent's own list of recognised events.
const hookEvent = "SessionEnd"

// hookCommand builds the shell command line the agent will run.
//
// The path is wrapped in double quotes so a directory containing a space does
// not split into two arguments. It must be shell quoting, not Go quoting: %q
// would escape the backslashes in a Windows path, and `C:\\bin\\x.exe` is not
// a path any shell resolves.
//
// On Windows the agent runs hooks through PowerShell, where a quoted path at
// the start of a statement is only a string expression - the arguments after it
// are a parse error. The call operator is required to execute it. Verified by
// installing a hook and watching a real session end report
// "UnexpectedToken: push".
//
// An executable path containing a double quote is refused rather than escaped,
// because the escaping rules differ between shells and a command line we cannot
// be sure of is worse than no hook at all.
func hookCommand(executable string) (string, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", errors.New("adapter: no executable path for the hook")
	}
	if strings.ContainsAny(executable, "\"\r\n") {
		return "", fmt.Errorf("adapter: executable path cannot be quoted safely: %q", executable)
	}

	command := `"` + executable + `" push ` + hookMarker
	if runtime.GOOS == "windows" {
		command = "& " + command
	}
	return command, nil
}

// SettingsPath is the user-level settings file the hook is registered in.
//
// User level rather than project level: whether a given project syncs is our
// configuration to decide, not something to encode by scattering hooks through
// the user's projects.
func (l Layout) SettingsPath() string {
	return filepath.Join(l.Home, "settings.json")
}

// InstallHook registers a SessionEnd hook that runs `agentsync push`.
//
// Idempotent: a second call updates the command if the executable moved and
// otherwise changes nothing. Hooks belonging to anyone else are preserved
// untouched - this is the user's file, and we are a guest in it (spec §4.9).
func (l Layout) InstallHook(executable string) error {
	command, err := hookCommand(executable)
	if err != nil {
		return err
	}

	settings, err := l.loadSettings()
	if err != nil {
		return err
	}

	groups := hookGroups(settings)

	if replaceOurCommand(groups, command) {
		return l.saveSettings(settings)
	}

	groups = append(groups, map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	})
	setHookGroups(settings, groups)
	return l.saveSettings(settings)
}

// RemoveHook deletes only the entry this tool installed, leaving the rest of
// the user's configuration - and any empty containers we created - as it found
// them. Removing the hook must leave the agent exactly as it was (BR-13).
func (l Layout) RemoveHook() error {
	settings, err := l.loadSettings()
	if err != nil {
		return err
	}

	groups, changed := withoutOurCommand(hookGroups(settings))
	if !changed {
		return nil
	}
	setHookGroups(settings, groups)
	pruneEmpty(settings)
	return l.saveSettings(settings)
}

// HookInstalled reports whether our entry is currently registered.
func (l Layout) HookInstalled() (bool, error) {
	settings, err := l.loadSettings()
	if err != nil {
		return false, err
	}
	for _, item := range hookItems(hookGroups(settings)) {
		if isOurs(item) {
			return true, nil
		}
	}
	return false, nil
}

// loadSettings reads the settings file, treating a missing file as empty.
//
// The whole document is kept as a generic map so that keys we know nothing
// about survive the round trip. Dropping a setting we failed to model would be
// a silent change to the user's agent.
func (l Layout) loadSettings() (map[string]any, error) {
	data, err := os.ReadFile(l.SettingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent settings: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		// Refuse rather than overwrite: the file is the user's, and replacing
		// something we cannot parse would destroy configuration we never saw.
		return nil, fmt.Errorf("agent settings are not valid JSON; fix or remove them before retrying: %w", err)
	}
	return settings, nil
}

// saveSettings writes the settings back atomically. An interrupted write must
// never leave the user without a settings file.
func (l Layout) saveSettings(settings map[string]any) error {
	// HTML escaping off: the command line contains `&` on Windows, and a
	// settings file full of & is something the user has to read and edit.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return fmt.Errorf("encode agent settings: %w", err)
	}
	data := buf.Bytes()

	if err := os.MkdirAll(l.Home, 0o755); err != nil {
		return fmt.Errorf("create agent directory: %w", err)
	}
	return writeFileAtomic(l.SettingsPath(), func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// hookGroups returns the entries registered for our event, or nil.
func hookGroups(settings map[string]any) []any {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	groups, _ := hooks[hookEvent].([]any)
	return groups
}

func setHookGroups(settings map[string]any, groups []any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	hooks[hookEvent] = groups
}

// hookItems flattens the command entries inside every group.
func hookItems(groups []any) []map[string]any {
	var items []map[string]any
	for _, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		inner, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, rawItem := range inner {
			if item, ok := rawItem.(map[string]any); ok {
				items = append(items, item)
			}
		}
	}
	return items
}

func isOurs(item map[string]any) bool {
	command, _ := item["command"].(string)
	return strings.Contains(command, hookMarker)
}

// replaceOurCommand updates an existing entry in place, reporting whether one
// was found. This is what makes reinstalling after a move a no-op rather than a
// duplicate.
func replaceOurCommand(groups []any, command string) bool {
	found := false
	for _, item := range hookItems(groups) {
		if isOurs(item) {
			item["command"] = command
			item["type"] = "command"
			found = true
		}
	}
	return found
}

// withoutOurCommand strips our entries, dropping groups left empty.
func withoutOurCommand(groups []any) ([]any, bool) {
	changed := false
	kept := make([]any, 0, len(groups))

	for _, raw := range groups {
		group, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		inner, ok := group["hooks"].([]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}

		keptInner := make([]any, 0, len(inner))
		for _, rawItem := range inner {
			item, ok := rawItem.(map[string]any)
			if ok && isOurs(item) {
				changed = true
				continue
			}
			keptInner = append(keptInner, rawItem)
		}

		// A group that only ever held our hook goes with it; one the user also
		// uses stays.
		if len(keptInner) == 0 {
			continue
		}
		group["hooks"] = keptInner
		kept = append(kept, group)
	}
	return kept, changed
}

// pruneEmpty removes containers our removal emptied, so uninstalling leaves no
// trace of us in the file.
func pruneEmpty(settings map[string]any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return
	}
	if groups, ok := hooks[hookEvent].([]any); ok && len(groups) == 0 {
		delete(hooks, hookEvent)
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
}
