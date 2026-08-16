package adapter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readSettings(t *testing.T, l Layout) map[string]any {
	t.Helper()
	data, err := os.ReadFile(l.SettingsPath())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("settings are not valid JSON: %v\n%s", err, data)
	}
	return out
}

func writeSettings(t *testing.T, l Layout, body string) {
	t.Helper()
	if err := os.MkdirAll(l.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.SettingsPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstallHookCreatesSettings(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	installed, err := l.HookInstalled()
	if err != nil || !installed {
		t.Fatalf("HookInstalled = %v, %v", installed, err)
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	item := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if item["type"] != "command" {
		t.Errorf("type = %v, want command", item["type"])
	}
	command := item["command"].(string)
	if !strings.Contains(command, hookMarker) || !strings.Contains(command, "push") {
		t.Errorf("command = %q", command)
	}
	// The path is quoted so a directory with a space does not split it.
	if !strings.Contains(command, `"C:\bin\agentsync.exe"`) {
		t.Errorf("executable not quoted: %q", command)
	}
}

func TestInstallHookIsIdempotent(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}

	for i := 0; i < 3; i++ {
		if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
			t.Fatalf("InstallHook: %v", err)
		}
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 1 {
		t.Errorf("reinstalling duplicated the hook: %d groups", len(groups))
	}
}

func TestInstallHookUpdatesAMovedExecutable(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}

	if err := l.InstallHook(`C:\old\agentsync.exe`); err != nil {
		t.Fatal(err)
	}
	if err := l.InstallHook(`D:\new\agentsync.exe`); err != nil {
		t.Fatal(err)
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	command := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.Contains(command, `D:\new\agentsync.exe`) {
		t.Errorf("command not updated: %q", command)
	}
}

func TestInstallHookPreservesEverythingElse(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}

	// A realistic file: settings we know nothing about, and the user's own
	// hooks - including one on the same event.
	writeSettings(t, l, `{
	  "model": "opus",
	  "someFutureSetting": {"nested": [1, 2, 3]},
	  "hooks": {
	    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "echo pre"}]}],
	    "SessionEnd": [{"hooks": [{"type": "command", "command": "echo mine"}]}]
	  }
	}`)

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	settings := readSettings(t, l)
	if settings["model"] != "opus" {
		t.Errorf("model was lost: %v", settings["model"])
	}
	if settings["someFutureSetting"] == nil {
		t.Error("an unknown setting was dropped")
	}

	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("another event's hooks were lost")
	}
	groups := hooks[hookEvent].([]any)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want the user's plus ours", len(groups))
	}
	if cmd := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; cmd != "echo mine" {
		t.Errorf("the user's hook was modified: %v", cmd)
	}
}

func TestRemoveHookLeavesNoTrace(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{"model":"opus"}`)

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}

	settings := readSettings(t, l)
	if _, ok := settings["hooks"]; ok {
		// Uninstalling must leave the agent exactly as it was found (BR-13),
		// including not leaving an empty container behind.
		t.Errorf("an empty hooks container survived: %v", settings["hooks"])
	}
	if settings["model"] != "opus" {
		t.Error("removal disturbed unrelated settings")
	}

	installed, err := l.HookInstalled()
	if err != nil || installed {
		t.Errorf("HookInstalled = %v, %v", installed, err)
	}
}

func TestRemoveHookKeepsTheUsersHooks(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{
	  "hooks": {
	    "SessionEnd": [{"hooks": [
	      {"type": "command", "command": "echo mine"},
	      {"type": "command", "command": "\"C:\\bin\\agentsync.exe\" push --agentsync-hook"}
	    ]}]
	  }
	}`)

	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}

	settings := readSettings(t, l)
	inner := settings["hooks"].(map[string]any)[hookEvent].([]any)[0].(map[string]any)["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("got %d hooks, want the user's one", len(inner))
	}
	if cmd := inner[0].(map[string]any)["command"]; cmd != "echo mine" {
		t.Errorf("removed the wrong hook, left %v", cmd)
	}
}

