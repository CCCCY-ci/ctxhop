// Command fingerprint captures and compares workspace state, to measure
// whether the consistency check of §9.5 can be both accurate and quiet.
//
// The check exists because a restored session describes files as the source
// machine left them. If the target machine's copies differ, the agent resumes
// with a confidently wrong picture of the code, which is worse than not
// resuming at all.
//
// The scan in poc/touched showed that the session's own file_path arguments are
// necessary but not sufficient: roughly half of all tool calls are shell
// commands with no recorded path, and a shell command can rewrite anything.
// So the fingerprint is anchored on git state, which observes every change
// regardless of how it was made, and the session's touched set is used only to
// scope per-file comparison and keep the report quiet.
//
// Three layers:
//
//	L1  HEAD commit and branch      - catches "you are on different code"
//	L2  the set of dirty files      - catches changes made by any means
//	L3  per-file content hashes     - scoped to touched ∪ dirty
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Fingerprint is what a push records alongside a session.
//
// It holds paths and digests only, never file contents, so it does not turn
// AgentSync into a code sync tool (BR-08).
type Fingerprint struct {
	Head    string            `json:"head"`
	Branch  string            `json:"branch"`
	Dirty   []string          `json:"dirty"`
	Touched []string          `json:"touched"`
	Hashes  map[string]string `json:"hashes"`
}

// Verdict classifies one file's current state against the fingerprint.
type Verdict string

const (
	// VerdictSame means the file is byte-identical to what the session saw.
	VerdictSame Verdict = "same"
	// VerdictAdvanced means the file changed but the change is explainable:
	// this machine is on a later commit and the file is clean, so the content
	// came from history rather than from someone editing over the session.
	VerdictAdvanced Verdict = "advanced"
	// VerdictDiverged means the file differs and we cannot explain why.
	VerdictDiverged Verdict = "diverged"
	// VerdictMissing means the file is gone.
	VerdictMissing Verdict = "missing"
)

func main() {
	mode := flag.String("mode", "", "capture | compare")
	root := flag.String("root", ".", "project root")
	touched := flag.String("touched", "", "comma-separated project-relative paths the session touched")
	file := flag.String("fingerprint", "fingerprint.json", "fingerprint file to write or read")
	flag.Parse()

	var err error
	switch *mode {
	case "capture":
		err = capture(*root, splitList(*touched), *file)
	case "compare":
		err = compare(*root, *file)
	default:
		fmt.Fprintln(os.Stderr, "usage: fingerprint -mode capture|compare -root <dir> [-touched a,b] [-fingerprint f.json]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fingerprint: %v\n", err)
		os.Exit(1)
	}
}

func capture(root string, touched []string, out string) error {
	fp := Fingerprint{Touched: touched, Hashes: map[string]string{}}

	var err error
	if fp.Head, err = git(root, "rev-parse", "HEAD"); err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	if fp.Branch, err = git(root, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return fmt.Errorf("read branch: %w", err)
	}
	if fp.Dirty, err = dirtyFiles(root); err != nil {
		return fmt.Errorf("read working tree status: %w", err)
	}

	// Hash the union of what the session touched and what the working tree
	// reports as modified. The first set catches edits the session made through
	// tools; the second catches everything else, including shell commands.
	for _, rel := range union(touched, fp.Dirty) {
		h, err := hashFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("hash %s: %w", rel, err)
		}
		fp.Hashes[rel] = h
	}

	data, err := json.MarshalIndent(fp, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fingerprint: %w", err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write fingerprint: %w", err)
	}

	fmt.Printf("captured: head=%s branch=%s dirty=%d touched=%d hashed=%d\n",
		short(fp.Head), fp.Branch, len(fp.Dirty), len(touched), len(fp.Hashes))
	return nil
}

