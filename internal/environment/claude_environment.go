package environment

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	claudeSessionSettingsFileName = "settings.json"
	claudeProjectSettingsFileName = "settings.json"
	claudeLocalSettingsFileName   = "settings.local.json"
	claudeMCPFileName             = ".mcp.json"
	claudeUserConfigFileName      = ".claude.json"

	maxClaudeJSONBytes = 1 << 20
)

// DiscoverClaude extracts only structured dependency evidence from Claude Code
// records. It intentionally ignores prompts, assistant text, shell commands,
// tool arguments, environment variables, and hook bodies.
func DiscoverClaude(records [][]byte, version string) []Reference {
	refs := Discover(records, "claude-code", version)
	for _, record := range records {
		root, ok := decodeClaudeRecord(record)
		if !ok {
			continue
		}
		if observedVersion := rawString(root["version"]); observedVersion != "" {
			refs = append(refs, Reference{
				Kind:        "tool-requirement",
				Name:        "claude-code",
				Version:     observedVersion,
				Portability: "platform-specific",
			})
		}
		for _, block := range claudeToolUseBlocks(root) {
			name := rawString(block["name"])
			if server := claudeMCPServerFromToolName(name); server != "" {
				refs = append(refs, Reference{
					Kind:        "mcp",
					Name:        server,
					Portability: "platform-specific",
				})
				continue
			}
			if !strings.EqualFold(name, "skill") {
				continue
			}
			input, ok := rawObject(block["input"])
			if !ok {
				continue
			}
			for _, key := range []string{"skill", "skill_name", "skillName"} {
				name := rawString(input[key])
				if name == "" {
					continue
				}
				reference := Reference{Kind: "skill", Name: name, Portability: "portable"}
				if reference.Validate() == nil {
					refs = append(refs, reference)
				}
				break
			}
		}
	}
	if _, ok := observedClaudeSessionSettings(records); ok {
		refs = append(refs, Reference{
			Kind:        "settings",
			Name:        claudeSessionSettingsName,
			Portability: "platform-specific",
		})
	}
	return Normalize(refs)
}

// CaptureClaudeSkillComponents reads only the observed SKILL.md files from
// Claude Code's global and project locations.
func CaptureClaudeSkillComponents(agentHome, projectRoot, projectID string, references []Reference) []ComponentContent {
	var captured []ComponentContent
	for _, reference := range Normalize(references) {
		if reference.Kind != "skill" {
			continue
		}
		globalContent, globalFound := readSkillDocument(agentHome, filepath.Join(agentHome, "skills", reference.Name, "SKILL.md"))
		if globalFound {
			if component, err := NewComponentContent("skill", reference.Name, "global", "", reference.Portability, "text/markdown", globalContent); err == nil {
				captured = append(captured, component)
			}
		}
		projectContent, projectFound := readSkillDocument(projectRoot, filepath.Join(projectRoot, ".claude", "skills", reference.Name, "SKILL.md"))
		if !projectFound {
			continue
		}
		if globalFound && strings.EqualFold(string(globalContent), string(projectContent)) {
			continue
		}
		if component, err := NewComponentContent("skill", reference.Name, "project", projectID, reference.Portability, "text/markdown", projectContent); err == nil {
			captured = append(captured, component)
		}
	}
	return NormalizeComponentContents(captured)
}

type claudeMCPIntent struct {
	Type    string
	Command string
	Args    []string
	URL     string
}

