package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/environment"
)

// CodexLayout locates the JSONL sessions written by Codex CLI.
//
// Codex stores sessions globally under CODEX_HOME/sessions/YYYY/MM/DD rather
// than in one directory per project. The session_meta.cwd and
// turn_context.workspace_roots fields are therefore the source of truth when
// a project is being discovered.
type CodexLayout struct {
	// Home is normally ~/.codex or the directory selected by CODEX_HOME.
	Home string

	// version overrides version discovery in tests. It never controls
	// compatibility; observed fields do.
	version func(context.Context) string
}

// DefaultCodexHome returns the Codex state directory for this machine.
func DefaultCodexHome() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CODEX_HOME")); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve CODEX_HOME: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// Name identifies the Codex adapter in configuration and session metadata.
func (l CodexLayout) Name() string {
	return "codex"
}

// Environment returns the Codex-specific filtered environment capability.
// Core invokes it through adapter.EnvironmentFor and never selects it by
// comparing an Agent name.
func (l CodexLayout) Environment() environment.Provider {
	return codexEnvironmentProvider{}
}

// SessionsDir is the root of Codex's dated JSONL session tree.
func (l CodexLayout) SessionsDir() string {
	return filepath.Join(l.Home, "sessions")
}

// Detect locates Codex state without starting the Codex executable.
func (l CodexLayout) Detect(ctx context.Context) (Installation, error) {
	if ctx == nil {
		return Installation{}, errors.New("adapter: context is required")
	}
	if l.Home == "" {
		return Installation{}, errors.New("adapter: no Codex home configured")
	}
	info, err := os.Stat(l.Home)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Installation{}, ErrNotInstalled
	case err != nil:
		return Installation{}, fmt.Errorf("inspect Codex directory: %w", err)
	case !info.IsDir():
		return Installation{}, errors.New("adapter: Codex data path is not a directory")
	}

	lookup := l.version
	if lookup == nil {
		lookup = l.versionFromNewestSession
	}
	level, reason := compatibilityBaseline()
	return Installation{
		Version:             lookup(ctx),
		DataDir:             l.Home,
		Compatibility:       level,
		CompatibilityReason: reason,
	}, nil
}

func (l CodexLayout) versionFromNewestSession(ctx context.Context) string {
	path, err := l.newestSessionFile(ctx)
	if err != nil || path == "" {
		return ""
	}
	summary, ok := readCodexSummary(path)
	if !ok {
		return ""
	}
	return summary.version
}

