package environment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

const (
	ComponentStateUnchanged   = "unchanged"
	ComponentStateMissing     = "missing"
	ComponentStateChanged     = "changed"
	ComponentStateUnavailable = "unavailable"
	ComponentStateManual      = "manual"
	ComponentStateConflict    = "conflict"
	ComponentStateApplied     = "applied"
	ComponentStateFailed      = "failed"
)

var ErrUnsupportedComponentApply = errors.New("environment: component has no safe file apply target")
var ErrConfigConflict = errors.New("environment: local config has a conflicting override")

// LocalComponentState describes what the target device currently has for one
// component. It contains local paths for the user's preview only; paths are
// never written into the encrypted environment manifest.
type LocalComponentState struct {
	Path   string
	State  string
	Backup string
	Reason string
}

// InspectComponent compares a filtered component descriptor with the local
// target. It never reads or returns raw configuration values; MCP and settings
// are compared through their allowlisted values only.
func InspectComponent(component Component, agent, agentHome, projectRoot string) LocalComponentState {
	if err := component.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateUnavailable, Reason: "remote component metadata is invalid"}
	}
	if agent != "codex" {
		return LocalComponentState{
			State:  ComponentStateManual,
			Reason: "only Codex environment components can be inspected locally",
		}
	}
	switch component.Kind {
	case "mcp":
		return inspectMCPComponent(component, agentHome, projectRoot)
	case "settings":
		return inspectCodexSettingsComponent(component, agentHome, projectRoot)
	case "skill":
		// Skills continue through the file-target path below.
	default:
		return LocalComponentState{
			State:  ComponentStateManual,
			Reason: "component kind has no local inspection target",
		}
	}
	path, err := ComponentPath(component, agent, agentHome, projectRoot)
	if err != nil {
		state := ComponentStateUnavailable
		if !errors.Is(err, os.ErrNotExist) {
			state = ComponentStateFailed
		}
		return LocalComponentState{Path: path, State: state, Reason: err.Error()}
	}
	result := LocalComponentState{Path: path}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		result.State = ComponentStateMissing
		return result
	case err != nil:
		result.State = ComponentStateUnavailable
		result.Reason = err.Error()
		return result
	case info.IsDir():
		result.State = ComponentStateFailed
		result.Reason = "target path is a directory"
		return result
	}
	root, err := componentRoot(component, agentHome, projectRoot)
	if err != nil {
		result.State = ComponentStateUnavailable
		result.Reason = err.Error()
		return result
	}
	content, found := readSkillDocument(root, path)
	if !found {
		result.State = ComponentStateFailed
		result.Reason = "existing Skill file is unreadable or contains sensitive material"
		return result
	}
	local, err := NewComponentContent("skill", component.Name, component.Scope, component.ProjectID, component.Portability, "text/markdown", content)
	if err != nil {
		result.State = ComponentStateFailed
		result.Reason = "existing Skill file is not a safe filtered text component"
		return result
	}
	if strings.EqualFold(local.Component.Fingerprint, component.Fingerprint) {
		result.State = ComponentStateUnchanged
	} else {
		result.State = ComponentStateChanged
	}
	return result
}

// ApplyComponent writes one filtered Codex Skill body after the caller has
// obtained explicit confirmation. Existing files are backed up before the
// atomic replacement. Unsupported components are reported as manual and do
// not cause any config, command or process side effect.
func ApplyConfigComponent(content ComponentContent, agent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	if err := content.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateFailed, Reason: "remote component content is invalid"}, err
	}
	if agent != "codex" || (content.Component.Kind != "mcp" && content.Component.Kind != "settings") {
		return LocalComponentState{State: ComponentStateManual, Reason: "only filtered Codex MCP and settings components have a safe configuration target"}, nil
	}
	return applyFilteredConfigComponent(content, agentHome, projectRoot, backupRoot)
}