func CaptureClaudeMCPComponents(agentHome, projectRoot, projectID string, references []Reference) []ComponentContent {
	userPath := claudeUserConfigPath(agentHome)
	globalDocument, globalExists, globalSafe := readClaudeJSONFile(userPath)
	globalServers, globalServersFound, globalServersSafe := claudeMCPServers(globalDocument)

	projectDocument, projectExists, projectSafe := readClaudeJSONFile(filepath.Join(projectRoot, claudeMCPFileName))
	projectServers, projectServersFound, projectServersSafe := claudeMCPServers(projectDocument)

	localServers, localServersFound, localServersSafe := claudeProjectMCPServers(globalDocument, projectRoot)
	if !globalExists {
		globalServersSafe = true
		localServersSafe = true
	}
	if !projectExists {
		projectServersSafe = true
	}

	var captured []ComponentContent
	for _, reference := range Normalize(references) {
		if reference.Kind != "mcp" {
			continue
		}
		var globalComponent ComponentContent
		var globalFound bool
		if globalSafe && globalServersSafe && globalServersFound {
			if raw, found := globalServers[reference.Name]; found {
				if intent, ok := parseClaudeMCPIntent(raw); ok {
					if component, err := newClaudeMCPComponent(reference, "global", "", intent); err == nil {
						globalComponent = component
						globalFound = true
					}
				}
			}
		}
		if globalFound {
			captured = append(captured, globalComponent)
		}

		var projectComponent ComponentContent
		var projectFound bool
		var projectCandidates []ComponentContent
		if projectSafe && projectServersSafe && projectServersFound {
			if raw, found := projectServers[reference.Name]; found {
				if intent, ok := parseClaudeMCPIntent(raw); ok {
					if component, err := newClaudeMCPComponent(reference, "project", projectID, intent); err == nil {
						projectCandidates = append(projectCandidates, component)
					}
				}
			}
		}
		if globalSafe && localServersSafe && localServersFound {
			if raw, found := localServers[reference.Name]; found {
				if intent, ok := parseClaudeMCPIntent(raw); ok {
					if component, err := newClaudeMCPComponent(reference, "project", projectID, intent); err == nil {
						projectCandidates = append(projectCandidates, component)
					}
				}
			}
		}
		if len(projectCandidates) == 1 {
			projectComponent = projectCandidates[0]
			projectFound = true
		} else if len(projectCandidates) > 1 &&
			strings.EqualFold(projectCandidates[0].Component.Fingerprint, projectCandidates[1].Component.Fingerprint) {
			projectComponent = projectCandidates[0]
			projectFound = true
		}
		if !projectFound {
			continue
		}
		if globalFound && strings.EqualFold(globalComponent.Component.Fingerprint, projectComponent.Component.Fingerprint) {
			continue
		}
		captured = append(captured, projectComponent)
	}
	return NormalizeComponentContents(captured)
}

