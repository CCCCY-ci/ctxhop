package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
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
	// The path lands inside a double-quoted string, and both PowerShell and sh
	// interpolate `$` and backtick there. Both are legal in a path, and the
	// result of getting it wrong is a hook that silently invokes the wrong
	// executable - so automatic backups stop with no visible symptom.
	if strings.ContainsAny(executable, "\"$`\r\n") {
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

	// Refuse if the file holds something valid but shaped differently from what
	// we model. Writing our entry there would overwrite a value we never
	// understood, and settings we cannot account for are exactly the case where
	// stopping beats guessing (BR-12).
	groups, err := hookGroups(settings)
	if err != nil {
		return err
	}

	switch replaceOurCommand(groups, command) {
	case replacedOurs:
		return l.saveSettings(settings)
	case leftUserCopy:
		// A command carrying our marker that we did not write - the user
		// wrapped it in a launcher or a logger. It already runs us, so the
		// hook is installed; rewriting it would throw their change away.
		return nil
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

	// Unlike installing, an unrecognised shape is no obstacle here: there is
	// nothing of ours inside something we do not model, so leaving it alone is
	// the correct outcome rather than a refusal.
	groups, _ := hookGroups(settings)

	kept, changed := withoutOurCommand(groups)
	if !changed {
		return nil
	}
	setHookGroups(settings, kept)
	pruneEmpty(settings)

	// A document emptied by our own removal means the file exists only because
	// we created it, and leaving an empty one behind is still a trace (BR-13).
	if len(settings) == 0 {
		if err := os.Remove(l.SettingsPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove agent settings: %w", err)
		}
		return nil
	}
	return l.saveSettings(settings)
}

// HookInstalled reports whether our entry is currently registered.
func (l Layout) HookInstalled() (bool, error) {
	settings, err := l.loadSettings()
	if err != nil {
		return false, err
	}
	groups, _ := hookGroups(settings)
	for _, item := range hookItems(groups) {
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
//
// Known limitation: this is a read-modify-write with no lock, because the file
// format offers nowhere to hold one and the agent does not coordinate. If the
// agent or an editor writes settings.json between our read and our rename, that
// change is lost - cleanly, since the rename is atomic, but lost. The window is
// milliseconds and the operation is user-initiated, so it is accepted rather
// than defended against with a lock protocol nobody else honours.
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

	// Publishing through a temporary file would otherwise replace the file's
	// mode with the 0600 of a freshly created temp file. Changing how the
	// user's settings are readable is a permanent trace of our having been
	// there, which uninstalling could not undo (BR-13).
	var mode os.FileMode
	if info, err := os.Stat(l.SettingsPath()); err == nil {
		mode = info.Mode().Perm()
	}

	return atomicfile.Write(l.SettingsPath(), func(w io.Writer) error {
		if mode != 0 {
			if f, ok := w.(*os.File); ok {
				if err := f.Chmod(mode); err != nil {
					return fmt.Errorf("preserve settings file mode: %w", err)
				}
			}
		}
		_, err := w.Write(data)
		return err
	})
}

// ErrUnexpectedSettings reports settings that parse as JSON but are shaped
// differently from what this adapter models.
var ErrUnexpectedSettings = errors.New("adapter: agent settings have an unexpected shape")

// hookGroups returns the entries registered for our event.
//
// Absent containers are normal and yield nil. A container that is present but
// not the shape we expect is an error: writing our entry over it would discard
// a value we never understood, and the file belongs to the user.
func hookGroups(settings map[string]any) ([]any, error) {
	raw, ok := settings["hooks"]
	if !ok || raw == nil {
		return nil, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: \"hooks\" is not an object", ErrUnexpectedSettings)
	}

	rawGroups, ok := hooks[hookEvent]
	if !ok || rawGroups == nil {
		return nil, nil
	}
	groups, ok := rawGroups.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a list", ErrUnexpectedSettings, hookEvent)
	}
	return groups, nil
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

// installOutcome describes what InstallHook found already in place.
type installOutcome int

const (
	// notFound means no command carries our marker.
	notFound installOutcome = iota
	// replacedOurs means an entry we generated was updated in place.
	replacedOurs
	// leftUserCopy means a command carries our marker but is not in the form
	// we generate, so the user has customised it and it was left alone.
	leftUserCopy
)

// replaceOurCommand updates an entry we previously generated, which is what
// makes reinstalling after a move an update rather than a duplicate.
//
// A marked command in any other form is somebody's customised wrapper. It still
// invokes us, so the hook counts as installed, but rewriting it would silently
// discard their change.
func replaceOurCommand(groups []any, command string) installOutcome {
	outcome := notFound

	for _, item := range hookItems(groups) {
		if !isOurs(item) {
			continue
		}
		if !looksGenerated(item) {
			if outcome == notFound {
				outcome = leftUserCopy
			}
			continue
		}
		item["command"] = command
		item["type"] = "command"
		outcome = replacedOurs
	}
	return outcome
}

// looksGenerated reports whether a command has the shape this tool writes:
// an optional call operator, a quoted path, then exactly our arguments.
func looksGenerated(item map[string]any) bool {
	command, _ := item["command"].(string)
	command = strings.TrimPrefix(command, "& ")
	if !strings.HasPrefix(command, `"`) {
		return false
	}
	end := strings.Index(command[1:], `"`)
	if end < 0 {
		return false
	}
	return command[end+2:] == " push "+hookMarker
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
