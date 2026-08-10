package adapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrCorruptSession reports a record that is fully written but unparseable.
// A truncated tail is not corruption - see ReadRecords.
var ErrCorruptSession = errors.New("adapter: session contains an unparseable record")

// maxTitleLen bounds a generated title so it stays readable in a terminal list.
const maxTitleLen = 72

// SessionData is the result of reading a session that the agent may still be
// appending to.
type SessionData struct {
	// Records holds complete, parseable records in file order.
	Records [][]byte

	// DroppedTail reports that trailing bytes were left out because they were
	// not yet a finished record. It is normal, not an error.
	DroppedTail bool
}

// ReadRecords reads the complete records of a JSONL session.
//
// The agent writes to these files while we read them, so the last record may be
// half written. A record only counts once its terminating newline has landed:
// bytes without one may still be in flight, and shards are immutable once
// pushed, so an incomplete record would be a permanent mistake. Returning one
// record less is always recoverable; returning half a record is not (§9.2).
//
// A malformed record that *is* terminated is different: it was fully written
// and is genuinely corrupt, so it fails loudly rather than being skipped.
func ReadRecords(r io.Reader) (SessionData, error) {
	var out SessionData
	br := bufio.NewReader(r)

	for {
		line, err := br.ReadString('\n')
		terminated := err == nil

		if err != nil && !errors.Is(err, io.EOF) {
			return SessionData{}, fmt.Errorf("read session: %w", err)
		}

		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// Blank lines carry nothing, terminated or not.
		case !terminated:
			// Trailing bytes with no newline: a write in progress.
			out.DroppedTail = true
		case !json.Valid([]byte(trimmed)):
			return SessionData{}, fmt.Errorf("%w: record %d", ErrCorruptSession, len(out.Records)+1)
		default:
			out.Records = append(out.Records, []byte(trimmed))
		}

		if err != nil {
			return out, nil
		}
	}
}

