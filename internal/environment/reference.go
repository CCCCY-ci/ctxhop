package environment

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxReferences = 64
	maxNameRunes  = 128
	maxVersionLen = 64
)

// Reference is a dependency observed in a session. It is metadata only: v1
// does not carry configuration content or credentials.
type Reference struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Portability string `json:"portability"`
}

var (
	ErrInvalidReference = errors.New("environment: invalid dependency reference")
	allowedKinds        = map[string]bool{
		"mcp":              true,
		"skill":            true,
		"settings":         true,
		"tool-requirement": true,
	}
	allowedPortability = map[string]bool{
		"portable":          true,
		"platform-specific": true,
		"unsupported":       true,
	}
)

// Validate checks a reference before it crosses the encrypted metadata
// boundary. Names are logical names, never local paths.
func (r Reference) Validate() error {
	if !allowedKinds[r.Kind] {
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidReference, r.Kind)
	}
	if !validName(r.Name) {
		return fmt.Errorf("%w: invalid name", ErrInvalidReference)
	}
	if r.Kind == "settings" && r.Name != codexSessionSettingsName {
		return fmt.Errorf("%w: unsupported settings reference", ErrInvalidReference)
	}
	if r.Version != "" && (!utf8.ValidString(r.Version) || len([]rune(r.Version)) > maxVersionLen || strings.ContainsAny(r.Version, "\r\n")) {
		return fmt.Errorf("%w: invalid version", ErrInvalidReference)
	}
	if !allowedPortability[r.Portability] {
		return fmt.Errorf("%w: unsupported portability %q", ErrInvalidReference, r.Portability)
	}
	return nil
}

// Normalize validates, sorts, and de-duplicates references. Invalid or
// incomplete observations are discarded; they must never trigger a broad
// environment upload.
func Normalize(input []Reference) []Reference {
	refs := make([]Reference, 0, len(input))
	for _, ref := range input {
		if err := ref.Validate(); err != nil {
			continue
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		if refs[i].Version != refs[j].Version {
			return refs[i].Version < refs[j].Version
		}
		return refs[i].Portability < refs[j].Portability
	})
	out := refs[:0]
	for _, ref := range refs {
		if len(out) == 0 || out[len(out)-1] != ref {
			if len(out) == MaxReferences {
				break
			}
			out = append(out, ref)
		}
	}
	return out
}

// Discover extracts only structured dependency evidence from session records.
// Free-form user or model text is intentionally ignored.
func Discover(records [][]byte, agent, version string) []Reference {
	var refs []Reference
	if agent != "" && version != "" {
		refs = append(refs, Reference{
			Kind:        "tool-requirement",
			Name:        agent,
			Version:     version,
			Portability: "platform-specific",
		})
	}
	if agent == "codex" && hasObservedCodexSessionSettings(records) {
		refs = append(refs, Reference{
			Kind:        "settings",
			Name:        codexSessionSettingsName,
			Portability: "platform-specific",
		})
	}

	for _, record := range records {
		recordType, payload, ok := decodeRecord(record)
		if !ok {
			continue
		}
		payloadType := rawString(payload["type"])
		if recordType == "session_meta" {
			if observedVersion := rawString(payload["cli_version"]); observedVersion != "" && agent != "" {
				refs = append(refs, Reference{
					Kind:        "tool-requirement",
					Name:        agent,
					Version:     observedVersion,
					Portability: "platform-specific",
				})
			}
		}
		if isToolCall(recordType, payloadType) {
			if server := mcpServerFromPayload(payload); server != "" {
				refs = append(refs, Reference{
					Kind:        "mcp",
					Name:        server,
					Portability: "platform-specific",
				})
			}
		}
		if isSkillCall(recordType, payloadType) {
			for _, name := range namesFromPayload(payload, "skill", "skill_name", "skillName", "name") {
				refs = append(refs, Reference{
					Kind:        "skill",
					Name:        name,
					Portability: "portable",
				})
			}
		}
	}
	return Normalize(refs)
}

func validName(value string) bool {
	if value == "" || !utf8.ValidString(value) || len([]rune(value)) > maxNameRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == 0 || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func decodeRecord(record []byte) (string, map[string]json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(record, &root); err != nil {
		return "", nil, false
	}
	recordType := rawString(root["type"])
	payload := root
	if raw, ok := root["payload"]; ok {
		var candidate map[string]json.RawMessage
		if err := json.Unmarshal(raw, &candidate); err == nil && candidate != nil {
			payload = candidate
		}
	}
	return recordType, payload, true
}

func rawString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func isToolCall(recordType, payloadType string) bool {
	for _, value := range []string{recordType, payloadType} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "function_call", "custom_tool_call", "mcp_tool_call", "tool_call":
			return true
		}
	}
	return false
}

func isSkillCall(recordType, payloadType string) bool {
	for _, value := range []string{recordType, payloadType} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "skill_call", "skill_use", "skill_used", "skill_invocation":
			return true
		}
	}
	return false
}

func mcpServerFromPayload(payload map[string]json.RawMessage) string {
	for _, key := range []string{"server", "server_name", "serverName", "mcp_server", "mcpServer"} {
		if value := rawString(payload[key]); value != "" {
			return value
		}
	}
	name := rawString(payload["name"])
	parts := strings.Split(name, "__")
	if len(parts) >= 3 && strings.EqualFold(parts[0], "mcp") {
		return parts[1]
	}
	return ""
}

func namesFromPayload(payload map[string]json.RawMessage, keys ...string) []string {
	var names []string
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		if value := rawString(raw); value != "" {
			names = append(names, value)
			continue
		}
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			names = append(names, list...)
			continue
		}
		var object struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &object) == nil && object.Name != "" {
			names = append(names, object.Name)
		}
	}
	return names
}