func (l CodexLayout) newestSessionFile(ctx context.Context) (string, error) {
	var newest string
	var newestTime int64
	err := filepath.WalkDir(l.SessionsDir(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !isCodexSessionName(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if stamp := info.ModTime().UnixNano(); stamp > newestTime {
			newestTime = stamp
			newest = path
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return newest, err
}

// DiscoverSessions lists Codex sessions belonging to projectRoot. It reads
// only one JSON object at a time, which is important because Codex can retain
// very large histories and discovery should not load every session into RAM.
func (l CodexLayout) DiscoverSessions(projectRoot string) ([]SessionRef, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, errors.New("list sessions: project root is empty")
	}
	root := l.SessionsDir()
	if info, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	} else if !info.IsDir() {
		return nil, errors.New("list sessions: Codex sessions path is not a directory")
	}

	var refs []SessionRef
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isCodexSessionName(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		summary, ok := readCodexSummary(path)
		if !ok || summary.cwd == "" || !sameProject(summary.cwd, projectRoot) {
			return nil
		}
		if summary.nativeID == "" {
			summary.nativeID = codexIDFromName(entry.Name())
		}
		if summary.nativeID == "" {
			return nil
		}
		created, updated := summary.created, summary.updated
		if created.IsZero() {
			created = info.ModTime()
		}
		if updated.IsZero() || info.ModTime().After(updated) {
			updated = info.ModTime()
		}
		refs = append(refs, SessionRef{
			Agent:       l.Name(),
			NativeID:    summary.nativeID,
			ProjectPath: projectRoot,
			Title:       summary.Title(projectRoot, info.ModTime()),
			CreatedAt:   created,
			UpdatedAt:   updated,
			Size:        info.Size(),
			localPath:   path,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].UpdatedAt.Equal(refs[j].UpdatedAt) {
			return refs[i].NativeID < refs[j].NativeID
		}
		return refs[i].UpdatedAt.After(refs[j].UpdatedAt)
	})
	return refs, nil
}

// ReadSession reads the complete Codex JSONL snapshot identified by ref.
func (l CodexLayout) ReadSession(ref SessionRef) (SessionData, error) {
	path := ref.localPath
	if path == "" {
		var err error
		path, err = l.findSessionPath(ref.NativeID)
		if err != nil {
			return SessionData{}, err
		}
	}
	return ReadSessionFile(path)
}

// WriteSession installs a new Codex session without replacing an existing id.
func (l CodexLayout) WriteSession(projectRoot, sessionID string, records [][]byte) error {
	return l.writeCodexSession(projectRoot, sessionID, records, false)
}

// ReplaceSession installs a Codex session over the existing native id.
func (l CodexLayout) ReplaceSession(projectRoot, sessionID string, records [][]byte) error {
	return l.writeCodexSession(projectRoot, sessionID, records, true)
}

func (l CodexLayout) writeCodexSession(projectRoot, sessionID string, records [][]byte, replace bool) error {
	if err := checkSessionID(sessionID); err != nil {
		return err
	}
	if len(records) == 0 {
		return errors.New("adapter: refusing to write a session with no records")
	}
	for i, record := range records {
		if len(record) == 0 || strings.ContainsAny(string(record), "\r\n") || !json.Valid(record) {
			return fmt.Errorf("%w: record %d", ErrInvalidRecord, i+1)
		}
	}

	existing, err := l.findSessionPath(sessionID)
	if err != nil {
		return err
	}
	if existing != "" && !replace {
		return fmt.Errorf("%w: %s", ErrSessionExists, sessionID)
	}

	summary := summarizeCodexRecords(records)
	when := summary.created
	if when.IsZero() {
		when = time.Now().UTC()
	}
	path := existing
	if path == "" {
		stamp := when.UTC().Format("2006-01-02T15-04-05")
		path = filepath.Join(l.SessionsDir(), when.UTC().Format("2006"), when.UTC().Format("01"), when.UTC().Format("02"), "rollout-"+stamp+"-"+sessionID+".jsonl")
	}
	if err := writeSessionAt(path, records); err != nil {
		return fmt.Errorf("write Codex session: %w", err)
	}
	return nil
}

func (l CodexLayout) findSessionPath(sessionID string) (string, error) {
	if err := checkSessionID(sessionID); err != nil {
		return "", err
	}
	var found string
	err := filepath.WalkDir(l.SessionsDir(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !isCodexSessionName(entry.Name()) {
			return nil
		}
		if codexIDFromName(entry.Name()) == sessionID {
			found = path
			return fs.SkipAll
		}
		summary, ok := readCodexSummary(path)
		if ok && summary.nativeID == sessionID {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find Codex session: %w", err)
	}
	return found, nil
}

// TouchedFiles extracts file arguments from Codex function/tool records. Shell
// commands remain intentionally conservative: their effects are covered by
// the Git/workspace fingerprint rather than guessed from command text.
func (l CodexLayout) TouchedFiles(records [][]byte, projectRoot string) []FileAccess {
	seen := map[string]bool{}
	for _, raw := range records {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			continue
		}
		collectCodexToolPaths(record, projectRoot, codexWriteRecord(record), seen)
	}
	out := make([]FileAccess, 0, len(seen))
	for path, written := range seen {
		out = append(out, FileAccess{Path: path, Written: written})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func collectCodexToolPaths(value any, projectRoot string, written bool, seen map[string]bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "file_path", "notebook_path", "filename", "path":
				if path, ok := child.(string); ok {
					if rel, inside := projectRelative(projectRoot, path); inside {
						seen[rel] = seen[rel] || written
					}
				}
			case "arguments", "input":
				if text, ok := child.(string); ok {
					var embedded any
					if json.Unmarshal([]byte(text), &embedded) == nil {
						collectCodexToolPaths(embedded, projectRoot, written, seen)
					}
				}
			}
			collectCodexToolPaths(child, projectRoot, written, seen)
		}
	case []any:
		for _, child := range value {
			collectCodexToolPaths(child, projectRoot, written, seen)
		}
	}
}

func codexWriteRecord(record map[string]any) bool {
	var names []string
	collectCodexNames(record, &names)
	for _, name := range names {
		name = strings.ToLower(name)
		for _, marker := range []string{"apply_patch", "write", "edit", "replace", "multi_edit", "notebook"} {
			if strings.Contains(name, marker) {
				return true
			}
		}
	}
	return false
}

func collectCodexNames(value any, names *[]string) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "name" || key == "tool" || key == "command" {
				if text, ok := child.(string); ok && len(text) < 256 {
					*names = append(*names, text)
				}
			}
			collectCodexNames(child, names)
		}
	case []any:
		for _, child := range value {
			collectCodexNames(child, names)
		}
	}
}

