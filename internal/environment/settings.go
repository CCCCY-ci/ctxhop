package environment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const codexSessionSettingsName = "codex-session-settings"

const maxCodexSettingsConfigBytes = 1 << 20

var codexSessionSettingKeys = []string{
	"effort",
	"model",
	"model_provider",
}

var codexConfigSettingKeys = map[string]string{
	"effort":                 "effort",
	"model":                  "model",
	"model_provider":         "model_provider",
	"model_reasoning_effort": "effort",
}

func inspectCodexSettingsComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	path, err := codexSettingsPath(component, agentHome, projectRoot)
	if err != nil {
		return LocalComponentState{State: ComponentStateUnavailable, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	root, rootErr := configComponentRoot(component, agentHome, projectRoot)
	if rootErr != nil {
		result.State = ComponentStateUnavailable
		result.Reason = rootErr.Error()
		return result
	}
	if targetErr := validateConfigTarget(root, path); targetErr != nil {
		result.State = ComponentStateFailed
		result.Reason = targetErr.Error()
		return result
	}
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		result.State = ComponentStateMissing
		return result
	case err != nil:
		result.State = ComponentStateUnavailable
		result.Reason = err.Error()
		return result
	case info.Mode()&os.ModeSymlink != 0:
		result.State = ComponentStateFailed
		result.Reason = "local Codex settings file is a symbolic link"
		return result
	case !info.Mode().IsRegular():
		result.State = ComponentStateFailed
		result.Reason = "local Codex settings file is not a regular file"
		return result
	case info.Size() <= 0:
		result.State = ComponentStateMissing
		return result
	case info.Size() > maxCodexSettingsConfigBytes:
		result.State = ComponentStateUnavailable
		result.Reason = "local Codex settings file exceeds the inspection limit"
		return result
	}
	values, found, safe := readCodexConfigSettings(path)
	if !safe {
		result.State = ComponentStateUnavailable
		result.Reason = "local Codex settings contain unsupported or sensitive values"
		return result
	}
	if !found {
		result.State = ComponentStateMissing
		return result
	}
	payload, err := json.Marshal(values)
	if err != nil {
		result.State = ComponentStateUnavailable
		result.Reason = "local Codex settings could not be normalized"
		return result
	}
	local, err := NewComponentContent("settings", codexSessionSettingsName, component.Scope, component.ProjectID, component.Portability, "application/json", payload)
	if err != nil {
		result.State = ComponentStateUnavailable
		result.Reason = "local Codex settings could not be normalized safely"
		return result
	}
	if strings.EqualFold(local.Component.Fingerprint, component.Fingerprint) {
		result.State = ComponentStateUnchanged
	} else {
		result.State = ComponentStateChanged
	}
	if result.State == ComponentStateUnchanged && component.Scope == "global" {
		projectValues, projectFound, projectSafe := readCodexProjectSettings(projectRoot)
		if !projectSafe {
			result.State = ComponentStateUnavailable
			result.Reason = "project Codex settings could not be inspected safely"
			return result
		}
		if projectFound {
			for key, value := range projectValues {
				if values[key] != value {
					result.State = ComponentStateConflict
					result.Reason = "project Codex settings override the global component; apply the project component or resolve it manually"
					return result
				}
			}
		}
	}
	return result
}

