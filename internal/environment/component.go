package environment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxComponents bounds the number of environment components carried by one
	// session. A session should reference a small set of components; this is not
	// a user-directory backup.
	MaxComponents = 16

	// MaxComponentContentBytes prevents one component from turning the small
	// environment manifest into an arbitrary file transfer.
	MaxComponentContentBytes = 32 << 10

	// MaxTotalComponentContentBytes leaves room in the encrypted manifest for
	// references and component descriptors.
	MaxTotalComponentContentBytes = 48 << 10
)

var ErrInvalidComponent = errors.New("environment: invalid component")

// Component describes a safe, non-sensitive component that was available on
// the source device. Content is intentionally kept in ComponentContent so a
// preview can expose descriptors without accidentally printing the body.
type Component struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	ProjectID   string `json:"projectId,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Portability string `json:"portability"`
	Format      string `json:"format"`
	Size        int    `json:"size"`
}

// ComponentContent is the encrypted form of one component. Content is never
// written to the legacy session metadata object and is never included in a
// list or preview response unless a future explicit apply flow asks for it.
type ComponentContent struct {
	Component Component `json:"component"`
	Content   []byte    `json:"content"`
}

// NewComponentContent normalizes and validates a text component before it is
// allowed into an environment manifest.
func NewComponentContent(kind, name, scope, projectID, portability, format string, content []byte) (ComponentContent, error) {
	normalized, err := normalizeText(content)
	if err != nil {
		return ComponentContent{}, err
	}
	if len(normalized) == 0 || len(normalized) > MaxComponentContentBytes {
		return ComponentContent{}, ErrInvalidComponent
	}
	if containsSensitiveMaterial(normalized) {
		return ComponentContent{}, ErrInvalidComponent
	}
	digest := sha256.Sum256(normalized)
	component := Component{
		Kind:        kind,
		Name:        name,
		Scope:       scope,
		ProjectID:   projectID,
		Fingerprint: hex.EncodeToString(digest[:]),
		Portability: portability,
		Format:      format,
		Size:        len(normalized),
	}
	result := ComponentContent{Component: component, Content: normalized}
	if err := result.Validate(); err != nil {
		return ComponentContent{}, err
	}
	return result, nil
}

func (c Component) Validate() error {
	if !supportedComponentKind(c.Kind) || !validName(c.Name) {
		return ErrInvalidComponent
	}
	if c.Scope != "global" && c.Scope != "project" {
		return ErrInvalidComponent
	}
	if c.Scope == "project" && !validName(c.ProjectID) {
		return ErrInvalidComponent
	}
	if c.Scope == "global" && c.ProjectID != "" {
		return ErrInvalidComponent
	}
	if c.Portability != "portable" && c.Portability != "platform-specific" && c.Portability != "unsupported" {
		return ErrInvalidComponent
	}
	switch c.Kind {
	case "skill":
		if c.Format != "text/markdown" {
			return ErrInvalidComponent
		}
	case "mcp":
		if c.Format != "application/json" {
			return ErrInvalidComponent
		}
	case "settings":
		if !isSessionSettingsName(c.Name) || c.Format != "application/json" {
			return ErrInvalidComponent
		}
	default:
		return ErrInvalidComponent
	}
	if c.Size <= 0 || c.Size > MaxComponentContentBytes {
		return ErrInvalidComponent
	}
	digest, err := hex.DecodeString(c.Fingerprint)
	if err != nil || len(digest) != sha256.Size || len(c.Fingerprint) != sha256.Size*2 {
		return ErrInvalidComponent
	}
	return nil
}

func (c ComponentContent) Validate() error {
	if err := c.Component.Validate(); err != nil {
		return err
	}
	if len(c.Content) != c.Component.Size || len(c.Content) > MaxComponentContentBytes || !utf8.Valid(c.Content) {
		return ErrInvalidComponent
	}
	if containsSensitiveMaterial(c.Content) {
		return ErrInvalidComponent
	}
	digest := sha256.Sum256(c.Content)
	if !strings.EqualFold(c.Component.Fingerprint, hex.EncodeToString(digest[:])) {
		return ErrInvalidComponent
	}
	return nil
}

// NormalizeComponentContents validates, sorts, de-duplicates, and bounds
// component contents. Conflicting bodies for the same scope/name are retained
// until Validate reports the ambiguity instead of silently choosing one.
func NormalizeComponentContents(input []ComponentContent) []ComponentContent {
	components := make([]ComponentContent, 0, len(input))
	for _, component := range input {
		if component.Validate() != nil {
			continue
		}
		components = append(components, ComponentContent{
			Component: component.Component,
			Content:   append([]byte(nil), component.Content...),
		})
	}
	sort.Slice(components, func(i, j int) bool {
		left, right := components[i].Component, components[j].Component
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Scope != right.Scope {
			return left.Scope < right.Scope
		}
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		return left.Fingerprint < right.Fingerprint
	})
	out := components[:0]
	var total int
	for _, component := range components {
		if len(out) > 0 && sameComponent(out[len(out)-1].Component, component.Component) {
			continue
		}
		if len(out) == MaxComponents || total+len(component.Content) > MaxTotalComponentContentBytes {
			break
		}
		out = append(out, component)
		total += len(component.Content)
	}
	return out
}

// ComponentSummaries strips bodies before they cross into list and preview
// output.
func ComponentSummaries(input []ComponentContent) []Component {
	normalized := NormalizeComponentContents(input)
	out := make([]Component, 0, len(normalized))
	for _, component := range normalized {
		out = append(out, component.Component)
	}
	return out
}

// CaptureSkillComponents reads only SKILL.md files for skills that were
// structurally observed in a session. Codex's global skills live below the
// agent home; project skills are supported below .agents/skills and
// .codex/skills. Other files, scripts, settings, and MCP configuration are
// deliberately outside this phase.
func CaptureSkillComponents(agent, agentHome, projectRoot, projectID string, references []Reference) []ComponentContent {
	if agent != "codex" {
		return nil
	}
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

		projectContent, projectFound := readProjectSkillDocument(projectRoot, reference.Name)
		if !projectFound {
			continue
		}
		if globalFound && bytes.Equal(globalContent, projectContent) {
			// The project copy resolves to the same global content; retaining only
			// the global component avoids an unnecessary second body.
			continue
		}
		if component, err := NewComponentContent("skill", reference.Name, "project", projectID, reference.Portability, "text/markdown", projectContent); err == nil {
			captured = append(captured, component)
		}
	}
	return NormalizeComponentContents(captured)
}

func readProjectSkillDocument(projectRoot, name string) ([]byte, bool) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, false
	}
	for _, candidate := range []string{
		filepath.Join(projectRoot, ".agents", "skills", name, "SKILL.md"),
		filepath.Join(projectRoot, ".codex", "skills", name, "SKILL.md"),
	} {
		if content, found := readSkillDocument(projectRoot, candidate); found {
			return content, true
		}
	}
	return nil, false
}

func readSkillDocument(root, path string) ([]byte, bool) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return nil, false
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}
	rootInfo, err := os.Stat(canonicalRoot)
	if err != nil || !rootInfo.IsDir() {
		return nil, false
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil || !pathWithin(canonicalRoot, canonicalPath) {
		return nil, false
	}
	info, err := os.Stat(canonicalPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxComponentContentBytes {
		return nil, false
	}
	file, err := os.Open(canonicalPath)
	if err != nil {
		return nil, false
	}
	content, readErr := io.ReadAll(io.LimitReader(file, MaxComponentContentBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(content) > MaxComponentContentBytes {
		return nil, false
	}
	normalized, err := normalizeText(content)
	if err != nil || len(normalized) == 0 || len(normalized) > MaxComponentContentBytes || containsSensitiveMaterial(normalized) {
		return nil, false
	}
	return normalized, true
}

func normalizeText(content []byte) ([]byte, error) {
	if !utf8.Valid(content) {
		return nil, ErrInvalidComponent
	}
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	content = bytes.ReplaceAll(content, []byte("\r"), []byte("\n"))
	return append([]byte(nil), content...), nil
}

func containsSensitiveMaterial(content []byte) bool {
	upper := strings.ToUpper(string(content))
	for _, marker := range []string{
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		line = strings.TrimLeft(line, "`*_>- \t")
		if strings.HasPrefix(strings.ToLower(line), "export ") {
			line = strings.TrimSpace(line[len("export "):])
		}
		separator := strings.IndexAny(line, ":=")
		if separator <= 0 || separator == len(line)-1 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:separator]))
		key = strings.Trim(key, "`'\"")
		if !sensitiveKey(key) {
			continue
		}
		if strings.TrimSpace(line[separator+1:]) != "" {
			return true
		}
	}
	if json.Valid(content) {
		var value any
		if json.Unmarshal(content, &value) == nil && sensitiveJSONValue(value) {
			return true
		}
	}
	return false
}

