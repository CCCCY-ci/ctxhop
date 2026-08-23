package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexHookInstallIsIdempotentAndPreservesUserHooks(t *testing.T) {
	home := t.TempDir()
	settings := map[string]any{
		"description": "keep this",
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo start"}}}},
			"SessionEnd":   []any{map[string]any{"matcher": "other", "hooks": []any{map[string]any{"type": "command", "command": "echo user"}}}},
		},
	}
	writeCodexHooksTestFile(t, filepath.Join(home, "hooks.json"), settings)

	layout := CodexLayout{Home: home}
	if err := layout.InstallHook(`C:\Program Files\CtxHop\ctxhop.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	installed, err := layout.HookInstalled()
	if err != nil || !installed {
		t.Fatalf("HookInstalled = %v, %v", installed, err)
	}

	if err := layout.InstallHook(`D:\tools\ctxhop.exe`); err != nil {
		t.Fatalf("reinstall hook: %v", err)
	}
	current := readCodexHooksTestFile(t, layout.HooksPath())
	groups, err := codexHookGroups(current)
	if err != nil {
		t.Fatal(err)
	}
	items := codexHookItems(groups)
	var ours int
	for _, item := range items {
		if codexIsOurs(item) {
			ours++
			if !strings.Contains(item["command"].(string), hookMarker) || !strings.Contains(item["commandWindows"].(string), "D:\\tools\\ctxhop.exe") {
				t.Fatalf("updated hook = %+v", item)
			}
		}
	}
	if ours != 1 || len(items) != 2 {
		t.Fatalf("hook items = %+v, ours = %d", items, ours)
	}

	if err := layout.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}
	installed, err = layout.HookInstalled()
	if err != nil || installed {
		t.Fatalf("HookInstalled after remove = %v, %v", installed, err)
	}
	remaining := readCodexHooksTestFile(t, layout.HooksPath())
	if remaining["description"] != "keep this" {
		t.Fatalf("top-level settings changed: %+v", remaining)
	}
}
func writeCodexHooksTestFile(t *testing.T, path string, value map[string]any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCodexHooksTestFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
