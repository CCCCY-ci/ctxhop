// Command rewrite translates a Claude Code session from one machine's path
// space into another's, so PoC-1 can test whether the agent still resumes it.
//
// The scan in poc/pathscan found three classes of path binding:
//
//	(1) structured project paths  - cwd, file_path, filePath, planFilePath, ...
//	(2) agent data directory paths - backup.realParentDir, snapshot keys
//	(3) paths inside free text     - message content, stdout, stderr
//
// This tool rewrites (1) and (2) and deliberately leaves (3) alone. Free text
// is a record of what happened on the other machine; rewriting it would edit
// the conversation's meaning, and the workspace consistency check is the
// mechanism meant to tell the agent what is actually true here (§9.5).
//
// Paths are rewritten by field allowlist rather than by blanket search and
// replace, so that an unexpected field carrying a path is reported instead of
// silently modified.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxLine = 64 << 20

// projectFields are leaf field names whose values are absolute paths inside the
// project. Matching is on the final path element so that the same name nested
// anywhere is covered.
var projectFields = map[string]bool{
	"cwd":          true,
	"file_path":    true,
	"filePath":     true,
	"planFilePath": true,
	"trackingPath": true,
}

// agentDirFields are leaf field names holding paths into the agent's own data
// directory, which differs between machines because the user name does.
var agentDirFields = map[string]bool{
	"realParentDir": true,
}

// keyRewriteContainers are objects whose *keys* are absolute paths.
var keyRewriteContainers = map[string]bool{
	"trackedFileBackups": true,
}

type rewriter struct {
	fromRoot, toRoot string
	fromHome, toHome string

	// rewritten counts substitutions per field path, for the report.
	rewritten map[string]int
	// skipped counts values that look absolute but sat in a field we do not
	// rewrite. These are the ones a real implementation must justify.
	skipped map[string]int
}

func main() {
	in := flag.String("in", "", "input session .jsonl")
	out := flag.String("out", "", "output session .jsonl")
	fromRoot := flag.String("from-root", "", "project root on the source machine")
	toRoot := flag.String("to-root", "", "project root on this machine")
	fromHome := flag.String("from-home", "", "agent data dir on the source machine")
	toHome := flag.String("to-home", "", "agent data dir on this machine")
	flag.Parse()

	if *in == "" || *out == "" || *fromRoot == "" || *toRoot == "" {
		fmt.Fprintln(os.Stderr, "usage: rewrite -in x.jsonl -out y.jsonl -from-root <path> -to-root <path> [-from-home <path> -to-home <path>]")
		os.Exit(2)
	}

	r := &rewriter{
		fromRoot:  *fromRoot,
		toRoot:    *toRoot,
		fromHome:  *fromHome,
		toHome:    *toHome,
		rewritten: map[string]int{},
		skipped:   map[string]int{},
	}

	if err := r.run(*in, *out); err != nil {
		fmt.Fprintf(os.Stderr, "rewrite: %v\n", err)
		os.Exit(1)
	}

	r.report()
}

func (r *rewriter) run(in, out string) error {
	src, err := os.Open(in)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer src.Close()

	// Write to a temporary file and rename, so an interrupted run never leaves
	// a half-written session behind (BR-11).
	tmp := out + ".tmp"
	dst, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	defer os.Remove(tmp)

	w := bufio.NewWriter(dst)
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			dst.Close()
			return fmt.Errorf("input is not valid jsonl: %w", err)
		}

		v = r.walk("", v)

		encoded, err := json.Marshal(v)
		if err != nil {
			dst.Close()
			return fmt.Errorf("re-encode record: %w", err)
		}
		if _, err := w.Write(append(encoded, '\n')); err != nil {
			dst.Close()
			return fmt.Errorf("write record: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		dst.Close()
		return fmt.Errorf("read input: %w", err)
	}

	if err := w.Flush(); err != nil {
		dst.Close()
		return fmt.Errorf("flush output: %w", err)
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return fmt.Errorf("sync output: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return fmt.Errorf("install output: %w", err)
	}
	return nil
}

// walk returns a copy of v with allowlisted paths translated.
func (r *rewriter) walk(path string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		leaf := leafName(path)
		result := make(map[string]any, len(t))
		for k, child := range t {
			newKey := k
			if keyRewriteContainers[leaf] {
				if s, changed := r.translate(k); changed {
					newKey = s
					r.rewritten[path+".<key>"]++
				}
			}
			result[newKey] = r.walk(join(path, k), child)
		}
		return result

	case []any:
		result := make([]any, len(t))
		for i, child := range t {
			result[i] = r.walk(path+"[]", child)
		}
		return result

	case string:
		leaf := leafName(path)
		if projectFields[leaf] || agentDirFields[leaf] {
			if s, changed := r.translate(t); changed {
				r.rewritten[path]++
				return s
			}
			return t
		}
		if looksAbsolute(t) {
			r.skipped[path]++
		}
		return t

	default:
		return v
	}
}

// translate replaces a source-machine prefix with this machine's equivalent.
// Comparison is case-insensitive because Windows paths are, and the recorded
// spelling is not guaranteed to match the user's.
func (r *rewriter) translate(s string) (string, bool) {
	if out, ok := replacePrefix(s, r.fromRoot, r.toRoot); ok {
		return out, true
	}
	if r.fromHome != "" {
		if out, ok := replacePrefix(s, r.fromHome, r.toHome); ok {
			return out, true
		}
	}
	return s, false
}

func replacePrefix(s, from, to string) (string, bool) {
	if from == "" || len(s) < len(from) {
		return s, false
	}
	if !strings.EqualFold(s[:len(from)], from) {
		return s, false
	}
	rest := s[len(from):]
	// Convert the remainder's separators to the target platform's, so a POSIX
	// tail does not survive into a Windows path or vice versa.
	sep := string(filepath.Separator)
	rest = strings.ReplaceAll(rest, `\`, sep)
	rest = strings.ReplaceAll(rest, "/", sep)
	return to + rest, true
}

// looksAbsolute is a coarse check used only to report fields we chose not to
// rewrite. False positives here are harmless; they just show up in the report.
func looksAbsolute(s string) bool {
	if len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	return strings.HasPrefix(s, "/Users/") || strings.HasPrefix(s, "/home/")
}

func leafName(path string) string {
	path = strings.TrimSuffix(path, "[]")
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func (r *rewriter) report() {
	fmt.Println("rewritten:")
	for _, k := range sortedKeys(r.rewritten) {
		fmt.Printf("  %-48s %d\n", k, r.rewritten[k])
	}
	if len(r.rewritten) == 0 {
		fmt.Println("  (nothing)")
	}

	fmt.Println("\nleft alone (absolute-looking values in non-allowlisted fields):")
	for _, k := range sortedKeys(r.skipped) {
		fmt.Printf("  %-48s %d\n", k, r.skipped[k])
	}
	if len(r.skipped) == 0 {
		fmt.Println("  (none)")
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
