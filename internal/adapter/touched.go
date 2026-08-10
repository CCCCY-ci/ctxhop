package adapter

import (
	"encoding/json"
	"sort"
	"strings"
)

// writeTools are the tools whose use means the session changed a file.
//
// Anything absent is treated as a read, which is the safe direction: an extra
// read costs one hash comparison, while a missed write lets a stale file
// through the consistency check unnoticed (spec §4.7).
var writeTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// FileAccess records how a session interacted with one project file.
type FileAccess struct {
	// Path is relative to the project root, slash-separated.
	Path string
	// Written reports whether a write tool touched it.
	Written bool
}

// TouchedFiles returns the project files a session read or wrote.
//
// This set scopes the per-file half of the workspace consistency check. The
// whole working tree would be the wrong scope: unrelated edits on the target
// machine would be flagged, users would learn to dismiss the warning, and a
// check nobody reads protects nobody (§9.5).
//
// It is deliberately not the only input to that check. PoC-2 measured that
// roughly 40% of tool calls are shell commands recording no path at all, and a
// shell command can rewrite anything, so the caller anchors on git state and
// uses this only to narrow the comparison. The gap is inherent to what the
// session records and cannot be closed here.
//
// Records that do not parse are skipped: one odd record must not hide every
// file the rest of the session touched.
func TouchedFiles(records [][]byte, projectRoot string) []FileAccess {
	seen := map[string]bool{}

	for _, raw := range records {
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		collectToolUses(rec, projectRoot, seen)
	}

	out := make([]FileAccess, 0, len(seen))
	for path, written := range seen {
		out = append(out, FileAccess{Path: path, Written: written})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// collectToolUses records every file path a record's tool calls name.
func collectToolUses(rec map[string]any, projectRoot string, seen map[string]bool) {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return
	}

	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}

		input, ok := block["input"].(map[string]any)
		if !ok {
			continue
		}
		path, ok := input["file_path"].(string)
		if !ok || path == "" {
			continue
		}

		rel, inside := projectRelative(projectRoot, path)
		if !inside {
			// Files outside the project are not part of the workspace we
			// compare; the fingerprint describes the project, not the machine.
			continue
		}

		name, _ := block["name"].(string)
		seen[rel] = seen[rel] || writeTools[name]
	}
}

// projectRelative converts an absolute path into one relative to the project
// root, reporting whether it lies inside the project at all.
func projectRelative(root, path string) (string, bool) {
	root = strings.TrimRight(root, `/\`)
	if root == "" || len(path) <= len(root) {
		return "", false
	}
	if !samePathPrefix(path[:len(root)], root) {
		return "", false
	}

	rest := path[len(root):]
	if rest[0] != '/' && rest[0] != '\\' {
		// A sibling directory sharing the prefix, not a child.
		return "", false
	}
	rest = strings.TrimLeft(rest, `/\`)
	if rest == "" {
		return "", false
	}
	return strings.ReplaceAll(rest, `\`, "/"), true
}
