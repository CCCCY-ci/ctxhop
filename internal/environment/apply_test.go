package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAndApplySkillComponentBacksUpChangedFile(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	backup := t.TempDir()
	target := filepath.Join(home, "skills", "demo", "SKILL.md")
	writeComponentFixture(t, target, "# old\n")
	desired, err := NewComponentContent("skill", "demo", "global", "", "portable", "text/markdown", []byte("# new\n"))
	if err != nil {
		t.Fatal(err)
	}

	inspected := InspectComponent(desired.Component, "codex", home, project)
	if inspected.State != ComponentStateChanged || inspected.Path != target {
		t.Fatalf("inspected = %#v", inspected)
	}
	applied, err := ApplyComponent(desired, "codex", home, project, backup)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != ComponentStateApplied || applied.Backup == "" {
		t.Fatalf("applied = %#v", applied)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# new\n" {
		t.Fatalf("target = %q", got)
	}
	original, err := os.ReadFile(applied.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != "# old\n" {
		t.Fatalf("backup = %q", original)
	}
	if after := InspectComponent(desired.Component, "codex", home, project); after.State != ComponentStateUnchanged {
		t.Fatalf("after apply = %#v", after)
	}
}

func TestApplySkillComponentCreatesProjectTargetAndUsesExistingScope(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".codex", "skills", "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	content, err := NewComponentContent("skill", "demo", "project", "project-one", "portable", "text/markdown", []byte("# project\n"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := ApplyComponent(content, "codex", home, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(project, ".codex", "skills", "demo", "SKILL.md")
	if state.State != ComponentStateApplied || state.Path != want {
		t.Fatalf("state = %#v, want path %q", state, want)
	}
}

func TestApplyComponentDoesNotWriteUnsupportedIntent(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	component, err := NewComponentContent("mcp", "demo", "global", "", "platform-specific", "application/json", []byte("{\"command\":\"node\"}"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := ApplyComponent(component, "codex", home, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != ComponentStateManual || !strings.Contains(state.Reason, "Skill") {
		t.Fatalf("state = %#v", state)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("home entries = %#v, want empty", entries)
	}
}

func TestApplySkillComponentBackupsDoNotCollide(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	backup := t.TempDir()
	backups := make(map[string]struct{})
	for _, name := range []string{"alpha", "beta"} {
		target := filepath.Join(home, "skills", name, "SKILL.md")
		writeComponentFixture(t, target, "# old\n")
		content, err := NewComponentContent("skill", name, "global", "", "portable", "text/markdown", []byte("# new\n"))
		if err != nil {
			t.Fatal(err)
		}
		state, err := ApplyComponent(content, "codex", home, project, backup)
		if err != nil {
			t.Fatal(err)
		}
		if state.Backup == "" {
			t.Fatalf("state = %#v", state)
		}
		if _, exists := backups[state.Backup]; exists {
			t.Fatalf("backup collision at %q", state.Backup)
		}
		backups[state.Backup] = struct{}{}
	}
	entries, err := os.ReadDir(backup)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("backup entries = %d, want 2", len(entries))
	}
}

func TestComponentPathRejectsSymlinkEscape(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "skills", "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "skills", "demo"), filepath.Join(home, "skills", "demo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	component, err := NewComponentContent("skill", "demo", "global", "", "portable", "text/markdown", []byte("# safe\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ComponentPath(component.Component, "codex", home, project); err == nil {
		t.Fatal("symlink escape unexpectedly accepted")
	}
}
