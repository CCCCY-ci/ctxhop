package adapter

import (
	"bytes"
	"encoding/json"
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

// pathValueFields are leaf field names whose value is a single absolute path.
//
// Every one of these is tried against both roots rather than being tied to one:
// PoC-1 found `file_path` holding paths under the agent's own directory, not
// just under the project.
var pathValueFields = map[string]bool{
	"cwd":           true,
	"file_path":     true,
	"filePath":      true,
	"planFilePath":  true,
	"trackingPath":  true,
	"realParentDir": true,
}

// pathKeyedContainers are objects whose *keys* are absolute paths. Rewriting
// only values silently misses these, which is exactly the kind of omission that
// produces a session the agent cannot resolve (spec §4.5).
var pathKeyedContainers = map[string]bool{
	"trackedFileBackups": true,
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

// UnknownPathFields returns field names, sorted, that held one of this
// machine's known path prefixes but are not in the allowlist.
//
// Such a field means the agent gained a path-bearing field we do not rewrite,
// so restoring would produce a session pointing at the source machine. The
// caller downgrades compatibility rather than writing anything (spec §4.8).
func (c *Canonicalizer) UnknownPathFields() []string {
	out := make([]string, 0, len(c.unknown))
	for k := range c.unknown {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// walk returns a copy of v with known path prefixes replaced by tokens. field
// is the map key that led to v, or "" at the top level.
func (c *Canonicalizer) walk(field string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[c.key(field, k)] = c.walk(k, child)
		}
		return out

	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			// Array elements inherit the field name that led to the array, so
			// that content[].input.file_path is still seen as file_path.
			out[i] = c.walk(field, child)
		}
		return out

	case string:
		return c.text(field, t)

	default:
		return v
	}
}

// key tokenizes a map key when the enclosing field is a path-keyed container.
func (c *Canonicalizer) key(parentField, k string) string {
	if pathKeyedContainers[parentField] {
		if out, ok := c.tokenize(k); ok {
			return out
		}
		return k
	}
	if _, ok := c.tokenize(k); ok {
		c.unknown[parentField+".<key>"] = true
	}
	return k
}

// text tokenizes a string value when its field is allowlisted, and otherwise
// records it as a finding if it nonetheless carries one of our path prefixes.
func (c *Canonicalizer) text(field, s string) string {
	if pathValueFields[field] {
		if out, ok := c.tokenize(s); ok {
			return out
		}
		// A path outside both roots. Left verbatim rather than guessed at; it
		// may be invalid on the target machine, which is a known trade-off
		// inherited from PoC-1.
		return s
	}

	// Only a value that *starts with* one of our roots is reported. Prose and
	// terminal output quote paths mid-string constantly, and flagging those
	// would downgrade compatibility on every session.
	if _, ok := c.tokenize(s); ok {
		c.unknown[field] = true
	}
	return s
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
	v, err := decode(raw)
	if err != nil {
		return nil, err
	}
	l := &localizer{space: space, sep: separatorFor(space.ProjectRoot)}
	out, err := l.walk("", v)
	if err != nil {
		return nil, err
	}
	return encode(out)
}

type localizer struct {
	space PathSpace
	sep   string
}

func (l *localizer) walk(field string, v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			key, err := l.text(field, k, pathKeyedContainers[field])
			if err != nil {
				return nil, err
			}
			value, err := l.walk(k, child)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil

	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			value, err := l.walk(field, child)
			if err != nil {
				return nil, err
			}
			out[i] = value
		}
		return out, nil

	case string:
		return l.text(field, t, pathValueFields[field])

	default:
		return v, nil
	}
}

// text substitutes a leading token. allowed reports whether this position is
// one where a token is expected at all.
func (l *localizer) text(field, s string, allowed bool) (string, error) {
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
		if !allowed {
			return "", fmt.Errorf("localize: %s in unexpected field %q", tok.token, field)
		}
		if !strings.HasPrefix(s, tok.token) {
			// A token anywhere but the start means the record was not produced
			// by our canonicaliser, or user content collided with the literal
			// token. Either way, guessing is worse than refusing.
			return "", fmt.Errorf("localize: %s not at start of value in field %q", tok.token, field)
		}
		if tok.root == "" {
			return "", fmt.Errorf("localize: %s present but no target path configured", tok.token)
		}

		rest := strings.TrimPrefix(s, tok.token)
		if rest == "" {
			return tok.root, nil
		}
		return tok.root + strings.ReplaceAll(rest, "/", l.sep), nil
	}
	return s, nil
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