func compare(root, in string) error {
	data, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("read fingerprint: %w", err)
	}
	var fp Fingerprint
	if err := json.Unmarshal(data, &fp); err != nil {
		return fmt.Errorf("decode fingerprint: %w", err)
	}

	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	branch, err := git(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("read branch: %w", err)
	}

	// A later commit that contains the session's starting point is a normal,
	// explainable situation: the user pulled or committed since. A branch that
	// does not contain it is not.
	descendant := head == fp.Head
	if !descendant {
		if _, err := git(root, "merge-base", "--is-ancestor", fp.Head, head); err == nil {
			descendant = true
		}
	}

	dirty, err := dirtyFiles(root)
	if err != nil {
		return fmt.Errorf("read working tree status: %w", err)
	}
	dirtyNow := map[string]bool{}
	for _, d := range dirty {
		dirtyNow[d] = true
	}

	verdicts := map[string]Verdict{}
	for rel, want := range fp.Hashes {
		got, err := hashFile(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case os.IsNotExist(err):
			verdicts[rel] = VerdictMissing
		case err != nil:
			return fmt.Errorf("hash %s: %w", rel, err)
		case got == want:
			verdicts[rel] = VerdictSame
		case descendant && !dirtyNow[rel]:
			// Content differs, this machine is ahead, and the file is committed
			// here. The difference came from history, not from an edit racing
			// the session.
			verdicts[rel] = VerdictAdvanced
		default:
			verdicts[rel] = VerdictDiverged
		}
	}

	report(fp, head, branch, descendant, verdicts)
	return nil
}

func report(fp Fingerprint, head, branch string, descendant bool, verdicts map[string]Verdict) {
	counts := map[Verdict]int{}
	for _, v := range verdicts {
		counts[v]++
	}

	fmt.Printf("head:   %s -> %s\n", short(fp.Head), short(head))
	fmt.Printf("branch: %s -> %s\n", fp.Branch, branch)
	fmt.Printf("ahead:  %v\n", descendant)
	fmt.Printf("files:  same=%d advanced=%d diverged=%d missing=%d\n\n",
		counts[VerdictSame], counts[VerdictAdvanced], counts[VerdictDiverged], counts[VerdictMissing])

	problems := make([]string, 0, len(verdicts))
	for rel, v := range verdicts {
		if v == VerdictDiverged || v == VerdictMissing {
			problems = append(problems, fmt.Sprintf("  %-10s %s", v, rel))
		}
	}
	sort.Strings(problems)

	advanced := make([]string, 0, len(verdicts))
	for rel, v := range verdicts {
		if v == VerdictAdvanced {
			advanced = append(advanced, "  advanced   "+rel)
		}
	}
	sort.Strings(advanced)

	switch {
	case len(problems) > 0:
		fmt.Println("VERDICT: inconsistent - these files differ from what the session recorded:")
		fmt.Println(strings.Join(problems, "\n"))
	case len(advanced) > 0:
		// Still worth showing. The difference is explainable, but the agent's
		// picture of these files is nonetheless out of date.
		fmt.Println("VERDICT: explainable - later commits changed these files:")
		fmt.Println(strings.Join(advanced, "\n"))
	case fp.Branch != branch:
		fmt.Println("VERDICT: explainable - different branch name, but no file differs")
	default:
		fmt.Println("VERDICT: consistent")
	}
}

// dirtyFiles lists modified, added, deleted and untracked paths, relative to
// the project root and slash-separated.
func dirtyFiles(root string) ([]string, error) {
	out, err := git(root, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: two status characters, a space, then the path. Renames
		// appear as "old -> new"; the new name is what exists on disk.
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		path = strings.Trim(path, `"`)
		files = append(files, filepath.ToSlash(path))
	}
	sort.Strings(files)
	return files, nil
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func git(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// Trim only the trailing newline. Trimming all whitespace would eat the
	// leading status column of `git status --porcelain`, shifting every path by
	// one character so that no file ever matches the fingerprint - which shows
	// up as a modified file being silently classified as explainable.
	return strings.TrimRight(string(out), "\r\n"), nil
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
