package environment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

type mcpIntent struct {
	Command           string
	Args              []string
	HasArgs           bool
	StartupTimeoutSec int
	HasStartupTimeout bool
}

func applyFilteredConfigComponent(content ComponentContent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	state := InspectComponent(content.Component, "codex", agentHome, projectRoot)
	switch state.State {
	case ComponentStateUnchanged:
		return state, nil
	case ComponentStateConflict:
		return state, fmt.Errorf("%w: %s", ErrConfigConflict, state.Reason)
	case ComponentStateMissing, ComponentStateChanged:
	default:
		return state, fmt.Errorf("%w: %s", ErrUnsupportedComponentApply, state.Reason)
	}

	root, path, err := configComponentTarget(content.Component, agentHome, projectRoot)
	if err != nil {
		state.State = ComponentStateFailed
		state.Reason = err.Error()
		return state, err
	}
	if err := validateConfigTarget(root, path); err != nil {
		state.State = ComponentStateFailed
		state.Reason = err.Error()
		return state, err
	}
	existing, exists, err := readConfigForApply(path)
	if err != nil {
		state.State = ComponentStateFailed
		state.Reason = err.Error()
		return state, err
	}

	var updated []byte
	switch content.Component.Kind {
	case "mcp":
		intent, parseErr := parseMCPIntent(content.Content)
		if parseErr != nil {
			state.State = ComponentStateFailed
			state.Reason = parseErr.Error()
			return state, parseErr
		}
		if conflictErr := checkMCPProjectOverride(content.Component, projectRoot); conflictErr != nil {
			state.State = ComponentStateConflict
			state.Reason = conflictErr.Error()
			return state, conflictErr
		}
		updated, err = patchMCPConfig(existing, content.Component.Name, intent)
	case "settings":
		values, parseErr := parseSettingsIntent(content.Content)
		if parseErr != nil {
			state.State = ComponentStateFailed
			state.Reason = parseErr.Error()
			return state, parseErr
		}
		if conflictErr := checkSettingsProjectOverride(content.Component, values, projectRoot); conflictErr != nil {
			state.State = ComponentStateConflict
			state.Reason = conflictErr.Error()
			return state, conflictErr
		}
		updated, err = patchCodexSettings(existing, values)
	default:
		err = ErrUnsupportedComponentApply
	}
	if err != nil {
		state.State = ComponentStateFailed
		state.Reason = err.Error()
		return state, err
	}
	if bytes.Equal(existing, updated) {
		state.State = ComponentStateUnchanged
		state.Reason = ""
		return state, nil
	}

	if exists {
		if strings.TrimSpace(backupRoot) == "" {
			state.State = ComponentStateFailed
			state.Reason = "backup directory is required before replacing a configuration file"
			return state, errors.New(state.Reason)
		}
		if err := os.MkdirAll(backupRoot, 0o700); err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("create configuration backup directory: %v", err)
			return state, err
		}
		state.Backup = filepath.Join(backupRoot, backupFileName(content.Component))
		if err := atomicfile.WriteBytes(state.Backup, existing); err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("write configuration backup: %v", err)
			return state, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("create configuration directory: %v", err)
		return state, err
	}
	if err := atomicfile.WriteBytes(path, updated); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("write filtered configuration values: %v", err)
		return state, err
	}
	state.Path = path
	state.State = ComponentStateApplied
	state.Reason = ""
	return state, nil
}

func checkMCPProjectOverride(component Component, projectRoot string) error {
	if component.Scope != "global" || strings.TrimSpace(projectRoot) == "" {
		return nil
	}
	projectServers := readMCPConfig(filepath.Join(projectRoot, ".codex", "config.toml"))
	projectServer, found := projectServers[component.Name]
	if !found {
		return nil
	}
	reference := Reference{Kind: "mcp", Name: component.Name, Portability: component.Portability}
	projectComponent, safe := newMCPComponent(reference, "project", component.ProjectID, projectServer)
	if !safe {
		return fmt.Errorf("%w: project MCP configuration overrides the global component with an unsafe intent", ErrConfigConflict)
	}
	if !strings.EqualFold(projectComponent.Component.Fingerprint, component.Fingerprint) {
		return fmt.Errorf("%w: project MCP configuration overrides the global component; apply the project component or resolve it manually", ErrConfigConflict)
	}
	return nil
}

