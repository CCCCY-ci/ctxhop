package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// digestAbsent marks a path that was not present when the fingerprint was
// taken. It has to be recorded rather than omitted: a file the session deleted
// is still consistent if it is still gone, and inconsistent if it came back.
const digestAbsent = "absent"

// Verdict is how much the working tree has moved since a session ran.
type Verdict int

const (
	// Consistent means nothing the session touched differs.
	Consistent Verdict = iota
	// Explainable means the differences are accounted for by commits made
	// since. The files are still listed: the agent's knowledge of them is out
	// of date either way, and a bare "looks fine" would hide that (PoC-2 §4.1).
	Explainable
	// Divergent means the working tree no longer matches and commits do not
	// explain it.
	Divergent
)

func (v Verdict) String() string {
	switch v {
	case Consistent:
		return "consistent"
	case Explainable:
		return "explainable"
	default:
		return "divergent"
	}
}

// Fingerprint is the state of a working tree at the moment a session was
// pushed, anchored on git rather than on the session (PoC-2 §3).
//
// The session's own record of which files it touched is not enough on its own:
// anything the user changed through a shell is invisible to it. The dirty set
// closes that gap, and the touched set narrows what is compared, which is what
// keeps unrelated edits from being reported.
//
// Paths are slash-separated and relative to the project root, because this
// value is captured on one machine and compared on another.
type Fingerprint struct {
	Head   string            `json:"head"`
	Branch string            `json:"branch"`
	Dirty  []string          `json:"dirty"`
	Files  map[string]string `json:"files"`
}

// FileVerdict is one file's contribution to a report.
type FileVerdict struct {
	Path    string
	Verdict Verdict
	Note    string
}

// Report is the outcome of comparing a working tree against a fingerprint.
type Report struct {
	Verdict Verdict
	Head    string
	Branch  string
	Files   []FileVerdict
}

// Capture records the state of root, narrowing the per-file comparison to the
// files the session touched plus everything currently modified.
//
// touched may hold absolute or relative paths; anything outside root is
// dropped, since a path outside the project has no meaning on another machine.
func Capture(ctx context.Context, root string, touched []string) (Fingerprint, error) {
	head, branch, err := headAndBranch(ctx, root)
	if err != nil {
		return Fingerprint{}, err
	}

	dirty, err := dirtyPaths(ctx, root)
	if err != nil {
		return Fingerprint{}, err
	}

	scope := union(dirty, relativePaths(root, touched))
	files := make(map[string]string, len(scope))
	for _, rel := range scope {
		digest, err := digestOf(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return Fingerprint{}, fmt.Errorf("fingerprint file: %w", err)
		}
		files[rel] = digest
	}

	return Fingerprint{Head: head, Branch: branch, Dirty: dirty, Files: files}, nil
}

// Compare measures root against a fingerprint taken earlier, possibly on
// another machine.
//
// It never answers Consistent on a failure. The check is worth having only
// because "consistent" can be believed; a timeout or an unreadable repository
// has to surface as an error, not as reassurance (spec §7.1).
func Compare(ctx context.Context, root string, fp Fingerprint) (Report, error) {
	head, branch, err := headAndBranch(ctx, root)
	if err != nil {
		return Report{}, err
	}

	report := Report{Verdict: Consistent, Head: head, Branch: branch}

	// Whether history moved forward decides whether a difference can be
	// explained at all. A head that is not a descendant means this working tree
	// is on other code, however similar the files look.
	movedForward := head != fp.Head && fp.Head != "" && isAncestor(ctx, root, fp.Head, head)
	if head != fp.Head && !movedForward {
		report.Verdict = Divergent
	}

	dirty, err := dirtyPaths(ctx, root)
	if err != nil {
		return Report{}, err
	}
	isDirty := make(map[string]bool, len(dirty))
	for _, p := range dirty {
		isDirty[p] = true
	}

	for _, rel := range sortedKeys(fp.Files) {
		now, err := digestOf(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return Report{}, fmt.Errorf("compare file: %w", err)
		}
		if now == fp.Files[rel] {
			// Committing the session's uncommitted work leaves content
			// identical, and a commit by itself is not a difference (PoC-2 §4.1).
			continue
		}

		fv := FileVerdict{Path: rel, Verdict: Divergent}
		switch {
		case now == digestAbsent:
			fv.Note = "deleted since the session"
		case fp.Files[rel] == digestAbsent:
			fv.Note = "created since the session"
		case movedForward && !isDirty[rel] && changedBetween(ctx, root, fp.Head, head, rel):
			// Explained only if commits account for the whole difference. A
			// file that also has uncommitted changes is not explained by them,
			// and calling it explainable would overstate what is known.
			fv.Verdict = Explainable
			fv.Note = "changed by commits made since the session"
		default:
			fv.Note = "modified since the session"
		}

		report.Files = append(report.Files, fv)
		if fv.Verdict > report.Verdict {
			report.Verdict = fv.Verdict
		}
	}

	return report, nil
}

