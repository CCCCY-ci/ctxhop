package environment

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDiscoverAndCaptureSessionSettingsUseOnlyAllowlistedMetadata(t *testing.T) {
	records := [][]byte{
		[]byte("{\"type\":\"session_meta\",\"payload\":{\"model\":\"gpt-5\",\"model_provider\":\"openai\",\"effort\":\"high\",\"cwd\":\"C:/source/project\",\"approval_policy\":\"never\",\"sandbox_policy\":{\"type\":\"danger-full-access\"},\"base_instructions\":\"do not copy\"}}"),
	}

	references := Discover(records, "codex", "")
	wantReference := Reference{Kind: "settings", Name: codexSessionSettingsName, Portability: "platform-specific"}
	if !reflect.DeepEqual(references, []Reference{wantReference}) {
		t.Fatalf("references = %#v, want %#v", references, []Reference{wantReference})
	}

	components := CaptureSessionSettings("codex", records, "project-one")
	if len(components) != 1 {
		t.Fatalf("components = %#v, want one component", components)
	}
	if components[0].Component.Scope != "global" || components[0].Component.ProjectID != "" {
		t.Fatalf("component = %#v, want global Codex settings", components[0].Component)
	}
	var values map[string]string
	if err := json.Unmarshal(components[0].Content, &values); err != nil {
		t.Fatal(err)
	}
	wantValues := map[string]string{
		"effort":         "high",
		"model":          "gpt-5",
		"model_provider": "openai",
	}
	if !reflect.DeepEqual(values, wantValues) {
		t.Fatalf("settings = %#v, want %#v", values, wantValues)
	}
	if string(components[0].Content) == "" {
		t.Fatal("settings component has empty content")
	}
}

func TestSessionSettingsIgnoreInvalidAndConflictingValues(t *testing.T) {
	records := [][]byte{
		[]byte("{\"type\":\"session_meta\",\"payload\":{\"model\":\"gpt-5\",\"effort\":{\"value\":\"high\"},\"model_provider\":\"openai\"}}"),
		[]byte("{\"type\":\"session_meta\",\"payload\":{\"model\":\"other-model\",\"model_provider\":\"openai\",\"effort\":\"high\\nunsafe\"}}"),
	}
	if got := Discover(records, "codex", ""); len(got) != 1 || got[0].Kind != "settings" {
		t.Fatalf("references = %#v, want only the valid provider setting", got)
	}
	components := CaptureSessionSettings("codex", records, "project-one")
	if len(components) != 1 {
		t.Fatalf("components = %#v, want one component", components)
	}
	var values map[string]string
	if err := json.Unmarshal(components[0].Content, &values); err != nil {
		t.Fatal(err)
	}
	wantValues := map[string]string{"model_provider": "openai"}
	if !reflect.DeepEqual(values, wantValues) {
		t.Fatalf("settings = %#v, want %#v", values, wantValues)
	}
}

func TestCaptureSessionSettingsRequiresCodexAndProject(t *testing.T) {
	records := [][]byte{[]byte("{\"type\":\"session_meta\",\"payload\":{\"model\":\"gpt-5\"}}")}
	if got := CaptureSessionSettings("claude-code", records, "project-one"); len(got) != 0 {
		t.Fatalf("claude components = %#v, want empty", got)
	}
	if got := CaptureSessionSettings("codex", records, ""); len(got) != 0 {
		t.Fatalf("unbound components = %#v, want empty", got)
	}
}
