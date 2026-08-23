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

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

const (
	codexHooksFileName = "hooks.json"
	codexHookEvent     = "SessionEnd"
	codexHookMatcher   = "other"
)

var _ HookInstaller = Layout{}
var _ HookInstaller = CodexLayout{}

// HooksPath is the user-level Codex hook configuration file. Project-local
// hooks are intentionally not used: CtxHop's project selection is stored in
// its own configuration and should not require adding files to every project.
func (l CodexLayout) HooksPath() string {
	return filepath.Join(l.Home, codexHooksFileName)
}

// InstallHook registers a Codex SessionEnd hook. The command starts an
// independent ctxhop push process because Codex gives SessionEnd handlers a
// short shutdown window; waiting for a remote push here would make the hook
// unreliable for normal S3 latency.
//
// Existing Codex hooks and unrelated top-level settings are preserved. The
// operation is idempotent and updates the generated command if the executable
// moved.
func (l CodexLayout) InstallHook(executable string) error {
	command, commandWindows, err := codexHookCommands(executable)
	if err != nil {
		return err
	}

	settings, err := l.loadCodexHooks()
	if err != nil {
		return err
	}
	groups, err := codexHookGroups(settings)
	if err != nil {
		return err
	}

	switch codexReplaceOurCommand(groups, command, commandWindows) {
	case replacedOurs:
		return l.saveCodexHooks(settings)
	case leftUserCopy:
		return nil
	}

	groups = append(groups, map[string]any{
		"matcher": codexHookMatcher,
		"hooks": []any{
			map[string]any{
				"type":           "command",
				"command":        command,
				"commandWindows": commandWindows,
				"statusMessage":  "starting CtxHop push",
			},
		},
	})
	setCodexHookGroups(settings, groups)
	return l.saveCodexHooks(settings)
}

// probe

// RemoveHook removes only the CtxHop command from Codex hooks.json.
func (l CodexLayout) RemoveHook() error {
	settings, err := l.loadCodexHooks()
	if err != nil {
		return err
	}
	groups, _ := codexHookGroups(settings)
	kept, changed := codexWithoutOurCommand(groups)
	if !changed {
		return nil
	}
	setCodexHookGroups(settings, kept)
	pruneCodexHooks(settings)
	if len(settings) == 0 {
		if err := os.Remove(l.HooksPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove Codex hooks: %w", err)
		}
		return nil
	}
	return l.saveCodexHooks(settings)
}

// HookInstalled reports whether a CtxHop command is registered for Codex.
func (l CodexLayout) HookInstalled() (bool, error) {
	settings, err := l.loadCodexHooks()
	if err != nil {
		return false, err
	}
	groups, err := codexHookGroups(settings)
	if err != nil {
		return false, err
	}
	for _, item := range codexHookItems(groups) {
		if codexIsOurs(item) {
			return true, nil
		}
	}
	return false, nil
}
func codexHookCommands(executable string) (string, string, error) {
	command, err := hookCommand(executable)
	if err != nil {
		return "", "", err
	}
	if runtime.GOOS != "windows" {
		// Codex executes the command through the platform shell. The background
		// process must not keep the hook's stdout/stderr pipe open.
		command += " >/dev/null 2>&1 &"
	}
	commandWindows := "Start-Process -FilePath " + powershellSingleQuoted(executable) + " -ArgumentList 'push','--ctxhop-hook' -WindowStyle Hidden"
	return command, commandWindows, nil
}

func powershellSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
func (l CodexLayout) loadCodexHooks() (map[string]any, error) {
	if strings.TrimSpace(l.Home) == "" {
		return nil, errors.New("adapter: no Codex home configured")
	}
	data, err := os.ReadFile(l.HooksPath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Codex hooks: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("Codex hooks are not valid JSON; fix or remove them before retrying: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%w: Codex hooks must be a JSON object", ErrUnexpectedSettings)
	}
	return settings, nil
}
func (l CodexLayout) saveCodexHooks(settings map[string]any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		return fmt.Errorf("encode Codex hooks: %w", err)
	}
	if err := os.MkdirAll(l.Home, 0o755); err != nil {
		return fmt.Errorf("create Codex directory: %w", err)
	}
	return atomicfile.Write(l.HooksPath(), func(w io.Writer) error {
		_, err := w.Write(buf.Bytes())
		return err
	})
}
func codexHookGroups(settings map[string]any) ([]any, error) {
	raw, ok := settings["hooks"]
	if !ok || raw == nil {
		return nil, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: Codex hooks must be an object", ErrUnexpectedSettings)
	}
	rawGroups, ok := hooks[codexHookEvent]
	if !ok || rawGroups == nil {
		return nil, nil
	}
	groups, ok := rawGroups.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: Codex %s hooks must be a list", ErrUnexpectedSettings, codexHookEvent)
	}
	return groups, nil
}

func setCodexHookGroups(settings map[string]any, groups []any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	hooks[codexHookEvent] = groups
}
func codexHookItems(groups []any) []map[string]any {
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
func codexIsOurs(item map[string]any) bool {
	command, _ := item["command"].(string)
	commandWindows, _ := item["commandWindows"].(string)
	return strings.Contains(command, hookMarker) || strings.Contains(commandWindows, hookMarker)
}
func codexLooksGenerated(item map[string]any) bool {
	command, _ := item["command"].(string)
	commandWindows, _ := item["commandWindows"].(string)
	posix := strings.HasSuffix(command, " push "+hookMarker) || strings.Contains(command, ">/dev/null 2>&1 &")
	windows := strings.HasPrefix(commandWindows, "Start-Process -FilePath ") && strings.Contains(commandWindows, "push") && strings.Contains(commandWindows, hookMarker)
	return posix && windows
}
func codexReplaceOurCommand(groups []any, command, commandWindows string) installOutcome {
	outcome := notFound
	for _, item := range codexHookItems(groups) {
		if !codexIsOurs(item) {
			continue
		}
		if !codexLooksGenerated(item) {
			if outcome == notFound {
				outcome = leftUserCopy
			}
			continue
		}
		item["type"] = "command"
		item["command"] = command
		item["commandWindows"] = commandWindows
		outcome = replacedOurs
	}
	return outcome
}
func codexWithoutOurCommand(groups []any) ([]any, bool) {
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
			if ok && codexIsOurs(item) {
				changed = true
				continue
			}
			keptInner = append(keptInner, rawItem)
		}
		if len(keptInner) == 0 {
			continue
		}
		group["hooks"] = keptInner
		kept = append(kept, group)
	}
	return kept, changed
}
func pruneCodexHooks(settings map[string]any) {
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return
	}
	if groups, ok := hooks[codexHookEvent].([]any); ok && len(groups) == 0 {
		delete(hooks, codexHookEvent)
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
}
