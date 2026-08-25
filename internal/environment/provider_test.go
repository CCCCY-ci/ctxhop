package environment

import "testing"

func TestCaptureResultWithoutConfigKeepsSessionDependenciesAndSkills(t *testing.T) {
	skill, err := NewComponentContent("skill", "demo-skill", "global", "", "portable", "text/markdown", []byte("# Demo skill\n"))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewComponentContent("settings", codexSessionSettingsName, "global", "", "platform-specific", "application/json", []byte(`{"model":"gpt-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := NewComponentContent("mcp", "demo-server", "global", "", "platform-specific", "application/json", []byte(`{"command":"node"}`))
	if err != nil {
		t.Fatal(err)
	}
	result := CaptureResult{
		References: []Reference{
			{Kind: "tool-requirement", Name: "codex", Version: "0.149.0", Portability: "platform-specific"},
			{Kind: "skill", Name: "demo-skill", Portability: "portable"},
			{Kind: "settings", Name: codexSessionSettingsName, Portability: "platform-specific"},
			{Kind: "mcp", Name: "demo-server", Portability: "platform-specific"},
		},
		Components: []ComponentContent{skill, settings, mcp},
	}

	filtered := result.WithoutConfig()
	if len(filtered.References) != 2 || filtered.References[0].Kind != "tool-requirement" || filtered.References[1].Kind != "skill" {
		t.Fatalf("filtered references = %#v", filtered.References)
	}
	if len(filtered.Components) != 1 || filtered.Components[0].Component.Kind != "skill" {
		t.Fatalf("filtered components = %#v", filtered.Components)
	}
	if len(result.References) != 4 || len(result.Components) != 3 {
		t.Fatal("WithoutConfig mutated the original capture")
	}
}
