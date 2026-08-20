package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Tokens standing in for machine-specific path prefixes in canonical records.
//
// The remote stores canonical records rather than device-local bytes. Session
// records embed absolute paths, so the same logical record has different bytes
// on every machine; storing local bytes would make the prefix comparison behind
// fast-forward vs fork always report divergence, and the version model would
// never work (§9.6, spec §3).
const (
	TokenProject   = "${AS_PROJECT}"
	TokenAgentHome = "${AS_AGENT_HOME}"
)

// PathSpace describes one machine's view of the paths a session refers to.
type PathSpace struct {
	// ProjectRoot is the absolute path of the project root.
	ProjectRoot string
	// AgentHome is the absolute path of the agent's data directory.
	AgentHome string
}

// pathValueFields are common leaf field names whose values, or array elements,
// are paths. They are hints for fields that can contain paths with spaces;
// unknown leaf values use the structural bare-path fallback below.
//
// Every one of these is tried against both roots rather than being tied to one:
// PoC-1 found `file_path` holding paths under the agent's own directory, not
// just under the project.
var pathValueFields = map[string]bool{
	"cwd":             true,
	"workspace_roots": true,
	"file_path":       true,
	"filePath":        true,
	"fileName":        true,
	"fileNames":       true,
	"filename":        true,
	"filenames":       true,
	"planFilePath":    true,
	"path":            true,
	"trackingPath":    true,
	"realParentDir":   true,
}

// pathValuePaths contains exact nested paths whose leaf name is too generic
// to add to pathValueFields. Claude Code 2.1.228 can emit a structured path in
// message.content.content. The array form, message.content[].content, remains
// conversation text and is deliberately not included here.
var pathValuePaths = map[string]bool{
	"message.content.content": true,
}

// embeddedJSONPaths are Codex tool argument fields. Their values are JSON
// strings rather than objects, so normal structural traversal cannot see the
// nested file_path/path fields. Invalid JSON is left untouched because it may
// be an ordinary command or user-provided text.
var embeddedJSONPaths = map[string]bool{
	"payload.arguments":      true,
	"payload.input":          true,
	"payload.item.arguments": true,
	"payload.item.input":     true,
}

// pathKeyedContainers are objects whose *keys* are absolute paths. Rewriting
// only values silently misses these, which is exactly the kind of omission that
// produces a session the agent cannot resolve (spec §4.5).
var pathKeyedContainers = map[string]bool{
	"trackedFileBackups": true,
}

// pathKeyedPaths contains agent-specific containers whose keys are paths but
// whose leaf name is too generic to enable globally. Codex stores file change
// snapshots under payload.item.changes/<absolute-path>.
var pathKeyedPaths = map[string]bool{
	"payload.item.changes": true,
}

// Canonicalizer converts a machine's session records into canonical form.
//
// It is stateful only to accumulate diagnostics across the records of one
// session; the conversion itself is independent per record.
type Canonicalizer struct {
	space   PathSpace
	unknown map[string]bool
}

// NewCanonicalizer returns a Canonicalizer for records written on space.
func NewCanonicalizer(space PathSpace) *Canonicalizer {
	return &Canonicalizer{space: space, unknown: map[string]bool{}}
}

// Record converts one raw JSONL record into its canonical form.
//
// The result is byte-for-byte identical on every machine for the same logical
// record, which is what makes prefix comparison meaningful.
func (c *Canonicalizer) Record(raw []byte) ([]byte, error) {
	v, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return encode(c.walk("", v))
}

// UnknownPathFields returns safe field names for path-bearing object keys.
// Unknown leaf values use the structural fallback instead.
//
// Unknown object keys remain conservative because their semantics are ambiguous,
// and changing arbitrary user content would be unsafe. The
// caller downgrades compatibility when an unresolved path key is present.
func (c *Canonicalizer) UnknownPathFields() []string {
	out := make([]string, 0, len(c.unknown))
	for k := range c.unknown {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isPathKeyedContainer(path string) bool {
	return pathKeyedContainers[fieldLeaf(path)] || pathKeyedPaths[path]
}

// walk returns a copy of v with known path prefixes replaced by tokens. path
// is the dotted map path that led to v, or "" at the top level.
func (c *Canonicalizer) walk(path string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[c.key(path, k)] = c.walk(joinFieldPath(path, k), child)
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			// Keep an array marker for exact schema paths, while fieldLeaf
			// still lets common leaf fields inherit their array's field name.
			out[i] = c.walk(path+"[]", child)
		}
		return out

	case string:
		return c.text(path, t)

	default:
		return v
	}
}

