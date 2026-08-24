package environment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// InspectClaudeComponent compares a filtered component with Claude Code's
// local allowlisted state without exposing raw configuration values.
func InspectClaudeComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	return inspectClaudeComponent(component, agentHome, projectRoot)
}

// ApplyClaudeComponent applies one already-confirmed filtered component. It
// never installs tools, runs commands, copies credentials, or writes hooks.
func ApplyClaudeComponent(content ComponentContent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	return applyClaudeComponent(content, agentHome, projectRoot, backupRoot)
}

func inspectClaudeComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	if err := component.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateUnavailable, Reason: "remote component metadata is invalid"}
	}
	switch component.Kind {
	case "skill":
		return inspectClaudeSkillComponent(component, agentHome, projectRoot)
	case "mcp":
		return inspectClaudeMCPComponent(component, agentHome, projectRoot)
	case "settings":
		return inspectClaudeSettingsComponent(component, agentHome, projectRoot)
	default:
		return LocalComponentState{State: ComponentStateManual, Reason: "component kind has no Claude Code target"}
	}
}

func inspectClaudeSkillComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	root, path, err := claudeSkillTarget(component, agentHome, projectRoot)
	if err != nil {
		return LocalComponentState{Path: path, State: ComponentStateUnavailable, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	if err := validateConfigTarget(root, path); err != nil {
		result.State = ComponentStateFailed
		result.Reason = err.Error()
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
		result.Reason = "Claude Skill file is a symbolic link"
		return result
	case info.IsDir():
		result.State = ComponentStateFailed
		result.Reason = "Claude Skill target is a directory"
		return result
	}
	content, found := readSkillDocument(root, path)
	if !found {
		result.State = ComponentStateFailed
		result.Reason = "existing Claude Skill file is unreadable or contains sensitive material"
		return result
	}
	local, err := NewComponentContent("skill", component.Name, component.Scope, component.ProjectID, component.Portability, "text/markdown", content)
	if err != nil {
		result.State = ComponentStateFailed
		result.Reason = "existing Claude Skill file is not a safe filtered text component"
		return result
	}
	if strings.EqualFold(local.Component.Fingerprint, component.Fingerprint) {
		result.State = ComponentStateUnchanged
	} else {
		result.State = ComponentStateChanged
	}
	return result
}

func inspectClaudeMCPComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	root, path, err := claudeMCPTarget(component, agentHome, projectRoot)
	if err != nil {
		return LocalComponentState{Path: path, State: ComponentStateUnavailable, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	if err := validateConfigTarget(root, path); err != nil {
		result.State = ComponentStateFailed
		result.Reason = err.Error()
		return result
	}

	userDocument, _, userSafe := readClaudeJSONFile(claudeUserConfigPath(agentHome))
	projectDocument, _, projectSafe := readClaudeJSONFile(filepath.Join(projectRoot, claudeMCPFileName))
	if !userSafe && component.Scope == "global" {
		result.State = ComponentStateUnavailable
		result.Reason = "Claude user configuration is unavailable or unsafe"
		return result
	}
	if !projectSafe && component.Scope == "project" {
		result.State = ComponentStateUnavailable
		result.Reason = "Claude project MCP configuration is unavailable or unsafe"
		return result
	}

	var candidates []ComponentContent
	switch component.Scope {
	case "global":
		servers, found, safe := claudeMCPServers(userDocument)
		if !safe {
			result.State = ComponentStateUnavailable
			result.Reason = "Claude global MCP configuration is unavailable or unsafe"
			return result
		}
		if found {
			if raw, exists := servers[component.Name]; exists {
				intent, valid := parseClaudeMCPIntent(raw)
				if !valid {
					result.State = ComponentStateUnavailable
					result.Reason = "Claude global MCP intent is unavailable or contains unsupported or sensitive values"
					return result
				}
				local, createErr := newClaudeMCPComponent(
					Reference{Kind: "mcp", Name: component.Name, Portability: component.Portability},
					"global", "", intent,
				)
				if createErr != nil {
					result.State = ComponentStateUnavailable
					result.Reason = "Claude global MCP intent could not be normalized"
					return result
				}
				candidates = append(candidates, local)
			}
		}
	case "project":
		sharedServers, sharedFound, sharedSafe := claudeMCPServers(projectDocument)
		localServers, localFound, localSafe := claudeProjectMCPServers(userDocument, projectRoot)
		if !sharedSafe || !localSafe {
			result.State = ComponentStateUnavailable
			result.Reason = "Claude project MCP configuration is unavailable or unsafe"
			return result
		}
		reference := Reference{Kind: "mcp", Name: component.Name, Portability: component.Portability}
		for _, source := range []struct {
			servers map[string]json.RawMessage
			found   bool
		}{{sharedServers, sharedFound}, {localServers, localFound}} {
			if !source.found {
				continue
			}
			raw, exists := source.servers[component.Name]
			if !exists {
				continue
			}
			intent, valid := parseClaudeMCPIntent(raw)
			if !valid {
				result.State = ComponentStateUnavailable
				result.Reason = "Claude project MCP intent is unavailable or contains unsupported or sensitive values"
				return result
			}
			local, createErr := newClaudeMCPComponent(reference, "project", component.ProjectID, intent)
			if createErr != nil {
				result.State = ComponentStateUnavailable
				result.Reason = "Claude project MCP intent could not be normalized"
				return result
			}
			candidates = append(candidates, local)
		}
	default:
		result.State = ComponentStateUnavailable
		result.Reason = "unsupported Claude component scope"
		return result
	}

	if len(candidates) == 0 {
		result.State = ComponentStateMissing
		return result
	}
	if len(candidates) > 1 && !strings.EqualFold(candidates[0].Component.Fingerprint, candidates[1].Component.Fingerprint) {
		result.State = ComponentStateConflict
		result.Reason = "Claude project MCP has conflicting shared and local definitions; resolve the conflict before applying"
		return result
	}
	local := candidates[0]
	if !strings.EqualFold(local.Component.Fingerprint, component.Fingerprint) {
		result.State = ComponentStateChanged
		return result
	}
	result.State = ComponentStateUnchanged
	if component.Scope == "global" {
		if conflict := inspectClaudeProjectMCPOverride(component, agentHome, projectRoot); conflict != nil {
			result.State = ComponentStateConflict
			result.Reason = conflict.Error()
		}
	}
	return result
}

func inspectClaudeProjectMCPOverride(component Component, agentHome, projectRoot string) error {
	if strings.TrimSpace(projectRoot) == "" {
		return nil
	}
	userDocument, _, userSafe := readClaudeJSONFile(claudeUserConfigPath(agentHome))
	projectDocument, _, projectSafe := readClaudeJSONFile(filepath.Join(projectRoot, claudeMCPFileName))
	if !userSafe || !projectSafe {
		return fmt.Errorf("%w: Claude project MCP configuration could not be inspected safely", ErrConfigConflict)
	}
	sharedServers, sharedFound, sharedSafe := claudeMCPServers(projectDocument)
	localServers, localFound, localSafe := claudeProjectMCPServers(userDocument, projectRoot)
	if !sharedSafe || !localSafe {
		return fmt.Errorf("%w: Claude project MCP configuration could not be inspected safely", ErrConfigConflict)
	}
	var fingerprints []string
	reference := Reference{Kind: "mcp", Name: component.Name, Portability: component.Portability}
	for _, source := range []struct {
		servers map[string]json.RawMessage
		found   bool
	}{{sharedServers, sharedFound}, {localServers, localFound}} {
		if !source.found {
			continue
		}
		raw, exists := source.servers[component.Name]
		if !exists {
			continue
		}
		intent, valid := parseClaudeMCPIntent(raw)
		if !valid {
			return fmt.Errorf("%w: Claude project MCP override is unsafe", ErrConfigConflict)
		}
		local, err := newClaudeMCPComponent(reference, "project", component.ProjectID, intent)
		if err != nil {
			return fmt.Errorf("%w: Claude project MCP override is unsafe", ErrConfigConflict)
		}
		fingerprints = append(fingerprints, local.Component.Fingerprint)
	}
	for _, fingerprint := range fingerprints {
		if !strings.EqualFold(fingerprint, component.Fingerprint) {
			return fmt.Errorf("%w: Claude project MCP configuration overrides the global component; apply the project component or resolve it manually", ErrConfigConflict)
		}
	}
	return nil
}

func inspectClaudeSettingsComponent(component Component, agentHome, projectRoot string) LocalComponentState {
	root, path, err := claudeSettingsTarget(component, agentHome, projectRoot)
	if err != nil {
		return LocalComponentState{Path: path, State: ComponentStateUnavailable, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	if err := validateConfigTarget(root, path); err != nil {
		result.State = ComponentStateFailed
		result.Reason = err.Error()
		return result
	}

	switch component.Scope {
	case "global":
		local, found, safe := claudeSettingsValues(filepath.Join(agentHome, claudeSessionSettingsFileName))
		if !safe {
			result.State = ComponentStateUnavailable
			result.Reason = "Claude global settings are unavailable or unsafe"
			return result
		}
		if !found {
			result.State = ComponentStateMissing
			return result
		}
		localComponent, err := claudeSettingsComponentFromValues(component, local)
		if err != nil {
			result.State = ComponentStateUnavailable
			result.Reason = err.Error()
			return result
		}
		if !strings.EqualFold(localComponent.Component.Fingerprint, component.Fingerprint) {
			result.State = ComponentStateChanged
			return result
		}
		if projectValues, projectFound, projectSafe := claudeProjectSettingsValues(projectRoot); !projectSafe {
			result.State = ComponentStateUnavailable
			result.Reason = "Claude project settings could not be inspected safely"
			return result
		} else if projectFound {
			for key, value := range projectValues {
				if observed, ok := local[key]; ok && observed != value {
					result.State = ComponentStateConflict
					result.Reason = "Claude project settings override the global component; apply the project component or resolve it manually"
					return result
				}
			}
		}
	case "project":
		local, found, safe := claudeProjectSettingsValues(projectRoot)
		if !safe {
			result.State = ComponentStateUnavailable
			result.Reason = "Claude project settings are unavailable or unsafe"
			return result
		}
		if !found {
			result.State = ComponentStateMissing
			return result
		}
		localComponent, err := claudeSettingsComponentFromValues(component, local)
		if err != nil {
			result.State = ComponentStateUnavailable
			result.Reason = err.Error()
			return result
		}
		if !strings.EqualFold(localComponent.Component.Fingerprint, component.Fingerprint) {
			result.State = ComponentStateChanged
			return result
		}
	default:
		result.State = ComponentStateUnavailable
		result.Reason = "unsupported Claude settings scope"
		return result
	}
	result.State = ComponentStateUnchanged
	return result
}

func claudeSettingsComponentFromValues(component Component, values map[string]string) (ComponentContent, error) {
	payload, err := json.Marshal(values)
	if err != nil {
		return ComponentContent{}, err
	}
	return NewComponentContent("settings", component.Name, component.Scope, component.ProjectID, component.Portability, "application/json", payload)
}
func applyClaudeComponent(content ComponentContent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	if err := content.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateFailed, Reason: "remote component content is invalid"}, err
	}
	state := inspectClaudeComponent(content.Component, agentHome, projectRoot)
	switch state.State {
	case ComponentStateUnchanged:
		return state, nil
	case ComponentStateConflict:
		return state, fmt.Errorf("%w: %s", ErrConfigConflict, state.Reason)
	case ComponentStateMissing, ComponentStateChanged:
	default:
		return state, fmt.Errorf("%w: %s", ErrUnsupportedComponentApply, state.Reason)
	}
	if content.Component.Kind == "skill" {
		return applyClaudeSkill(content, state, agentHome, projectRoot, backupRoot)
	}
	return applyClaudeConfig(content, state, agentHome, projectRoot, backupRoot)
}

func applyClaudeSkill(content ComponentContent, state LocalComponentState, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	root, path, err := claudeSkillTarget(content.Component, agentHome, projectRoot)
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
	var existing []byte
	if state.State == ComponentStateChanged {
		existing, err = os.ReadFile(path)
		if err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("read existing Claude Skill file: %v", err)
			return state, err
		}
	}
	if err := backupClaudeExisting(&state, content.Component, existing, backupRoot); err != nil {
		return state, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("create Claude Skill directory: %v", err)
		return state, err
	}
	if err := atomicfile.WriteBytes(path, content.Content); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("write Claude Skill file: %v", err)
		return state, err
	}
	state.Path = path
	state.State = ComponentStateApplied
	state.Reason = ""
	return state, nil
}