func checkSettingsProjectOverride(component Component, values map[string]string, projectRoot string) error {
	if component.Scope != "global" || strings.TrimSpace(projectRoot) == "" {
		return nil
	}
	projectValues, found, safe := readCodexProjectSettings(projectRoot)
	if !safe {
		return fmt.Errorf("%w: project Codex settings could not be inspected safely", ErrConfigConflict)
	}
	if !found {
		return nil
	}
	for key, value := range values {
		if projectValue, exists := projectValues[key]; exists && projectValue != value {
			return fmt.Errorf("%w: project Codex settings override the global component; apply the project component or resolve it manually", ErrConfigConflict)
		}
	}
	return nil
}
func configComponentTarget(component Component, agentHome, projectRoot string) (string, string, error) {
	if component.Kind != "mcp" && component.Kind != "settings" {
		return "", "", ErrUnsupportedComponentApply
	}
	root, err := configComponentRoot(component, agentHome, projectRoot)
	if err != nil {
		return "", "", err
	}
	var path string
	switch component.Kind {
	case "mcp":
		path, err = mcpComponentPath(component, agentHome, projectRoot)
	case "settings":
		path, err = codexSettingsPath(component, agentHome, projectRoot)
	}
	if err != nil {
		return "", "", err
	}
	return root, path, nil
}

