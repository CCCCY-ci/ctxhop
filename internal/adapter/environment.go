package adapter

import "github.com/CCCCY-ci/ctxhop/internal/environment"

// codexEnvironmentProvider keeps Codex's filtered config rules behind the
// Codex adapter. The Core only sees environment.Provider and does not know
// about TOML, config.toml, or Codex's Skill layout.
type codexEnvironmentProvider struct{}

func (codexEnvironmentProvider) Name() string { return "codex" }

func (codexEnvironmentProvider) Capture(records [][]byte, version, agentHome, projectRoot, projectID string) environment.CaptureResult {
	references := environment.Discover(records, "codex", version)
	components := environment.CaptureSkillComponents("codex", agentHome, projectRoot, projectID, references)
	components = append(components, environment.CaptureMCPComponents("codex", agentHome, projectRoot, projectID, references)...)
	components = append(components, environment.CaptureSessionSettingsWithSources("codex", agentHome, projectRoot, records, projectID)...)
	return environment.CaptureResult{
		References: references,
		Components: environment.NormalizeComponentContents(components),
	}
}

func (codexEnvironmentProvider) Inspect(component environment.Component, agentHome, projectRoot string) environment.LocalComponentState {
	return environment.InspectComponent(component, "codex", agentHome, projectRoot)
}

func (codexEnvironmentProvider) Apply(content environment.ComponentContent, agentHome, projectRoot, backupRoot string) (environment.LocalComponentState, error) {
	if content.Component.Kind == "mcp" || content.Component.Kind == "settings" {
		return environment.ApplyConfigComponent(content, "codex", agentHome, projectRoot, backupRoot)
	}
	return environment.ApplyComponent(content, "codex", agentHome, projectRoot, backupRoot)
}

// claudeEnvironmentProvider keeps Claude Code's JSON environment rules behind
// the Claude adapter. Core synchronization only sees the common Provider
// contract and therefore does not branch on Agent names.
type claudeEnvironmentProvider struct{}

func (claudeEnvironmentProvider) Name() string { return "claude-code" }

func (claudeEnvironmentProvider) Capture(records [][]byte, version, agentHome, projectRoot, projectID string) environment.CaptureResult {
	references := environment.DiscoverClaude(records, version)
	components := environment.CaptureClaudeSkillComponents(agentHome, projectRoot, projectID, references)
	components = append(components, environment.CaptureClaudeMCPComponents(agentHome, projectRoot, projectID, references)...)
	components = append(components, environment.CaptureClaudeSessionSettings(agentHome, projectRoot, records, projectID)...)
	return environment.CaptureResult{
		References: references,
		Components: environment.NormalizeComponentContents(components),
	}
}

func (claudeEnvironmentProvider) Inspect(component environment.Component, agentHome, projectRoot string) environment.LocalComponentState {
	return environment.InspectClaudeComponent(component, agentHome, projectRoot)
}

func (claudeEnvironmentProvider) Apply(content environment.ComponentContent, agentHome, projectRoot, backupRoot string) (environment.LocalComponentState, error) {
	return environment.ApplyClaudeComponent(content, agentHome, projectRoot, backupRoot)
}
