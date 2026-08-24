package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureMCPComponentsReadsOnlyObservedSafeIntent(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	config := strings.Join([]string{
		"[mcp_servers.demo]",
		"command = \"node\"",
		"args = [\"server.js\", \"--mode\", \"stdio\"]",
		"startup_timeout_sec = 15",
		"",
		"[mcp_servers.demo.env]",
		"TOKEN = \"do-not-upload\"",
		"",
		"[mcp_servers.unobserved]",
		"command = \"python\"",
	}, "\n")
	writeMCPFixture(t, filepath.Join(home, "config.toml"), config)
	references := []Reference{{Kind: "mcp", Name: "demo", Portability: "platform-specific"}}
	components := CaptureMCPComponents("codex", home, project, "project-one", references)
	if len(components) != 1 {
		t.Fatalf("components = %#v, want one observed MCP component", components)
	}
	component := components[0]
	if component.Component.Kind != "mcp" || component.Component.Scope != "global" || component.Component.Format != "application/json" {
		t.Fatalf("component = %#v", component.Component)
	}
	var payload map[string]any
	if err := json.Unmarshal(component.Content, &payload); err != nil {
		t.Fatalf("component content is not JSON: %v", err)
	}
	if payload["command"] != "node" || payload["startupTimeoutSec"] != float64(15) {
		t.Fatalf("payload = %#v", payload)
	}
	if strings.Contains(string(component.Content), "TOKEN") || strings.Contains(string(component.Content), "do-not-upload") {
		t.Fatalf("MCP environment value leaked into component: %s", component.Content)
	}
}

func TestCaptureMCPComponentsDeduplicatesIdenticalGlobalAndProjectConfig(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	config := "[mcp_servers.demo]\ncommand = \"node\"\nargs = [\"server.js\"]\n"
	writeMCPFixture(t, filepath.Join(home, "config.toml"), config)
	writeMCPFixture(t, filepath.Join(project, ".codex", "config.toml"), config)
	references := []Reference{{Kind: "mcp", Name: "demo", Portability: "platform-specific"}}
	components := CaptureMCPComponents("codex", home, project, "project-one", references)
	if len(components) != 1 || components[0].Component.Scope != "global" {
		t.Fatalf("components = %#v, want one global component", components)
	}
}

func TestCaptureMCPComponentsFailsClosedForSensitiveArgumentsAndPaths(t *testing.T) {
	for _, argument := range []string{"--token=secret", `C:\Users\alice\server.js`} {
		home := t.TempDir()
		config := strings.Join([]string{
			"[mcp_servers.demo]",
			"command = \"node\"",
			"args = [\"" + argument + "\"]",
		}, "\n")
		writeMCPFixture(t, filepath.Join(home, "config.toml"), config)
		references := []Reference{{Kind: "mcp", Name: "demo", Portability: "platform-specific"}}
		if components := CaptureMCPComponents("codex", home, t.TempDir(), "project-one", references); len(components) != 0 {
			t.Fatalf("argument %q produced unsafe components: %#v", argument, components)
		}
	}
}

func TestParseMCPConfigIgnoresEnvironmentSections(t *testing.T) {
	config := strings.Join([]string{
		"[mcp_servers.demo]",
		"command = \"node\"",
		"[mcp_servers.demo.env]",
		"SECRET = \"value\"",
	}, "\n")
	servers := parseMCPConfig([]byte(config))
	if len(servers) != 1 || servers["demo"].Command != "node" {
		t.Fatalf("servers = %#v", servers)
	}
}

func writeMCPFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
