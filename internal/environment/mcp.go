package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxMCPConfigBytes = 1 << 20
	maxMCPArgs        = 32
	maxMCPArgLength   = 256
)

type mcpServerConfig struct {
	Command           string
	Args              []string
	StartupTimeoutSec int
	Invalid           bool
}

func inspectMCPComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	path, err := mcpComponentPath(component, agentHome, projectRoot)
	if err != nil {
		return LocalComponentState{State: ComponentStateUnavailable, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		result.State = ComponentStateMissing
		return result
	case err != nil:
		result.State = ComponentStateUnavailable
		result.Reason = err.Error()
		return result
	case !info.Mode().IsRegular():
		result.State = ComponentStateFailed
		result.Reason = "local MCP config is not a regular file"
		return result
	}
	servers := readMCPConfig(path)
	server, found := servers[component.Name]
	if !found {
		result.State = ComponentStateMissing
		return result
	}
	reference := Reference{Kind: "mcp", Name: component.Name, Portability: component.Portability}
	local, safe := newMCPComponent(reference, component.Scope, component.ProjectID, server)
	if !safe {
		result.State = ComponentStateUnavailable
		result.Reason = "local MCP intent is unavailable or contains unsupported or sensitive values"
		return result
	}
	if strings.EqualFold(local.Component.Fingerprint, component.Fingerprint) {
		result.State = ComponentStateUnchanged
	} else {
		result.State = ComponentStateChanged
	}
	return result
}

func mcpComponentPath(component Component, agentHome, projectRoot string) (string, error) {
	switch component.Scope {
	case "global":
		if strings.TrimSpace(agentHome) == "" {
			return "", os.ErrNotExist
		}
		return filepath.Join(agentHome, "config.toml"), nil
	case "project":
		if strings.TrimSpace(projectRoot) == "" {
			return "", os.ErrNotExist
		}
		return filepath.Join(projectRoot, ".codex", "config.toml"), nil
	default:
		return "", ErrInvalidComponent
	}
}

// CaptureMCPComponents extracts only the observed Codex MCP servers from the
// small allowlisted subset of config.toml. Environment sections and all their
// values are ignored. The resulting component is an intent record, not a copy
// of the original TOML and not an executable command.
func CaptureMCPComponents(agent, agentHome, projectRoot, projectID string, references []Reference) []ComponentContent {
	if agent != "codex" {
		return nil
	}
	var global map[string]mcpServerConfig
	if strings.TrimSpace(agentHome) != "" {
		global = readMCPConfig(filepath.Join(agentHome, "config.toml"))
	}
	var project map[string]mcpServerConfig
	if strings.TrimSpace(projectRoot) != "" {
		project = readMCPConfig(filepath.Join(projectRoot, ".codex", "config.toml"))
	}
	var captured []ComponentContent
	for _, reference := range Normalize(references) {
		if reference.Kind != "mcp" {
			continue
		}
		globalConfig, globalFound := global[reference.Name]
		projectConfig, projectFound := project[reference.Name]
		var globalComponent ComponentContent
		var projectComponent ComponentContent
		var globalComponentFound, projectComponentFound bool
		if globalFound {
			if component, ok := newMCPComponent(reference, "global", "", globalConfig); ok {
				globalComponent = component
				globalComponentFound = true
				captured = append(captured, component)
			}
		}
		if projectFound {
			if component, ok := newMCPComponent(reference, "project", projectID, projectConfig); ok {
				projectComponent = component
				projectComponentFound = true
			}
		}
		if !projectComponentFound {
			continue
		}
		if globalComponentFound && strings.EqualFold(globalComponent.Component.Fingerprint, projectComponent.Component.Fingerprint) {
			continue
		}
		captured = append(captured, projectComponent)
	}
	return NormalizeComponentContents(captured)
}

func newMCPComponent(reference Reference, scope, projectID string, server mcpServerConfig) (ComponentContent, bool) {
	if server.Invalid {
		return ComponentContent{}, false
	}
	command, ok := safeMCPCommand(server.Command)
	if !ok {
		return ComponentContent{}, false
	}
	args := make([]string, 0, len(server.Args))
	if len(server.Args) > maxMCPArgs {
		return ComponentContent{}, false
	}
	for _, argument := range server.Args {
		if !safeMCPArgument(argument) {
			return ComponentContent{}, false
		}
		args = append(args, argument)
	}
	wire := map[string]any{"command": command}
	if len(args) != 0 {
		wire["args"] = args
	}
	if server.StartupTimeoutSec > 0 {
		wire["startupTimeoutSec"] = server.StartupTimeoutSec
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return ComponentContent{}, false
	}
	component, err := NewComponentContent("mcp", reference.Name, scope, projectID, reference.Portability, "application/json", payload)
	if err != nil {
		return ComponentContent{}, false
	}
	return component, true
}