// ReadSessionFile reads a session from disk without disturbing the agent.
//
// The file is opened read-only and never locked, moved or modified: the agent
// owns it and must keep working whether or not we are running (§4 P2, BR-06).
func ReadSessionFile(path string) (SessionData, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionData{}, fmt.Errorf("open session: %w", err)
	}
	defer f.Close()

	data, err := ReadRecords(f)
	if err != nil {
		return SessionData{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return data, nil
}

// Layout locates Claude Code's data directory on this machine.
type Layout struct {
	// Home is the agent's data directory, normally ~/.claude.
	Home string
}

// ProjectsDir is where per-project session directories live.
func (l Layout) ProjectsDir() string {
	return filepath.Join(l.Home, "projects")
}

// SessionDir returns the directory holding the sessions of one project.
func (l Layout) SessionDir(projectRoot string) string {
	return filepath.Join(l.ProjectsDir(), EncodeProjectSlug(projectRoot))
}

// SessionFile returns the path a session with the given native id occupies.
func (l Layout) SessionFile(projectRoot, sessionID string) string {
	return filepath.Join(l.SessionDir(projectRoot), sessionID+".jsonl")
}

// EncodeProjectSlug derives the directory name Claude Code uses for a project
// from its absolute path, by replacing `:` and `\` with `-`.
//
// There is deliberately no decoder. The encoding is lossy - a `-` in a slug may
// have come from a separator, from a drive colon, or from a directory whose
// name really contains one - so reversing it would guess. Callers that need to
// know which project a session belongs to read `cwd` from the session itself,
// which is authoritative; the slug is only ever used to locate files (spec §2).
func EncodeProjectSlug(projectRoot string) string {
	slug := strings.TrimRight(projectRoot, `/\`)
	slug = strings.ReplaceAll(slug, ":", "-")
	slug = strings.ReplaceAll(slug, `\`, "-")
	slug = strings.ReplaceAll(slug, "/", "-")
	return slug
}

// summary is what a scan of a session's records yields.
type summary struct {
	aiTitle     string
	firstPrompt string
	cwd         string
	version     string
	created     time.Time
	updated     time.Time
}

// Summarize extracts listing metadata from a session's records.
//
// Individual records that do not parse are skipped rather than failing the
// whole scan: discovery must keep working when one record is odd, and the
// stricter check belongs in ReadRecords (spec §4.2).
func summarize(records [][]byte) summary {
	var s summary

	for _, raw := range records {
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}

		if v, ok := rec["cwd"].(string); ok && s.cwd == "" {
			s.cwd = v
		}
		if v, ok := rec["version"].(string); ok {
			s.version = v
		}
		if ts, ok := rec["timestamp"].(string); ok {
			s.observeTime(ts)
		}

		switch rec["type"] {
		case "ai-title":
			// Later titles supersede earlier ones: the agent refines them as
			// the conversation develops.
			if v, ok := rec["aiTitle"].(string); ok && v != "" {
				s.aiTitle = v
			}
		case "user":
			if s.firstPrompt == "" && rec["isMeta"] != true {
				s.firstPrompt = promptText(rec)
			}
		}
	}
	return s
}

func (s *summary) observeTime(value string) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return
	}
	if s.created.IsZero() || t.Before(s.created) {
		s.created = t
	}
	if t.After(s.updated) {
		s.updated = t
	}
}

// promptText pulls the user's words out of a message, which may carry either a
// plain string or a list of typed blocks.
func promptText(rec map[string]any) string {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return ""
	}

	switch content := msg["content"].(type) {
	case string:
		return content
	case []any:
		for _, raw := range content {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

// Title derives a display name for a session.
//
// Agents do not name sessions, so this is synthesised locally (§9.2.1). A name
// the user set themselves wins, but that is held above this layer; here the
// order is the agent's own title, then the opening prompt, then the project and
// time as a last resort.
func (s summary) Title(projectRoot string, fallback time.Time) string {
	if t := clean(s.aiTitle); t != "" {
		return t
	}
	if t := clean(s.firstPrompt); t != "" {
		return t
	}

	when := s.created
	if when.IsZero() {
		when = fallback
	}
	name := filepath.Base(strings.TrimRight(projectRoot, `/\`))
	return fmt.Sprintf("%s %s", name, when.Format("2006-01-02 15:04"))
}

// clean collapses whitespace and truncates on a rune boundary, so a multi-line
// prompt does not break a single-line listing.
func clean(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxTitleLen {
		return s
	}

	runes := []rune(s)
	limit := maxTitleLen - 1
	if len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// DiscoverSessions lists the sessions Claude Code holds for one project.
//
// A session that cannot be read at all is skipped rather than failing the scan:
// one damaged file must not hide every other session from the user.
func (l Layout) DiscoverSessions(projectRoot string) ([]SessionRef, error) {
	dir := l.SessionDir(projectRoot)

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		// No directory means no sessions for this project, which is normal.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	var refs []SessionRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		ref, ok := l.describe(dir, entry.Name(), projectRoot)
		if !ok {
			continue
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// describe builds a SessionRef, reporting false when the file cannot be used.
func (l Layout) describe(dir, name, projectRoot string) (SessionRef, bool) {
	path := filepath.Join(dir, name)

	info, err := os.Stat(path)
	if err != nil {
		return SessionRef{}, false
	}

	data, err := ReadSessionFile(path)
	if err != nil {
		return SessionRef{}, false
	}

	s := summarize(data.Records)
	created, updated := s.created, s.updated
	if created.IsZero() {
		created = info.ModTime()
	}
	if updated.IsZero() {
		updated = info.ModTime()
	}

	return SessionRef{
		NativeID:    strings.TrimSuffix(name, ".jsonl"),
		ProjectPath: projectRoot,
		Title:       s.Title(projectRoot, info.ModTime()),
		CreatedAt:   created,
		UpdatedAt:   updated,
		Size:        info.Size(),
	}, true
}