// key tokenizes a map key when the enclosing field is a path-keyed container.
func (c *Canonicalizer) key(parentPath, k string) string {
	if isPathKeyedContainer(parentPath) {
		if out, ok := c.tokenize(k); ok {
			return out
		}
		return k
	}
	if isBarePath(k) {
		if _, ok := c.tokenize(k); ok {
			c.report(joinFieldPath(parentPath, "<key>"))
		}
	}
	return k
}

// text tokenizes a string value when its field is allowlisted. For unknown
// fields, an exact bare value under one of the current machine's roots is also
// rewritten. This structural fallback is what keeps ordinary Claude Code
// minor releases from requiring a new field-specific adapter release.
func (c *Canonicalizer) text(path, s string) string {
	if embeddedJSONPaths[path] {
		if rewritten, ok := c.rewriteEmbeddedJSON(path, s); ok {
			return rewritten
		}
	}
	if isPathValuePath(path) {
		if out, ok := c.tokenize(s); ok {
			return out
		}
		// A path outside both roots. Left verbatim rather than guessed at; it
		// may be invalid on the target machine, which is a known trade-off
		// inherited from PoC-1.
		return s
	}

	if isBarePath(s) {
		if out, ok := c.tokenize(s); ok {
			return out
		}
	}
	return s
}

func (c *Canonicalizer) rewriteEmbeddedJSON(path, value string) (string, bool) {
	decoded, err := decode([]byte(value))
	if err != nil {
		return value, false
	}
	rewritten := c.walk(path+".<json>", decoded)
	encoded, err := encode(rewritten)
	if err != nil {
		return value, false
	}
	return string(encoded), true
}

func isPathValuePath(path string) bool {
	return pathValueFields[fieldLeaf(path)] || pathValuePaths[path]
}

func fieldLeaf(path string) string {
	for strings.HasSuffix(path, "[]") {
		path = strings.TrimSuffix(path, "[]")
	}
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func joinFieldPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// report records a schema finding, but only under a name that is safe to show.
//
// Findings surface in `agentsync doctor`, whose output must be safe to paste
// into a public issue - no paths, project names or session content (BR-09).
// Keys of an unknown path-keyed container are themselves absolute paths, so a
// name that is not a plain identifier is replaced rather than leaked.
func (c *Canonicalizer) report(field string) {
	if !isFieldName(field) {
		field = "<redacted>"
	}
	c.unknown[field] = true
}

func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '[' || r == ']' || r == '<' || r == '>':
		default:
			return false
		}
	}
	return true
}

// isBarePath reports whether a value is plausibly a path and nothing else.
//
// Prose can begin with a path - a pasted filename followed by a question,
// a shell command in a tool argument - and rewriting such prose would change
// the session's meaning. Unknown fields therefore use a conservative shape
// check; named path fields still support spaces and non-ASCII paths.
func isBarePath(s string) bool {
	return s != "" && !strings.ContainsAny(s, " \t\r\n")
}

// isCanonicalPath reports whether s has the shape produced by tokenize for an
// unknown leaf field. A canonical path is either the token itself or the token
// followed by a slash-separated remainder.
func isCanonicalPath(s string) bool {
	if !isBarePath(s) {
		return false
	}
	for _, token := range []string{TokenProject, TokenAgentHome} {
		if s == token || strings.HasPrefix(s, token+"/") || strings.HasPrefix(s, token+"\\") {
			return true
		}
	}
	return false
}

// tokenize replaces a leading path prefix with its token. The longer root is
// tried first so that a nested root cannot shadow the more specific one.
func (c *Canonicalizer) tokenize(s string) (string, bool) {
	type candidate struct {
		root  string
		token string
	}
	candidates := []candidate{
		{c.space.ProjectRoot, TokenProject},
		{c.space.AgentHome, TokenAgentHome},
	}
	if len(candidates[1].root) > len(candidates[0].root) {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}

	for _, cand := range candidates {
		if out, ok := replaceRoot(s, cand.root, cand.token); ok {
			return out, true
		}
	}
	return s, false
}