func sensitiveKey(key string) bool {
	key = strings.TrimSpace(key)
	compactKey := strings.NewReplacer("_", "", "-", "").Replace(key)
	for _, marker := range []string{
		"token", "secret", "password", "api_key", "api-key", "access_key", "access-key", "private_key", "private-key", "authorization", "cookie",
		"credential", "credentials", "oauth", "auth",
	} {
		compactMarker := strings.NewReplacer("_", "", "-", "").Replace(marker)
		oauthPrefix := compactMarker == "oauth" && strings.HasPrefix(compactKey, compactMarker)
		if key == marker || compactKey == compactMarker || strings.HasSuffix(key, "_"+marker) || strings.HasSuffix(key, "-"+marker) || strings.HasSuffix(compactKey, compactMarker) || oauthPrefix {
			return true
		}
	}
	return false
}

func sensitiveJSONValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if sensitiveKey(key) && jsonValueHasMaterial(child) {
				return true
			}
			if sensitiveJSONValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if sensitiveJSONValue(child) {
				return true
			}
		}
	}
	return false
}

func jsonValueHasMaterial(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) != 0
	case map[string]any:
		return len(typed) != 0
	default:
		return true
	}
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func supportedComponentKind(kind string) bool {
	return kind == "skill" || kind == "mcp" || kind == "settings"
}
func sameComponent(left, right Component) bool {
	return left.Kind == right.Kind && left.Name == right.Name && left.Scope == right.Scope && left.ProjectID == right.ProjectID && left.Fingerprint == right.Fingerprint
}