func headAndBranch(ctx context.Context, root string) (head, branch string, err error) {
	head, err = runGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read HEAD: %w", err)
	}
	branch, err = runGit(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read branch: %w", err)
	}
	return head, branch, nil
}

// isAncestor reports whether older is in the history of newer.
//
// A false answer from a failed command is safe here: it can only downgrade a
// verdict to Divergent, never upgrade one to Consistent.
func isAncestor(ctx context.Context, root, older, newer string) bool {
	_, err := runGit(ctx, root, "merge-base", "--is-ancestor", older, newer)
	return err == nil
}

// changedBetween reports whether a commit in (older, newer] touched rel.
func changedBetween(ctx context.Context, root, older, newer, rel string) bool {
	out, err := runGitRaw(ctx, root, "diff", "--name-only", "-z", older+".."+newer, "--", rel)
	if err != nil {
		return false
	}
	return len(splitNUL(out)) > 0
}

// dirtyPaths lists everything git reports as modified, staged or untracked.
//
// This is the layer that catches changes made through a shell, which the
// session itself never sees (PoC-2 §3).
func dirtyPaths(ctx context.Context, root string) ([]string, error) {
	out, err := runGitRaw(ctx, root, "status", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("read working tree status: %w", err)
	}
	return parsePorcelainZ(out)
}

// parsePorcelainZ reads NUL-separated status output.
//
// -z is not an optimisation. Without it git quotes and octal-escapes any path
// that is not plain ASCII - 中文名.txt arrives as
// "\344\270\255\346\226\207\345\220\215.txt" - which matches no real filename,
// so every non-English file would be silently reported as unmodified. -z also
// removes the need to trim, which is where the leading status column used to
// get eaten (spec §4.1).
//
// The cost is that a rename emits two fields, and in -z mode the *new* path
// comes first - the opposite order from the human-readable form.
func parsePorcelainZ(out []byte) ([]string, error) {
	fields := splitNUL(out)
	var dirty []string

	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			return nil, fmt.Errorf("unparseable status entry %q", entry)
		}
		status, path := entry[:2], entry[3:]
		dirty = append(dirty, filepath.ToSlash(path))

		if status[0] == 'R' || status[0] == 'C' {
			i++
			if i >= len(fields) {
				return nil, fmt.Errorf("status entry %q promised an origin path that never came", entry)
			}
			// Both ends of a rename matter: the old path is gone and the new
			// one is new, and a session may have touched either.
			dirty = append(dirty, filepath.ToSlash(fields[i]))
		}
	}

	sort.Strings(dirty)
	return dirty, nil
}

// digestOf hashes a file's contents, reporting a missing file as absent rather
// than as an error.
func digestOf(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return digestAbsent, nil
		}
		return "", err
	}
	defer func() {
		// Read-only handle: nothing was buffered, so there is no write to lose.
		_ = f.Close()
	}()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		// A path that became a directory is not a file that is unchanged.
		return "directory", nil
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// relativePaths converts touched paths into root-relative slash form, dropping
// anything outside the project.
//
// A path outside the project root cannot be compared on another machine, and
// guessing at one would be exactly the speculative rewriting §9.3 forbids.
func relativePaths(root string, paths []string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		out = append(out, rel)
	}
	return out
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
