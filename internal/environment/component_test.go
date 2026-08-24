package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureSkillComponentsScopesAndDeduplicates(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	globalOne := filepath.Join(home, "skills", "one", "SKILL.md")
	projectOne := filepath.Join(project, ".agents", "skills", "one", "SKILL.md")
	projectTwo := filepath.Join(project, ".codex", "skills", "two", "SKILL.md")
	contentOne := "# one\n\nUse the shared workflow.\n"
	contentTwo := "# two\n\nUse the project workflow.\n"
	writeComponentFixture(t, globalOne, contentOne)
	writeComponentFixture(t, projectOne, contentOne)
	writeComponentFixture(t, projectTwo, contentTwo)

	references := []Reference{
		{Kind: "skill", Name: "one", Portability: "portable"},
		{Kind: "skill", Name: "two", Portability: "portable"},
	}
	components := CaptureSkillComponents("codex", home, project, "project-one", references)
	if len(components) != 2 {
		t.Fatalf("components = %#v, want one global and one project component", components)
	}
	if components[0].Component.Name != "one" || components[0].Component.Scope != "global" {
		t.Fatalf("first component = %#v", components[0].Component)
	}
	if components[1].Component.Name != "two" || components[1].Component.Scope != "project" || components[1].Component.ProjectID != "project-one" {
		t.Fatalf("second component = %#v", components[1].Component)
	}
	if string(components[1].Content) != contentTwo || components[1].Component.Size != len(contentTwo) {
		t.Fatalf("project component content = %q", components[1].Content)
	}
}

func TestCaptureSkillComponentsFailsClosedForSensitiveAndOversizeFiles(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	writeComponentFixture(t, filepath.Join(home, "skills", "secret", "SKILL.md"), "TOKEN=do-not-upload\n")
	writeComponentFixture(t, filepath.Join(home, "skills", "large", "SKILL.md"), strings.Repeat("x", MaxComponentContentBytes+1))
	references := []Reference{
		{Kind: "skill", Name: "secret", Portability: "portable"},
		{Kind: "skill", Name: "large", Portability: "portable"},
	}
	if components := CaptureSkillComponents("codex", home, project, "project-one", references); len(components) != 0 {
		t.Fatalf("components = %#v, want no unsafe component", components)
	}
}

func TestNewComponentContentNormalizesAndRejectsTampering(t *testing.T) {
	component, err := NewComponentContent("skill", "one", "global", "", "portable", "text/markdown", []byte("# one\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(component.Content) != "# one\n" || component.Component.Size != len(component.Content) {
		t.Fatalf("component = %#v", component)
	}
	component.Content = append(component.Content, '!')
	if err := component.Validate(); err == nil {
		t.Fatal("tampered component unexpectedly validated")
	}
	if _, err := NewComponentContent("skill", "one", "global", "", "portable", "text/markdown", []byte("-----BEGIN PRIVATE KEY-----\n")); err == nil {
		t.Fatal("private key content unexpectedly accepted")
	}
}

func TestCaptureSkillComponentsIgnoresInvalidNamesAndOtherAgents(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	references := []Reference{{Kind: "skill", Name: "../outside", Portability: "portable"}}
	if components := CaptureSkillComponents("codex", home, project, "project-one", references); len(components) != 0 {
		t.Fatalf("invalid-name components = %#v", components)
	}
	if components := CaptureSkillComponents("claude-code", home, project, "project-one", nil); len(components) != 0 {
		t.Fatalf("other-agent components = %#v", components)
	}
}

func writeComponentFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
