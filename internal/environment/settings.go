package environment

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const codexSessionSettingsName = "codex-session-settings"

var codexSessionSettingKeys = []string{
	"effort",
	"model",
	"model_provider",
}

// CaptureSessionSettings records only the small set of Codex settings that is
// already present in structured session metadata. It is a project-scoped
// snapshot for the session, not a copy of config.toml and not an apply plan.
func CaptureSessionSettings(agent string, records [][]byte, projectID string) []ComponentContent {
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
	component, err := NewComponentContent("settings", codexSessionSettingsName, "project", projectID, "platform-specific", "application/json", payload)
	if err != nil {
		return nil
	}
	return []ComponentContent{component}
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