func TestRemoveHookWhenAbsentIsANoOp(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{"model":"opus"}`)

	before, err := os.ReadFile(l.SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}
	after, err := os.ReadFile(l.SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	// Not even a reformat: there was nothing of ours to remove.
	if string(before) != string(after) {
		t.Errorf("the file was rewritten needlessly:\n%s\n%s", before, after)
	}
}

func TestHookRefusesToTouchUnparseableSettings(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	const broken = `{"model": "opus",`
	writeSettings(t, l, broken)

	// Overwriting a file we cannot parse would destroy configuration we never
	// saw, so every entry point refuses instead.
	if err := l.InstallHook(`C:\bin\agentsync.exe`); err == nil {
		t.Error("InstallHook overwrote unparseable settings")
	}
	if err := l.RemoveHook(); err == nil {
		t.Error("RemoveHook overwrote unparseable settings")
	}
	if _, err := l.HookInstalled(); err == nil {
		t.Error("HookInstalled ignored unparseable settings")
	}

	data, err := os.ReadFile(l.SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != broken {
		t.Errorf("the file was modified: %s", data)
	}
}

func TestInstallHookRejectsUnquotableExecutables(t *testing.T) {
	// Escaping rules differ between cmd and POSIX shells, so a path we cannot
	// quote with confidence is refused rather than guessed at - a wrong command
	// line would run something other than what we meant on every session end.
	for _, exe := range []string{
		"   ",
		"",
		`C:\bin\has"quote.exe`,
		"C:\\bin\\has\nnewline.exe",
		// Both shells interpolate these inside a double-quoted string, and
		// both are legal in a path. `C:\Users\a$b\x.exe` would silently invoke
		// `C:\Users\a\x.exe`, so backups would stop with no visible symptom.
		`C:\Users\a$b\agentsync.exe`,
		"C:\\bin\\back`tick.exe",
	} {
		l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
		if err := l.InstallHook(exe); err == nil {
			t.Errorf("InstallHook(%q) succeeded, want an error", exe)
		}
		if _, err := os.Stat(l.SettingsPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("InstallHook(%q) wrote settings anyway", exe)
		}
	}
}

func TestHookCommandIsExecutableByTheHostShell(t *testing.T) {
	got, err := hookCommand(`C:\Program Files\agentsync.exe`)
	if err != nil {
		t.Fatalf("hookCommand: %v", err)
	}

	// The path is quoted so a directory with a space stays one argument, and
	// the backslashes are literal - Go-style quoting would emit `C:\\Program`,
	// which no shell resolves.
	if !strings.Contains(got, `"C:\Program Files\agentsync.exe"`) {
		t.Errorf("path not quoted literally: %q", got)
	}
	if !strings.HasSuffix(got, " push "+hookMarker) {
		t.Errorf("arguments missing: %q", got)
	}

	if runtime.GOOS == "windows" {
		// The agent runs hooks through PowerShell, where a leading quoted
		// string is an expression and the arguments after it are a parse
		// error. Verified against a real session end, which reported
		// "UnexpectedToken: push" before the call operator was added.
		if !strings.HasPrefix(got, "& ") {
			t.Errorf("PowerShell needs the call operator: %q", got)
		}
	} else if strings.HasPrefix(got, "& ") {
		t.Errorf("POSIX shells do not want a call operator: %q", got)
	}
}

func TestRemoveHookPreservesShapesItDoesNotUnderstand(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{
	  "hooks": {
	    "SessionEnd": [
	      "a string",
	      {"hooks": "not a list"},
	      {"hooks": [{"type": "command", "command": "\"x\" push --agentsync-hook"}]}
	    ]
	  }
	}`)

	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want the two we do not understand", len(groups))
	}
	if groups[0] != "a string" {
		t.Errorf("an entry we do not model was altered: %v", groups[0])
	}
}

func TestRemoveHookWithNoHooksSection(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{"hooks": "not an object"}`)

	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}
	if got := readSettings(t, l)["hooks"]; got != "not an object" {
		t.Errorf("hooks was altered: %v", got)
	}
}

func TestHookHandlesAnEmptySettingsFile(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, "\n  \n")

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	installed, err := l.HookInstalled()
	if err != nil || !installed {
		t.Errorf("HookInstalled = %v, %v", installed, err)
	}
}

func TestHookToleratesUnexpectedEntriesInAListWeUnderstand(t *testing.T) {
	// The containers are the shape we model; the entries inside are not. Those
	// are safe to walk past and must survive untouched.
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	writeSettings(t, l, `{
	  "hooks": {
	    "SessionEnd": ["a string", {"hooks": "not a list"}, {"noHooksKey": 1}, {"hooks": [42]}]
	  }
	}`)

	installed, err := l.HookInstalled()
	if err != nil {
		t.Fatalf("HookInstalled: %v", err)
	}
	if installed {
		t.Error("found a hook that is not there")
	}

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 5 {
		t.Errorf("got %d groups, want the four odd ones plus ours", len(groups))
	}
}