func configComponentRoot(component Component, agentHome, projectRoot string) (string, error) {
	switch component.Scope {
	case "global":
		if strings.TrimSpace(agentHome) == "" {
			return "", fmt.Errorf("global Codex home is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Clean(agentHome), nil
	case "project":
		if strings.TrimSpace(projectRoot) == "" {
			return "", fmt.Errorf("project root is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Clean(projectRoot), nil
	default:
		return "", fmt.Errorf("unsupported Codex component scope %q", component.Scope)
	}
}

func validateConfigTarget(root, path string) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve configuration root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return fmt.Errorf("inspect configuration root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("configuration root is not a directory")
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve configuration path: %w", err)
	}
	if !pathWithin(absoluteRoot, absolutePath) {
		return errors.New("configuration path escapes its root")
	}
	return resolvedPathWithin(absoluteRoot, absolutePath)
}

func readConfigForApply(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect configuration file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("configuration file is a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("configuration file is not a regular file")
	}
	if info.Size() > maxMCPConfigBytes {
		return nil, false, errors.New("configuration file exceeds the inspection limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read configuration file: %w", err)
	}
	if !utf8.Valid(data) {
		return nil, false, errors.New("configuration file is not valid UTF-8")
	}
	return data, true, nil
}

func parseMCPIntent(data []byte) (mcpIntent, error) {
	raw, err := decodeJSONObject(data)
	if err != nil {
		return mcpIntent{}, fmt.Errorf("MCP intent is invalid: %w", err)
	}
	for key := range raw {
		switch key {
		case "command", "args", "startupTimeoutSec":
		default:
			return mcpIntent{}, fmt.Errorf("MCP intent contains unsupported key %q", key)
		}
	}
	commandRaw, ok := raw["command"]
	if !ok {
		return mcpIntent{}, errors.New("MCP intent has no command")
	}
	var command string
	if err := json.Unmarshal(commandRaw, &command); err != nil {
		return mcpIntent{}, errors.New("MCP intent command is invalid")
	}
	command, ok = safeMCPCommand(command)
	if !ok {
		return mcpIntent{}, errors.New("MCP intent command is unsafe")
	}
	intent := mcpIntent{Command: command}
	if argsRaw, found := raw["args"]; found {
		if err := json.Unmarshal(argsRaw, &intent.Args); err != nil || len(intent.Args) > maxMCPArgs {
			return mcpIntent{}, errors.New("MCP intent arguments are invalid")
		}
		for _, argument := range intent.Args {
			if !safeMCPArgument(argument) {
				return mcpIntent{}, errors.New("MCP intent contains an unsafe argument")
			}
		}
		intent.HasArgs = true
	}
	if timeoutRaw, found := raw["startupTimeoutSec"]; found {
		if err := json.Unmarshal(timeoutRaw, &intent.StartupTimeoutSec); err != nil || intent.StartupTimeoutSec < 0 || intent.StartupTimeoutSec > 86400 {
			return mcpIntent{}, errors.New("MCP intent startup timeout is invalid")
		}
		intent.HasStartupTimeout = true
	}
	return intent, nil
}

func parseSettingsIntent(data []byte) (map[string]string, error) {
	raw, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("session settings are invalid: %w", err)
	}
	values := make(map[string]string, len(raw))
	for key, valueRaw := range raw {
		normalizedKey, ok := codexConfigSettingKeys[key]
		if !ok {
			return nil, fmt.Errorf("session settings contain unsupported key %q", key)
		}
		var value string
		if err := json.Unmarshal(valueRaw, &value); err != nil || !safeCodexSettingValue(value) {
			return nil, fmt.Errorf("session setting %q is unsafe", key)
		}
		if previous, exists := values[normalizedKey]; exists && previous != value {
			return nil, fmt.Errorf("session setting %q has conflicting values", normalizedKey)
		}
		values[normalizedKey] = value
	}
	if len(values) == 0 {
		return nil, errors.New("session settings are empty")
	}
	return values, nil
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return nil, errors.New("expected a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("trailing JSON data")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	return raw, nil
}

func patchMCPConfig(data []byte, name string, intent mcpIntent) ([]byte, error) {
	lines := configLines(data)
	start, end := findMCPSection(lines, name)
	values := map[string]string{
		"command": strconv.Quote(intent.Command),
	}
	if intent.HasArgs {
		values["args"] = formatTOMLArray(intent.Args)
	}
	if intent.HasStartupTimeout {
		values["startup_timeout_sec"] = strconv.Itoa(intent.StartupTimeoutSec)
	}
	order := []string{"command", "args", "startup_timeout_sec"}

	if start < 0 {
		lines = trimTrailingEmptyLines(lines)
		if len(lines) != 0 {
			lines = append(lines, "")
		}
		lines = append(lines, mcpSectionHeader(name))
		for _, key := range order {
			if value, ok := values[key]; ok {
				lines = append(lines, key+" = "+value)
			}
		}
		return marshalConfigLines(lines), nil
	}

	out := append([]string(nil), lines[:start+1]...)
	seen := make(map[string]bool, len(values))
	for index := start + 1; index < end; index++ {
		line := lines[index]
		key, _, ok := splitTOMLAssignment(strings.TrimSpace(stripTOMLComment(line)))
		if ok && isMCPManagedKey(key) {
			if seen[key] {
				continue
			}
			seen[key] = true
			if value, keep := values[key]; keep {
				out = append(out, key+" = "+value)
			}
			continue
		}
		out = append(out, line)
	}
	for _, key := range order {
		if value, ok := values[key]; ok && !seen[key] {
			out = append(out, key+" = "+value)
		}
	}
	out = append(out, lines[end:]...)
	return marshalConfigLines(out), nil
}

func findMCPSection(lines []string, name string) (int, int) {
	start := -1
	end := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(stripTOMLComment(line))
		if len(trimmed) < 2 || trimmed[0] != '[' || trimmed[len(trimmed)-1] != ']' {
			continue
		}
		if start >= 0 {
			end = index
			break
		}
		section := parseMCPSection(trimmed[1 : len(trimmed)-1])
		if section == name {
			start = index
		}
	}
	return start, end
}

func isMCPManagedKey(key string) bool {
	switch key {
	case "command", "args", "startup_timeout_sec":
		return true
	default:
		return false
	}
}

func mcpSectionHeader(name string) string {
	if strings.ContainsAny(name, ".\"") {
		return "[mcp_servers." + strconv.Quote(name) + "]"
	}
	return "[mcp_servers." + name + "]"
}

func formatTOMLArray(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func patchCodexSettings(data []byte, values map[string]string) ([]byte, error) {
	lines := configLines(data)
	firstSection := len(lines)
	for index, line := range lines {
		trimmed := strings.TrimSpace(stripTOMLComment(line))
		if len(trimmed) >= 2 && trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']' {
			firstSection = index
			break
		}
	}
	prefix := append([]string(nil), lines[:firstSection]...)
	suffix := append([]string(nil), lines[firstSection:]...)
	prefix = trimTrailingEmptyLines(prefix)
	targetKeys := make([]string, 0, len(values))
	for key := range values {
		targetKeys = append(targetKeys, key)
	}
	sort.Strings(targetKeys)
	seen := make(map[string]bool, len(values))
	filtered := make([]string, 0, len(prefix)+len(values))
	for _, line := range prefix {
		key, _, ok := splitTOMLAssignment(strings.TrimSpace(stripTOMLComment(line)))
		normalized, recognized := codexConfigSettingKeys[key]
		if ok && recognized {
			if _, wanted := values[normalized]; wanted {
				if seen[normalized] {
					continue
				}
				seen[normalized] = true
				filtered = append(filtered, canonicalCodexConfigLine(normalized, values[normalized]))
				continue
			}
		}
		filtered = append(filtered, line)
	}
	for _, key := range targetKeys {
		if !seen[key] {
			filtered = append(filtered, canonicalCodexConfigLine(key, values[key]))
		}
	}
	filtered = append(filtered, suffix...)
	return marshalConfigLines(filtered), nil
}

func canonicalCodexConfigLine(key, value string) string {
	switch key {
	case "effort":
		key = "model_reasoning_effort"
	}
	return key + " = " + strconv.Quote(value)
}

func configLines(data []byte) []string {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func trimTrailingEmptyLines(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func marshalConfigLines(lines []string) []byte {
	text := strings.Join(lines, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text == "" {
		text = "\n"
	}
	return []byte(text)
}