func ApplyComponent(content ComponentContent, agent, agentHome, projectRoot, backupRoot string) (LocalComponentState, error) {
	if err := content.Validate(); err != nil {
		return LocalComponentState{State: ComponentStateFailed, Reason: "remote component content is invalid"}, err
	}
	if agent != "codex" {
		return LocalComponentState{State: ComponentStateManual, Reason: "only Codex environment components have a safe automatic file target"}, nil
	}
	if content.Component.Kind != "skill" {
		return LocalComponentState{State: ComponentStateManual, Reason: "only filtered Codex Skill files have a safe automatic file target"}, nil
	}
	state := InspectComponent(content.Component, agent, agentHome, projectRoot)
	if state.State == ComponentStateUnchanged {
		return state, nil
	}
	if state.State != ComponentStateMissing && state.State != ComponentStateChanged {
		return state, fmt.Errorf("%w: %s", ErrUnsupportedComponentApply, state.Reason)
	}

	var existing []byte
	if state.State == ComponentStateChanged {
		var err error
		existing, err = os.ReadFile(state.Path)
		if err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("read existing Skill file: %v", err)
			return state, err
		}
	}
	if strings.TrimSpace(backupRoot) == "" && state.State == ComponentStateChanged {
		state.State = ComponentStateFailed
		state.Reason = "backup directory is required before replacing a file"
		return state, errors.New(state.Reason)
	}
	if state.State == ComponentStateChanged {
		if err := os.MkdirAll(backupRoot, 0o700); err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("create backup directory: %v", err)
			return state, err
		}
		backupName := backupFileName(content.Component)
		state.Backup = filepath.Join(backupRoot, backupName)
		if err := atomicfile.WriteBytes(state.Backup, existing); err != nil {
			state.State = ComponentStateFailed
			state.Reason = fmt.Sprintf("write backup: %v", err)
			return state, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(state.Path), 0o700); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("create Skill directory: %v", err)
		return state, err
	}
	if err := atomicfile.WriteBytes(state.Path, content.Content); err != nil {
		state.State = ComponentStateFailed
		state.Reason = fmt.Sprintf("write Skill file: %v", err)
		return state, err
	}
	state.State = ComponentStateApplied
	state.Reason = ""
	return state, nil
}

func backupFileName(component Component) string {
	descriptor := strings.Join([]string{component.Kind, component.Name, component.Scope, component.ProjectID, component.Fingerprint}, "\x00")
	digest := sha256.Sum256([]byte(descriptor))
	return component.Scope + "-" + hex.EncodeToString(digest[:8]) + "-" + component.Fingerprint + ".bak"
}

// ComponentPath returns the local target for a supported file component. It
// validates lexical and resolved containment so a component name or symlink
// cannot redirect a write outside the selected agent/project root.
func ComponentPath(component Component, agent, agentHome, projectRoot string) (string, error) {
	if err := component.Validate(); err != nil {
		return "", err
	}
	if component.Kind != "skill" || agent != "codex" {
		return "", ErrUnsupportedComponentApply
	}
	root, err := componentRoot(component, agentHome, projectRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("component root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("component root is not a directory")
	}
	var path string
	switch component.Scope {
	case "global":
		path = filepath.Join(root, "skills", component.Name, "SKILL.md")
	case "project":
		path = projectSkillTarget(root, component.Name)
	default:
		return "", ErrUnsupportedComponentApply
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve component root: %w", err)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve component path: %w", err)
	}
	if !pathWithin(absoluteRoot, absolutePath) {
		return "", errors.New("component path escapes its root")
	}
	if err := resolvedPathWithin(absoluteRoot, absolutePath); err != nil {
		return "", err
	}
	return absolutePath, nil
}

func componentRoot(component Component, agentHome, projectRoot string) (string, error) {
	if component.Scope == "global" {
		if strings.TrimSpace(agentHome) == "" {
			return "", fmt.Errorf("global Codex home is unavailable: %w", os.ErrNotExist)
		}
		return filepath.Clean(agentHome), nil
	}
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("project root is unavailable: %w", os.ErrNotExist)
	}
	return filepath.Clean(projectRoot), nil
}

func projectSkillTarget(root, name string) string {
	candidates := []string{
		filepath.Join(root, ".agents", "skills", name, "SKILL.md"),
		filepath.Join(root, ".codex", "skills", name, "SKILL.md"),
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(filepath.Dir(candidate)); err == nil && info.IsDir() {
			return candidate
		}
	}
	return candidates[0]
}

func resolvedPathWithin(root, target string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve component root: %w", err)
	}
	current := target
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			canonicalCurrent, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return fmt.Errorf("resolve component path: %w", resolveErr)
			}
			if !pathWithin(canonicalRoot, canonicalCurrent) {
				return errors.New("component path follows a symlink outside its root")
			}
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect component path: %w", statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("component path has no existing safe parent")
		}
		current = parent
	}
}