func TestInstallHookRefusesContainersItDoesNotModel(t *testing.T) {
	// Valid JSON in a shape we do not model is the case where writing our entry
	// would overwrite a value we never understood. Refusing is the same choice
	// already made for unparseable files, for the same reason (BR-12).
	cases := map[string]string{
		"SessionEnd is an object": `{"hooks": {"SessionEnd": {"userThing": "keep me"}}}`,
		"SessionEnd is a string":  `{"hooks": {"SessionEnd": "something"}}`,
		"hooks is not an object":  `{"hooks": "not an object"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
			writeSettings(t, l, body)

			err := l.InstallHook(`C:\bin\agentsync.exe`)
			if !errors.Is(err, ErrUnexpectedSettings) {
				t.Fatalf("got %v, want ErrUnexpectedSettings", err)
			}

			data, readErr := os.ReadFile(l.SettingsPath())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(data) != body {
				t.Errorf("the file was modified:\n%s", data)
			}
		})
	}
}

func TestInstallHookLeavesAUserWrapperAlone(t *testing.T) {
	// A marked command we did not write is somebody's wrapper - a logger, a
	// launcher. It already invokes us, so the hook counts as installed, and
	// rewriting it would silently discard their change.
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	const wrapper = `nohup "C:\bin\agentsync.exe" push --agentsync-hook >> /tmp/log 2>&1`
	writeSettings(t, l, `{"hooks":{"SessionEnd":[{"hooks":[{"type":"command","command":`+
		mustJSON(t, wrapper)+`}]}]}}`)

	installed, err := l.HookInstalled()
	if err != nil || !installed {
		t.Fatalf("HookInstalled = %v, %v", installed, err)
	}
	if err := l.InstallHook(`D:\new\agentsync.exe`); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	settings := readSettings(t, l)
	groups := settings["hooks"].(map[string]any)[hookEvent].([]any)
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want no duplicate", len(groups))
	}
	got := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	if got != wrapper {
		t.Errorf("the wrapper was rewritten:\n got  %v\n want %v", got, wrapper)
	}
}

func TestRemoveHookDeletesASettingsFileWeCreated(t *testing.T) {
	// Installing into a machine with no settings file creates one. Leaving an
	// empty document behind is still a trace of our having been there (BR-13).
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}

	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveHook(); err != nil {
		t.Fatalf("RemoveHook: %v", err)
	}

	if _, err := os.Stat(l.SettingsPath()); !errors.Is(err, os.ErrNotExist) {
		data, _ := os.ReadFile(l.SettingsPath())
		t.Errorf("a settings file survived removal: %s", data)
	}
}

func TestLooksGenerated(t *testing.T) {
	tests := map[string]bool{
		`"C:\bin\agentsync.exe" push --agentsync-hook`:   true,
		`& "C:\bin\agentsync.exe" push --agentsync-hook`: true,
		// Anything else carrying the marker is somebody's own command.
		`nohup "x" push --agentsync-hook`:            false,
		`"C:\bin\x.exe" push --agentsync-hook --now`: false,
		`C:\bin\x.exe push --agentsync-hook`:         false,
		`"unterminated push --agentsync-hook`:        false,
		``:                                           false,
	}
	for command, want := range tests {
		got := looksGenerated(map[string]any{"command": command})
		if got != want {
			t.Errorf("looksGenerated(%q) = %v, want %v", command, got, want)
		}
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSettingsPath(t *testing.T) {
	l := Layout{Home: filepath.Join("home", ".claude")}
	if want := filepath.Join("home", ".claude", "settings.json"); l.SettingsPath() != want {
		t.Errorf("got %q, want %q", l.SettingsPath(), want)
	}
}

func TestSettingsWriteIsAtomic(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	if err := l.InstallHook(`C:\bin\agentsync.exe`); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(l.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestLoadSettingsReportsUnreadableFiles(t *testing.T) {
	// A directory where the settings file belongs is not "no settings".
	l := Layout{Home: filepath.Join(t.TempDir(), ".claude")}
	if err := os.MkdirAll(l.SettingsPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := l.HookInstalled()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory was mistaken for a missing file: %v", err)
	}
}