type codexSummary struct {
	nativeID string
	cwd      string
	title    string
	version  string
	created  time.Time
	updated  time.Time
}

func (s codexSummary) Title(projectRoot string, fallback time.Time) string {
	if title := clean(s.title); title != "" {
		return title
	}
	return (summary{cwd: projectRoot, created: s.created}).Title(projectRoot, fallback)
}

func readCodexSummary(path string) (codexSummary, bool) {
	file, err := os.Open(path)
	if err != nil {
		return codexSummary{}, false
	}
	defer file.Close() //nolint:errcheck

	var summary codexSummary
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 256*1024*1024)
	valid := 0
	for scanner.Scan() {
		var record codexRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Type == "" {
			continue
		}
		valid++
		observeCodexRecord(&summary, record)
		if summary.nativeID != "" && summary.cwd != "" && !summary.created.IsZero() && summary.title != "" {
			break
		}
	}
	if scanner.Err() != nil || valid == 0 {
		return codexSummary{}, false
	}
	return summary, true
}

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func summarizeCodexRecords(records [][]byte) codexSummary {
	var summary codexSummary
	for _, raw := range records {
		var record codexRecord
		if json.Unmarshal(raw, &record) == nil {
			observeCodexRecord(&summary, record)
		}
	}
	return summary
}

func observeCodexRecord(summary *codexSummary, record codexRecord) {
	if summary == nil {
		return
	}
	observeCodexTime(summary, record.Timestamp)
	var payload map[string]json.RawMessage
	if json.Unmarshal(record.Payload, &payload) != nil {
		return
	}
	if value := rawString(payload["cwd"]); value != "" && summary.cwd == "" {
		summary.cwd = value
	}
	if roots := rawStringArray(payload["workspace_roots"]); len(roots) != 0 && summary.cwd == "" {
		summary.cwd = roots[0]
	}

	switch record.Type {
	case "session_meta":
		if value := rawString(payload["id"]); value != "" {
			summary.nativeID = value
		} else if value := rawString(payload["session_id"]); value != "" {
			summary.nativeID = value
		} else if value := rawString(payload["id"]); value != "" {
			summary.nativeID = value
		}
		if value := rawString(payload["cli_version"]); value != "" {
			summary.version = value
		}
		observeCodexTime(summary, rawString(payload["timestamp"]))
	case "turn_context":
		if summary.cwd == "" {
			if value := rawString(payload["cwd"]); value != "" {
				summary.cwd = value
			} else if roots := rawStringArray(payload["workspace_roots"]); len(roots) != 0 {
				summary.cwd = roots[0]
			}
		}
	}
	if summary.title == "" {
		if prompt := codexPrompt(record.Type, payload); prompt != "" {
			summary.title = prompt
		}
	}
}

func observeCodexTime(summary *codexSummary, value string) {
	if summary == nil || strings.TrimSpace(value) == "" {
		return
	}
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return
	}
	if summary.created.IsZero() || timestamp.Before(summary.created) {
		summary.created = timestamp
	}
	if timestamp.After(summary.updated) {
		summary.updated = timestamp
	}
}

func codexPrompt(recordType string, payload map[string]json.RawMessage) string {
	if recordType == "event_msg" {
		kind := rawString(payload["type"])
		if kind != "user_message" && kind != "user" && kind != "input" {
			return ""
		}
		for _, key := range []string{"message", "text"} {
			if value := rawString(payload[key]); value != "" {
				return value
			}
		}
	}
	if recordType == "response_item" && rawString(payload["role"]) == "user" {
		var content []map[string]json.RawMessage
		if json.Unmarshal(payload["content"], &content) == nil {
			for _, block := range content {
				if value := rawString(block["text"]); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) != 0 && json.Unmarshal(raw, &value) == nil {
		return value
	}
	return ""
}

func rawStringArray(raw json.RawMessage) []string {
	var values []string
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

func isCodexSessionName(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}

func codexIDFromName(name string) string {
	if !isCodexSessionName(name) {
		return ""
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, "rollout-"), ".jsonl")
	candidate := value
	if len(value) > 37 && value[len(value)-37] == '-' {
		candidate = value[len(value)-36:]
	} else if index := strings.LastIndexByte(value, '-'); index >= 0 {
		candidate = value[index+1:]
	}
	if checkSessionID(candidate) != nil {
		return ""
	}
	return candidate
}