func CaptureClaudeSessionSettings(agentHome, projectRoot string, records [][]byte, projectID string) []ComponentContent {
	if strings.TrimSpace(projectID) == "" {
		return nil
	}
	values, ok := observedClaudeSessionSettings(records)
	if !ok {
		return nil
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	scope, componentProjectID := claudeSessionSettingsScope(values, agentHome, projectRoot, projectID)
	component, err := NewComponentContent("settings", claudeSessionSettingsName, scope, componentProjectID, "platform-specific", "application/json", payload)
	if err != nil {
		return nil
	}
	return []ComponentContent{component}
}

func claudeSessionSettingsScope(values map[string]string, agentHome, projectRoot, projectID string) (string, string) {
	if strings.TrimSpace(agentHome) == "" {
		return "project", projectID
	}
	global, globalFound, globalSafe := claudeSettingsValues(filepath.Join(agentHome, claudeSessionSettingsFileName))
	if !globalSafe || !globalFound || len(global) != len(values) || !sameStringMap(global, values) {
		return "project", projectID
	}
	projectValues, projectFound, projectSafe := claudeProjectSettingsValues(projectRoot)
	if !projectSafe {
		return "project", projectID
	}
	if projectFound {
		for key, value := range projectValues {
			if observed, ok := values[key]; ok && observed != value {
				return "project", projectID
			}
		}
	}
	return "global", ""
}

func observedClaudeSessionSettings(records [][]byte) (map[string]string, bool) {
	values := make(map[string]string, 1)
	conflicts := make(map[string]bool)
	for _, record := range records {
		root, ok := decodeClaudeRecord(record)
		if !ok {
			continue
		}
		candidates := make([]json.RawMessage, 0, 2)
		if message, ok := rawObject(root["message"]); ok {
			candidates = append(candidates, message["model"])
		}
		candidates = append(candidates, root["model"])
		for _, raw := range candidates {
			value, valid := safeCodexSessionSetting(raw)
			if !valid {
				continue
			}
			if previous, exists := values["model"]; exists && previous != value {
				conflicts["model"] = true
				continue
			}
			values["model"] = value
		}
	}
	for key := range conflicts {
		delete(values, key)
	}
	return values, len(values) != 0
}

func decodeClaudeRecord(record []byte) (map[string]json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(record, &root) != nil || root == nil {
		return nil, false
	}
	return root, true
}

func claudeToolUseBlocks(root map[string]json.RawMessage) []map[string]json.RawMessage {
	message, ok := rawObject(root["message"])
	if !ok {
		return nil
	}
	rawContent, ok := message["content"]
	if !ok {
		return nil
	}
	var content []json.RawMessage
	if json.Unmarshal(rawContent, &content) != nil {
		return nil
	}
	var blocks []map[string]json.RawMessage
	for _, raw := range content {
		block, ok := rawObject(raw)
		if !ok || rawString(block["type"]) != "tool_use" {
			continue
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func claudeMCPServerFromToolName(name string) string {
	if !strings.HasPrefix(name, "mcp__") {
		return ""
	}
	rest := strings.TrimPrefix(name, "mcp__")
	separator := strings.Index(rest, "__")
	if separator <= 0 {
		return ""
	}
	server := rest[:separator]
	reference := Reference{Kind: "mcp", Name: server, Portability: "platform-specific"}
	if reference.Validate() != nil {
		return ""
	}
	return server
}

func parseClaudeMCPIntent(raw json.RawMessage) (claudeMCPIntent, bool) {
	object, ok := rawObject(raw)
	if !ok {
		return claudeMCPIntent{}, false
	}
	intent := claudeMCPIntent{}
	for key, value := range object {
		switch key {
		case "type":
			intent.Type = strings.TrimSpace(rawString(value))
		case "command":
			command := rawString(value)
			safe, valid := safeMCPCommand(command)
			if !valid {
				return claudeMCPIntent{}, false
			}
			intent.Command = safe
		case "args":
			var args []string
			if json.Unmarshal(value, &args) != nil || len(args) > maxMCPArgs {
				return claudeMCPIntent{}, false
			}
			for _, argument := range args {
				if !safeMCPArgument(argument) {
					return claudeMCPIntent{}, false
				}
			}
			intent.Args = append([]string(nil), args...)
		case "url":
			value := rawString(value)
			safe, valid := safeClaudeMCPURL(value)
			if !valid {
				return claudeMCPIntent{}, false
			}
			intent.URL = safe
		default:
			// env, headers, OAuth values and all future keys are intentionally
			// ignored when capturing but make a component unsafe to apply if
			// they are the only usable transport description.
		}
	}
	switch strings.ToLower(intent.Type) {
	case "", "stdio", "http", "sse", "streamable-http":
	default:
		return claudeMCPIntent{}, false
	}
	if intent.Command != "" && intent.URL != "" {
		return claudeMCPIntent{}, false
	}
	if intent.URL != "" && intent.Type == "" {
		intent.Type = "http"
	}
	if intent.URL == "" && intent.Command == "" {
		return claudeMCPIntent{}, false
	}
	if intent.URL != "" && intent.Type == "stdio" {
		return claudeMCPIntent{}, false
	}
	if intent.Command != "" && intent.Type != "" && intent.Type != "stdio" {
		return claudeMCPIntent{}, false
	}
	return intent, true
}

func newClaudeMCPComponent(reference Reference, scope, projectID string, intent claudeMCPIntent) (ComponentContent, error) {
	wire := make(map[string]any)
	if intent.Type != "" {
		wire["type"] = intent.Type
	}
	if intent.Command != "" {
		wire["command"] = intent.Command
	}
	if len(intent.Args) != 0 {
		wire["args"] = intent.Args
	}
	if intent.URL != "" {
		wire["url"] = intent.URL
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return ComponentContent{}, err
	}
	return NewComponentContent("mcp", reference.Name, scope, projectID, reference.Portability, "application/json", payload)
}

func safeClaudeMCPURL(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\r\n\t") || sensitiveMCPText(value) {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	return parsed.String(), true
}

func readClaudeJSONFile(path string) (map[string]json.RawMessage, bool, bool) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, true
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxClaudeJSONBytes {
		return nil, true, false
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return nil, true, false
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil || document == nil {
		return nil, true, false
	}
	return document, true, true
}

func claudeMCPServers(document map[string]json.RawMessage) (map[string]json.RawMessage, bool, bool) {
	if document == nil {
		return nil, false, true
	}
	raw, found := document["mcpServers"]
	if !found {
		return nil, false, true
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(raw, &servers) != nil || servers == nil {
		return nil, true, false
	}
	return servers, true, true
}

func claudeProjectMCPServers(document map[string]json.RawMessage, projectRoot string) (map[string]json.RawMessage, bool, bool) {
	if document == nil || strings.TrimSpace(projectRoot) == "" {
		return nil, false, true
	}
	raw, found := document["projects"]
	if !found {
		return nil, false, true
	}
	var projects map[string]json.RawMessage
	if json.Unmarshal(raw, &projects) != nil || projects == nil {
		return nil, true, false
	}
	var projectRaw json.RawMessage
	for key, value := range projects {
		if claudeProjectPathEqual(key, projectRoot) {
			projectRaw = value
			break
		}
	}
	if projectRaw == nil {
		return nil, false, true
	}
	project, ok := rawObject(projectRaw)
	if !ok {
		return nil, true, false
	}
	return claudeMCPServers(project)
}

func claudeSettingsValues(path string) (map[string]string, bool, bool) {
	document, found, safe := readClaudeJSONFile(path)
	if !safe {
		return nil, found, false
	}
	if !found {
		return nil, false, true
	}
	values := make(map[string]string, 1)
	if raw, present := document["model"]; present {
		value, valid := safeCodexSessionSetting(raw)
		if !valid {
			return nil, true, false
		}
		values["model"] = value
	}
	return values, len(values) != 0, true
}

func claudeProjectSettingsValues(projectRoot string) (map[string]string, bool, bool) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, false, true
	}
	shared, sharedFound, sharedSafe := claudeSettingsValues(filepath.Join(projectRoot, ".claude", claudeProjectSettingsFileName))
	local, localFound, localSafe := claudeSettingsValues(filepath.Join(projectRoot, ".claude", claudeLocalSettingsFileName))
	if !sharedSafe || !localSafe {
		return nil, sharedFound || localFound, false
	}
	values := make(map[string]string, 1)
	for key, value := range shared {
		values[key] = value
	}
	for key, value := range local {
		values[key] = value
	}
	return values, len(values) != 0, true
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func claudeUserConfigPath(agentHome string) string {
	if strings.TrimSpace(agentHome) == "" {
		return ""
	}
	insideConfigDir := filepath.Join(filepath.Clean(agentHome), claudeUserConfigFileName)
	if strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) != "" {
		return insideConfigDir
	}
	// The default Claude layout keeps ~/.claude.json next to ~/.claude.
	// Prefer an existing in-directory file as well, which makes a directly
	// constructed custom Layout behave safely in tests and embedded callers.
	if _, err := os.Lstat(insideConfigDir); err == nil {
		return insideConfigDir
	}
	return filepath.Join(claudeHomeParent(agentHome), claudeUserConfigFileName)
}

func claudeHomeParent(agentHome string) string {
	if strings.TrimSpace(agentHome) == "" {
		return ""
	}
	return filepath.Dir(filepath.Clean(agentHome))
}

func claudeProjectPathEqual(left, right string) bool {
	left = filepath.Clean(filepath.FromSlash(strings.TrimSpace(left)))
	right = filepath.Clean(filepath.FromSlash(strings.TrimSpace(right)))
	if left == "" || right == "" {
		return false
	}
	if filepath.IsAbs(left) && filepath.IsAbs(right) {
		if strings.EqualFold(left, right) {
			return true
		}
	}
	return left == right
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}