// replaceRoot swaps a leading root for a token, normalising the remainder to
// forward slashes.
func replaceRoot(s, root, token string) (string, bool) {
	root = strings.TrimRight(root, `/\`)
	if root == "" || len(s) < len(root) {
		return s, false
	}
	if !samePathPrefix(s[:len(root)], root) {
		return s, false
	}

	rest := s[len(root):]
	if rest == "" {
		return token, true
	}
	// Require a separator at the boundary so that "D:\Proj" does not match
	// "D:\Project2\x".
	if rest[0] != '/' && rest[0] != '\\' {
		return s, false
	}
	return token + strings.ReplaceAll(rest, `\`, "/"), true
}

// Localize converts a canonical record into bytes for the machine described by
// space. It fails rather than emitting a partially substituted record: a
// session that still refers to another machine's paths must never reach the
// agent's data directory (BR-10).
func Localize(raw []byte, space PathSpace) ([]byte, error) {
	// Refuse before touching the record rather than only when a token turns up.
	// Localising against an unknown project root cannot produce a session the
	// agent will resolve, so there is nothing useful to return (spec §5).
	if space.ProjectRoot == "" {
		return nil, errors.New("localize: no project root configured")
	}

	v, err := decode(raw)
	if err != nil {
		return nil, err
	}
	l := &localizer{space: space}
	out, err := l.walk("", v)
	if err != nil {
		return nil, err
	}
	return encode(out)
}

type localizer struct {
	space PathSpace
}

func (l *localizer) walk(path string, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			key, err := l.text(path, k, isPathKeyedContainer(path))
			if err != nil {
				return nil, err
			}
			value, err := l.walk(joinFieldPath(path, k), child)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil

	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			value, err := l.walk(path+"[]", child)
			if err != nil {
				return nil, err
			}
			out[i] = value
		}
		return out, nil

	case string:
		return l.text(path, t, isPathValuePath(path) || isCanonicalPath(t))

	default:
		return v, nil
	}
}

// text substitutes a leading token. tokenized reports whether this position is
// one the canonicaliser would have written a token into.
//
// A token elsewhere is left alone rather than rejected. We only ever write
// tokens into allowlisted positions, so the same characters appearing in a
// message body or a command line are user content that happens to look like
// our marker - and refusing there would make any session that discusses
// AgentSync itself impossible to restore.
func (l *localizer) text(field, s string, tokenized bool) (string, error) {
	if embeddedJSONPaths[field] {
		if rewritten, ok, err := l.rewriteEmbeddedJSON(field, s); err != nil {
			return "", err
		} else if ok {
			return rewritten, nil
		}
	}
	if !tokenized {
		return s, nil
	}

	for _, tok := range []struct {
		token string
		root  string
	}{
		{TokenProject, l.space.ProjectRoot},
		{TokenAgentHome, l.space.AgentHome},
	} {
		if !strings.Contains(s, tok.token) {
			continue
		}
		if !strings.HasPrefix(s, tok.token) {
			// In a field we do tokenize, a marker anywhere but the start means
			// the record did not come from our canonicaliser. Guessing what it
			// meant is worse than refusing to write it.
			return "", fmt.Errorf("localize: %s not at start of value in field %q", tok.token, field)
		}
		if tok.root == "" {
			return "", fmt.Errorf("localize: %s present but no target path configured", tok.token)
		}

		rest := strings.TrimPrefix(s, tok.token)
		if rest == "" {
			return tok.root, nil
		}
		// The separator follows the root being substituted, not the project's.
		// An agent directory can sit on a different-looking path from the
		// project, and mixing the two produces `C:\Users\x\.claude/backups`.
		return tok.root + strings.ReplaceAll(rest, "/", separatorFor(tok.root)), nil
	}
	return s, nil
}

func (l *localizer) rewriteEmbeddedJSON(path, value string) (string, bool, error) {
	decoded, err := decode([]byte(value))
	if err != nil {
		return value, false, nil
	}
	rewritten, err := l.walk(path+".<json>", decoded)
	if err != nil {
		return "", false, err
	}
	encoded, err := encode(rewritten)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

// samePathPrefix compares two path fragments of equal length under the
// conventions of the platform the root belongs to.
//
// Windows treats paths case-insensitively and accepts either separator, and
// sessions do record mixed spellings - `D:/a/b` and `D:\a\b` denote the same
// directory, so a comparison that misses that leaves the path untokenized and
// the restore broken. POSIX gets an exact comparison instead: there `\` is an
// ordinary filename character, and folding case would risk matching a genuinely
// different directory on case-sensitive filesystems.
func samePathPrefix(a, b string) bool {
	if separatorFor(b) != `\` {
		return a == b
	}
	return strings.EqualFold(toSlash(a), toSlash(b))
}

func toSlash(s string) string {
	return strings.ReplaceAll(s, `\`, "/")
}

// separatorFor infers the path separator of a machine from its root, so that a
// record can be localised for a platform other than the one running the code -
// which the cross-platform tests depend on.
func separatorFor(root string) string {
	if len(root) >= 2 && root[1] == ':' {
		return `\`
	}
	if strings.HasPrefix(root, `\\`) {
		return `\`
	}
	if !strings.Contains(root, "/") && strings.Contains(root, `\`) {
		return `\`
	}
	return "/"
}

// decode parses a record, preserving numeric literals exactly. Decoding numbers
// into float64 would rewrite large integers into exponent form and could lose
// precision, which changes the canonical bytes and breaks prefix comparison.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse record: %w", err)
	}
	return v, nil
}

// encode serialises a record deterministically: object keys sorted (the
// encoding/json behaviour for maps), no HTML escaping, no trailing newline.
// Every machine must produce identical bytes for the same logical record.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
