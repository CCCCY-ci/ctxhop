package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureClaudeMCPComponentsNeverCopiesAuthenticationOrTransportSecrets(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	writeClaudeFixture(t, filepath.Join(home, ".claude.json"), "{\"oauthAccount\":{\"accessToken\":\"oauth-secret\",\"refreshToken\":\"refresh-secret\"},\"mcpServers\":{\"demo\":{\"type\":\"stdio\",\"command\":\"node\",\"args\":[\"server.js\"],\"env\":{\"MCP_TOKEN\":\"mcp-secret\"},\"headers\":{\"Authorization\":\"Bearer header-secret\"}}}}")

	components := CaptureClaudeMCPComponents(home, project, "project-one", []Reference{
		{Kind: "mcp", Name: "demo", Portability: "platform-specific"},
	})
	if len(components) != 1 {
		t.Fatalf("components = %#v, want one filtered component", components)
	}
	var payload map[string]any
	if err := json.Unmarshal(components[0].Content, &payload); err != nil {
		t.Fatalf("component payload is not JSON: %v", err)
	}
	if payload["command"] != "node" || payload["type"] != "stdio" {
		t.Fatalf("payload = %#v", payload)
	}
	text := string(components[0].Content)
	for _, secret := range []string{"oauthAccount", "accessToken", "refreshToken", "oauth-secret", "refresh-secret", "env", "headers", "mcp-secret", "header-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("Claude authentication or transport secret %q leaked into component: %s", secret, text)
		}
	}
}

func TestClaudeAuthenticationFilesAreNotConfigurationSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	writeClaudeFixture(t, filepath.Join(home, ".credentials.json"), "{\"claudeAiOauth\":{\"accessToken\":\"never-read\"}}")
	if _, found, safe := readClaudeJSONFile(filepath.Join(home, ".credentials.json")); !found || safe {
		t.Fatalf("credentials file read result = found:%t safe:%t, want found but unsafe", found, safe)
	}
	components := CaptureClaudeMCPComponents(home, t.TempDir(), "project-one", []Reference{
		{Kind: "mcp", Name: "demo", Portability: "platform-specific"},
	})
	if len(components) != 0 {
		t.Fatalf("credentials-only Claude home produced components: %#v", components)
	}
}

func TestCaptureClaudeSessionSettingsIgnoresAuthenticationFields(t *testing.T) {
	records := [][]byte{[]byte("{\"message\":{\"model\":\"claude-sonnet-4\",\"accessToken\":\"oauth-secret\"},\"oauthAccount\":{\"refreshToken\":\"refresh-secret\"}}")}
	components := CaptureClaudeSessionSettings(t.TempDir(), t.TempDir(), records, "project-one")
	if len(components) != 1 {
		t.Fatalf("components = %#v, want one model component", components)
	}
	if string(components[0].Content) != "{\"model\":\"claude-sonnet-4\"}" {
		t.Fatalf("settings payload = %s", components[0].Content)
	}
}

func TestNewComponentContentRejectsNestedAuthenticationFields(t *testing.T) {
	for _, content := range []string{
		"{\"oauthAccount\":{\"accessToken\":\"secret\"}}",
		"{\"credentials\":{\"apiKey\":\"secret\"}}",
		"{\"auth\":{\"refreshToken\":\"secret\"}}",
	} {
		if _, err := NewComponentContent("settings", claudeSessionSettingsName, "global", "", "platform-specific", "application/json", []byte(content)); err == nil {
			t.Fatalf("authentication payload was accepted: %s", content)
		}
	}
}

func writeClaudeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