func codexSettingsPath(component Component, agentHome, projectRoot string) (string, error) {
	switch component.Scope {
	case "global":
		if strings.TrimSpace(agentHome) == "" {
			return "", fmt.Errorf("global Codex settings path is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Join(agentHome, "config.toml"), nil
	case "project":
		if strings.TrimSpace(projectRoot) == "" {
			return "", fmt.Errorf("project Codex settings path is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Join(projectRoot, ".codex", "config.toml"), nil
	default:
		return "", fmt.Errorf("unsupported Codex settings scope %q", component.Scope)
	}
}

func readCodexConfigSettings(path string) (map[string]string, bool, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCodexSettingsConfigBytes {
		return nil, false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, false
	}
	values := make(map[string]string)
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
	inSection := false
	for _, rawLine := range lines {
		line := strings.TrimSpace(stripTOMLComment(rawLine))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = true
			continue
		}
		if inSection {
			continue
		}
		key, rawValue, ok := splitTOMLAssignment(line)
		if !ok {
			continue
		}
		normalizedKey, recognized := codexConfigSettingKeys[key]
		if !recognized {
			continue
		}
		value, valid := parseTOMLString(rawValue)
		if !valid || !safeCodexSettingValue(value) {
			return nil, false, false
		}
		if previous, exists := values[normalizedKey]; exists && previous != value {
			return nil, false, false
		}
		values[normalizedKey] = value
	}
	return values, len(values) != 0, true
}

func readCodexConfigFileSettings(root string) (map[string]string, bool, bool) {
	if strings.TrimSpace(root) == "" {
		return nil, false, true
	}
	return readCodexSettingsFile(filepath.Join(root, "config.toml"))
}

func readCodexProjectSettings(root string) (map[string]string, bool, bool) {
	if strings.TrimSpace(root) == "" {
		return nil, false, true
	}
	return readCodexSettingsFile(filepath.Join(root, ".codex", "config.toml"))
}

func readCodexSettingsFile(path string) (map[string]string, bool, bool) {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, true
	}
	if err != nil {
		return nil, false, false
	}
	return readCodexConfigSettings(path)
}

func safeCodexSettingValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len([]byte(value)) > 256 || !utf8.ValidString(value) {
		return false
	}
	if sensitiveMCPText(value) || containsSensitiveMaterial([]byte(value+"\n")) {
		return false
	}
	for _, character := range value {
		if character == '\r' || character == '\n' || character == '\t' || character == 0 || character < 0x20 {
			return false
		}
	}
	return true
}

// CaptureSessionSettings records only the small set of Codex settings that is
// already present in structured session metadata. Without local config paths it
// keeps the component project-scoped, not a copy of config.toml or an apply plan.
func CaptureSessionSettings(agent string, records [][]byte, projectID string) []ComponentContent {
	return CaptureSessionSettingsWithSources(agent, "", "", records, projectID)
}

// CaptureSessionSettingsWithSources records the observed allowlisted settings
// and chooses the narrowest useful scope. If the observed values are already
// provided by the global config and the project config does not override one
// of them, the component remains global. Otherwise it stays project-scoped.
func CaptureSessionSettingsWithSources(agent, agentHome, projectRoot string, records [][]byte, projectID string) []ComponentContent {
	if agent != "codex" || strings.TrimSpace(projectID) == "" {
		return nil
	}
	values, ok := observedCodexSessionSettings(records)
	if !ok {
		return nil
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	scope, componentProjectID := codexSessionSettingsScope(values, agentHome, projectRoot, projectID)
	component, err := NewComponentContent("settings", codexSessionSettingsName, scope, componentProjectID, "platform-specific", "application/json", payload)
	if err != nil {
		return nil
	}
	return []ComponentContent{component}
}

func codexSessionSettingsScope(values map[string]string, agentHome, projectRoot, projectID string) (string, string) {
	global, globalFound, globalSafe := readCodexConfigFileSettings(agentHome)
	if !globalSafe || !globalFound || len(values) == 0 {
		return "project", projectID
	}
	if len(global) != len(values) {
		return "project", projectID
	}
	for key, value := range values {
		if global[key] != value {
			return "project", projectID
		}
	}
	project, projectFound, projectSafe := readCodexProjectSettings(projectRoot)
	if !projectSafe {
		return "project", projectID
	}
	if projectFound {
		for key, value := range project {
			if observed, ok := values[key]; ok && observed != value {
				return "project", projectID
			}
		}
	}
	return "global", ""
}

func hasObservedCodexSessionSettings(records [][]byte) bool {
	_, ok := observedCodexSessionSettings(records)
	return ok
}

func observedCodexSessionSettings(records [][]byte) (map[string]string, bool) {
	values := make(map[string]string, len(codexSessionSettingKeys))
	conflicts := make(map[string]bool)
	for _, record := range records {
		recordType, payload, ok := decodeRecord(record)
		if !ok || recordType != "session_meta" {
			continue
		}
		for _, key := range codexSessionSettingKeys {
			raw, present := payload[key]
			if !present {
				continue
			}
			value, valid := safeCodexSessionSetting(raw)
			if !valid {
				continue
			}
			if previous, exists := values[key]; exists && previous != value {
				conflicts[key] = true
				continue
			}
			values[key] = value
		}
	}
	for key := range conflicts {
		delete(values, key)
	}
	return values, len(values) != 0
}

func safeCodexSessionSetting(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || len([]byte(value)) > 256 || !utf8.ValidString(value) {
		return "", false
	}
	if sensitiveMCPText(value) || containsSensitiveMaterial([]byte(value+"\n")) {
		return "", false
	}
	for _, character := range value {
		if character == '\r' || character == '\n' || character == '\t' || character == 0 || character < 0x20 {
			return "", false
		}
	}
	return value, true
}