func applyClaudeConfig(content ComponentContent, state LocalComponentState, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	var root, path string
	var err error
	switch content.Component.Kind {
	case "mcp":
		root, path, err = claudeMCPTarget(content.Component, agentHome, projectRoot)
	case "settings":
		root, path, err = claudeSettingsTarget(content.Component, agentHome, projectRoot)
	default:
		err = ErrUnsupportedComponentApply
	}
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
	document := make(map[string]json.RawMessage)
	if exists {
		if json.Unmarshal(existing, &document) != nil || document == nil {
			state.State = ComponentStateFailed
			state.Reason = "Claude configuration is not a JSON object"
			return state, errors.New(state.Reason)
		}
	}
	var updated []byte
	switch content.Component.Kind {
	case "mcp":
		intent, valid := parseClaudeMCPIntent(content.Content)
		if !valid {
			err = errors.New("Claude MCP intent is invalid")
			state.State = ComponentStateFailed
			state.Reason = err.Error()
			return state, err
		}
		updated, err = patchClaudeMCPDocument(document, content.Component, intent, path, agentHome, projectRoot)
	case "settings":
		values, parseErr := parseClaudeSettingsContent(content)
		if parseErr != nil {
			state.State = ComponentStateFailed
			state.Reason = parseErr.Error()
			return state, parseErr
		}
		updated, err = patchClaudeSettingsDocument(document, values)
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
		if err := backupClaudeExisting(&state, content.Component, existing, backupRoot); err != nil {
			return state, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("create Claude configuration directory: %v", err)
		return state, err
	}
	if err := atomicfile.WriteBytes(path, updated); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("write filtered Claude configuration: %v", err)
		return state, err
	}
	state.Path = path
	state.State = ComponentStateApplied
	state.Reason = ""
	return state, nil
}