func readMCPConfig(path string) map[string]mcpServerConfig {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxMCPConfigBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseMCPConfig(data)
}

func parseMCPConfig(data []byte) map[string]mcpServerConfig {
	servers := make(map[string]mcpServerConfig)
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
	current := ""
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(stripTOMLComment(lines[index]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = parseMCPSection(line[1 : len(line)-1])
			continue
		}
		if current == "" {
			continue
		}
		key, value, ok := splitTOMLAssignment(line)
		if !ok {
			continue
		}
		if key == "args" && !strings.Contains(value, "]") {
			for index+1 < len(lines) {
				index++
				value += " " + stripTOMLComment(lines[index])
				if strings.Contains(value, "]") {
					break
				}
			}
		}
		server := servers[current]
		switch key {
		case "command":
			if parsed, ok := parseTOMLString(value); ok {
				server.Command = parsed
			} else {
				server.Invalid = true
			}
		case "args":
			if parsed, ok := parseTOMLStringArray(value); ok {
				server.Args = parsed
			} else {
				server.Invalid = true
			}
		case "startup_timeout_sec":
			if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed >= 0 && parsed <= 86400 {
				server.StartupTimeoutSec = parsed
			}
		}
		servers[current] = server
	}
	return servers
}

func parseMCPSection(value string) string {
	value = strings.TrimSpace(value)
	const prefix = "mcp_servers."
	if !strings.HasPrefix(value, prefix) {
		return ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if name == "" || strings.HasSuffix(name, ".env") {
		return ""
	}
	if strings.HasPrefix(name, "\"") && strings.HasSuffix(name, "\"") {
		parsed, ok := parseTOMLString(name)
		if !ok {
			return ""
		}
		name = parsed
	}
	if (Reference{Kind: "mcp", Name: name, Portability: "platform-specific"}).Validate() != nil {
		return ""
	}
	return name
}

func splitTOMLAssignment(line string) (string, string, bool) {
	separator := strings.IndexByte(line, '=')
	if separator <= 0 || separator == len(line)-1 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:separator])
	value := strings.TrimSpace(line[separator+1:])
	if key == "" || value == "" {
		return "", "", false
	}
	return key, value, true
}

func parseTOMLString(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return "", false
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1], true
	}
	if value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	parsed, err := strconv.Unquote(value)
	return parsed, err == nil
}

func parseTOMLStringArray(value string) ([]string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, false
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return nil, true
	}
	var result []string
	for len(body) != 0 {
		body = strings.TrimSpace(body)
		if body == "" {
			break
		}
		if body[0] != '"' && body[0] != '\'' {
			return nil, false
		}
		quote := body[0]
		end := 1
		for end < len(body) {
			if body[end] == quote && (quote == '\'' || body[end-1] != '\\') {
				break
			}
			end++
		}
		if end >= len(body) {
			return nil, false
		}
		parsed, ok := parseTOMLString(body[:end+1])
		if !ok {
			return nil, false
		}
		result = append(result, parsed)
		body = strings.TrimSpace(body[end+1:])
		if body == "" {
			break
		}
		if body[0] != ',' {
			return nil, false
		}
		body = body[1:]
	}
	return result, true
}

func stripTOMLComment(line string) string {
	quoted := byte(0)
	escaped := false
	for index := 0; index < len(line); index++ {
		char := line[index]
		if quoted != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' && quoted == '"' {
				escaped = true
				continue
			}
			if char == quoted {
				quoted = 0
			}
			continue
		}
		if char == '"' || char == '\'' {
			quoted = char
			continue
		}
		if char == '#' {
			return line[:index]
		}
	}
	return line
}

func safeMCPCommand(command string) (string, bool) {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\r\n\t ;&|<>`$") || sensitiveMCPText(command) {
		return "", false
	}
	if strings.ContainsAny(command, `/\\`) {
		command = filepath.Base(strings.ReplaceAll(command, "\\", "/"))
	}
	if command == "." || command == string(filepath.Separator) || strings.ContainsAny(command, `/\\`) || len(command) > maxMCPArgLength {
		return "", false
	}
	return command, true
}

func safeMCPArgument(argument string) bool {
	argument = strings.TrimSpace(argument)
	if argument == "" || len(argument) > maxMCPArgLength || strings.ContainsAny(argument, "\r\n") || sensitiveMCPText(argument) {
		return false
	}
	if filepath.IsAbs(argument) || strings.HasPrefix(argument, "/") || windowsAbsolutePath(argument) {
		return false
	}
	return true
}

func sensitiveMCPText(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"token=", "token:", "secret=", "secret:", "password=", "password:",
		"api_key=", "api-key=", "access_key=", "access-key=", "authorization:",
		"bearer ", "oauth", "cookie=",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return containsSensitiveMaterial([]byte(value + "\n"))
}

func windowsAbsolutePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}