func backupClaudeExisting(state *LocalComponentState, component Component, existing []byte, backupRoot string) error {
	if len(existing) == 0 {
		return nil
	}
	if strings.TrimSpace(backupRoot) == "" {
		state.State = ComponentStateFailed
		state.Reason = "backup directory is required before replacing a Claude file"
		return errors.New(state.Reason)
	}
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("create Claude backup directory: %v", err)
		return err
	}
	state.Backup = filepath.Join(backupRoot, backupFileName(component))
	if err := atomicfile.WriteBytes(state.Backup, existing); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("write Claude backup: %v", err)
		return err
	}
	return nil
}

func claudeSkillTarget(component Component, agentHome, projectRoot string) (string, string, error) {
	if component.Scope == "global" {
		if strings.TrimSpace(agentHome) == "" {
			return "", "", fmt.Errorf("global Claude home is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Clean(agentHome), filepath.Join(agentHome, "skills", component.Name, "SKILL.md"), nil
	}
	if component.Scope == "project" {
		if strings.TrimSpace(projectRoot) == "" {
			return "", "", fmt.Errorf("Claude project root is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Clean(projectRoot), filepath.Join(projectRoot, ".claude", "skills", component.Name, "SKILL.md"), nil
	}
	return "", "", errors.New("unsupported Claude Skill scope")
}

func claudeMCPTarget(component Component, agentHome, projectRoot string) (string, string, error) {
	if component.Scope == "global" {
		root := claudeHomeParent(agentHome)
		if strings.TrimSpace(root) == "" {
			return "", "", fmt.Errorf("Claude user home is unavailable: %w", os.ErrNotExist)
		}
		return root, claudeUserConfigPath(agentHome), nil
	}
	if component.Scope != "project" || strings.TrimSpace(projectRoot) == "" {
		return "", "", errors.New("Claude project MCP root is unavailable")
	}
	sharedPath := filepath.Join(projectRoot, claudeMCPFileName)
	localPath := claudeUserConfigPath(agentHome)
	if info, err := os.Stat(sharedPath); err == nil && info.Mode().IsRegular() {
		return filepath.Clean(projectRoot), sharedPath, nil
	}
	if _, found, safe := claudeProjectMCPServersFromPath(localPath, projectRoot); found && safe {
		return claudeHomeParent(agentHome), localPath, nil
	}
	return filepath.Clean(projectRoot), sharedPath, nil
}

func claudeProjectMCPServersFromPath(path, projectRoot string) (map[string]json.RawMessage, bool, bool) {
	document, _, safe := readClaudeJSONFile(path)
	if !safe {
		return nil, false, false
	}
	return claudeProjectMCPServers(document, projectRoot)
}

func claudeSettingsTarget(component Component, agentHome, projectRoot string) (string, string, error) {
	if component.Scope == "global" {
		root := filepath.Clean(agentHome)
		if strings.TrimSpace(agentHome) == "" {
			return "", "", fmt.Errorf("global Claude home is unavailable: %w", os.ErrNotExist)
		}
		return root, filepath.Join(root, claudeSessionSettingsFileName), nil
	}
	if component.Scope != "project" || strings.TrimSpace(projectRoot) == "" {
		return "", "", errors.New("Claude project settings root is unavailable")
	}
	root := filepath.Clean(projectRoot)
	localPath := filepath.Join(root, ".claude", claudeLocalSettingsFileName)
	sharedPath := filepath.Join(root, ".claude", claudeProjectSettingsFileName)
	if info, err := os.Stat(localPath); err == nil && info.Mode().IsRegular() {
		return root, localPath, nil
	}
	if info, err := os.Stat(sharedPath); err == nil && info.Mode().IsRegular() {
		return root, sharedPath, nil
	}
	return root, localPath, nil
}

func parseClaudeSettingsContent(content ComponentContent) (map[string]string, error) {
	if content.Component.Kind != "settings" || content.Component.Name != claudeSessionSettingsName {
		return nil, errors.New("Claude settings component is unsupported")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(content.Content, &raw) != nil || raw == nil {
		return nil, errors.New("Claude settings component is not a JSON object")
	}
	values := make(map[string]string, len(raw))
	for key, value := range raw {
		if key != "model" {
			return nil, fmt.Errorf("Claude settings component contains unsupported key %q", key)
		}
		parsed, valid := safeCodexSessionSetting(value)
		if !valid {
			return nil, errors.New("Claude settings component contains an unsafe value")
		}
		values[key] = parsed
	}
	if len(values) == 0 {
		return nil, errors.New("Claude settings component is empty")
	}
	return values, nil
}

func patchClaudeMCPDocument(document map[string]json.RawMessage, component Component, intent claudeMCPIntent, path, agentHome, projectRoot string) ([]byte, error) {
	intentRaw, err := marshalClaudeMCPIntent(intent)
	if err != nil {
		return nil, err
	}
	isLocalProject := component.Scope == "project" && sameFilePath(path, claudeUserConfigPath(agentHome))
	if isLocalProject {
		if strings.TrimSpace(projectRoot) == "" {
			return nil, errors.New("Claude project root is required for local MCP configuration")
		}
		projects, err := mutableClaudeObject(document, "projects")
		if err != nil {
			return nil, err
		}
		projectKey := projectRoot
		for key := range projects {
			if claudeProjectPathEqual(key, projectRoot) {
				projectKey = key
				break
			}
		}
		projectRaw := projects[projectKey]
		project, ok := rawObject(projectRaw)
		if projectRaw != nil && !ok {
			return nil, errors.New("Claude project state is not a JSON object")
		}
		if project == nil {
			project = make(map[string]json.RawMessage)
		}
		servers, err := mutableClaudeObject(project, "mcpServers")
		if err != nil {
			return nil, err
		}
		servers[component.Name] = intentRaw
		project["mcpServers"], err = json.Marshal(servers)
		if err != nil {
			return nil, err
		}
		projects[projectKey], err = json.Marshal(project)
		if err != nil {
			return nil, err
		}
		document["projects"], err = json.Marshal(projects)
		if err != nil {
			return nil, err
		}
	} else {
		servers, err := mutableClaudeObject(document, "mcpServers")
		if err != nil {
			return nil, err
		}
		servers[component.Name] = intentRaw
		document["mcpServers"], err = json.Marshal(servers)
		if err != nil {
			return nil, err
		}
	}
	return marshalClaudeDocument(document)
}

func patchClaudeSettingsDocument(document map[string]json.RawMessage, values map[string]string) ([]byte, error) {
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		document[key] = raw
	}
	return marshalClaudeDocument(document)
}

func marshalClaudeMCPIntent(intent claudeMCPIntent) (json.RawMessage, error) {
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
	return json.Marshal(wire)
}

func mutableClaudeObject(document map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	raw, found := document[key]
	if !found {
		return make(map[string]json.RawMessage), nil
	}
	object, ok := rawObject(raw)
	if !ok {
		return nil, fmt.Errorf("Claude configuration field %q is not a JSON object", key)
	}
	return object, nil
}

func marshalClaudeDocument(document map[string]json.RawMessage) ([]byte, error) {
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sameFilePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	left, _ = filepath.Abs(filepath.Clean(left))
	right, _ = filepath.Abs(filepath.Clean(right))
	if strings.EqualFold(left, right) {
		return true
	}
	return left == right
}
