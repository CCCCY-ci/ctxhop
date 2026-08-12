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
	top, err := worktreeTop(ctx, root)
	if err != nil {
		return Fingerprint{}, err
	}

	head, branch, err := headAndBranch(ctx, top)
	if err != nil {
		return Fingerprint{}, err
	}

	dirty, err := dirtyPaths(ctx, top)
	if err != nil {
		return Fingerprint{}, err
	}

	scope := union(dirty, relativePaths(top, touched))
	files, err := digestAll(ctx, top, scope)
	if err != nil {
		return Fingerprint{}, err
	}

	return Fingerprint{Head: head, Branch: branch, Dirty: dirty, Files: files}, nil
}

// worktreeTop resolves root to the top of its working tree.
//
// git reports status paths relative to the repository root no matter which
// directory it was invoked from (measured). Joining them onto anything else -
// a project bound to a subdirectory, say - produces paths that exist nowhere,
// which read as "absent" on both sides and therefore as agreement. That is the
// check silently answering yes forever, which spec §4.1 calls worse than not
// having it.
func worktreeTop(ctx context.Context, root string) (string, error) {
	top, ok, err := worktreeRoot(ctx, root)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("the consistency check needs a git working tree; this directory is not in one")
	}
	return top, nil
}

// Compare measures root against a fingerprint taken earlier, possibly on
// another machine.
//
// It never answers Consistent on a failure. The check is worth having only
// because "consistent" can be believed; a timeout or an unreadable repository
// has to surface as an error, not as reassurance (spec §7.1).
func Compare(ctx context.Context, root string, fp Fingerprint) (Report, error) {
	top, err := worktreeTop(ctx, root)
	if err != nil {
		return Report{}, err
	}

	head, branch, err := headAndBranch(ctx, top)
	if err != nil {
		return Report{}, err
	}

	report := Report{Verdict: Consistent, Head: head, Branch: branch}

	// Whether history moved forward decides whether a difference can be
	// explained at all. A head that is not a descendant means this working tree
	// is on other code, however similar the files look.
	//
	// An empty head on either side is a repository with no commits yet, where
	// ancestry says nothing either way; the file digests still do.
	movedForward := false
	if head != fp.Head && head != "" && fp.Head != "" {
		movedForward = isAncestor(ctx, top, fp.Head, head)
		if !movedForward {
			report.Verdict = Divergent
		}
	}

	dirty, err := dirtyPaths(ctx, top)
	if err != nil {
		return Report{}, err
	}
	isDirty := make(map[string]bool, len(dirty))
	for _, p := range dirty {
		isDirty[p] = true
	}

	current, err := digestAll(ctx, top, sortedKeys(fp.Files))
	if err != nil {
		return Report{}, err
	}

	for _, rel := range sortedKeys(fp.Files) {
		now := current[rel]
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
		case movedForward && !isDirty[rel] && changedBetween(ctx, top, fp.Head, head, rel):
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

// headAndBranch reads the current commit and branch.
//
// A repository with no commits yet reports an empty head rather than failing:
// `rev-parse HEAD` exits 128 there (measured), and a project whose first
// session runs before its first commit is completely ordinary. Refusing would
// mean the very first session of a new project could never be fingerprinted.
func headAndBranch(ctx context.Context, root string) (head, branch string, err error) {
	head, err = runGit(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		if unanswered(err) {
			return "", "", fmt.Errorf("read HEAD: %w", err)
		}
		head = ""
	}
	// --show-current rather than rev-parse: it still answers before the first
	// commit, and returns empty on a detached head instead of the word "HEAD".
	branch, err = runGit(ctx, root, "branch", "--show-current")
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
	// -uall lists untracked files individually. Without it git collapses a whole
	// untracked directory into one entry ending in "/" (measured), and every
	// file inside it - created, edited or deleted since the session - would
	// compare equal forever.
	out, err := runGitRaw(ctx, root, "status", "--porcelain", "-z", "-uall")
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

// Digests for paths that are not readable file content. They are values rather
// than errors because a path whose very kind changed is a difference, not a
// failure to look.
const (
	digestDirectory = "kind:directory"
	digestIrregular = "kind:irregular"
)

// digestAll computes a comparable digest for each path, relative to top.
//
// Content is hashed by git rather than by reading raw bytes. With
// core.autocrlf=true - the default for Git for Windows - the same commit checks
// out as CRLF on Windows and LF elsewhere (measured), so raw bytes differ
// between machines for every text file while git considers them identical. A
// fingerprint taken on one platform would then report every touched file as
// modified on the other, which is precisely the comparison this whole layer
// exists to make. `git hash-object` applies the same filters git applies, and
// its answer matches the blob in the index exactly.
func digestAll(ctx context.Context, top string, rels []string) (map[string]string, error) {
	digests := make(map[string]string, len(rels))

	var hashable []string
	for _, rel := range rels {
		full := filepath.Join(top, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		switch {
		case errors.Is(err, os.ErrNotExist):
			digests[rel] = digestAbsent
		case err != nil:
			return nil, fmt.Errorf("read a file in the working tree: %w", pathSafe(err))
		case info.IsDir():
			digests[rel] = digestDirectory
		case !info.Mode().IsRegular():
			// A symlink, socket or FIFO. Opening a FIFO blocks until somebody
			// writes to it, which would hang the capture with no timeout - and
			// build tools do leave them lying around.
			digests[rel] = digestIrregular
		case strings.ContainsAny(rel, "\n\r"):
			// --stdin-paths is newline-delimited, so such a name cannot be sent
			// through it. Falling back keeps one pathological filename from
			// making the whole project unfingerprintable; it can only exist on
			// systems where the CRLF problem does not arise.
			digest, err := rawDigest(full)
			if err != nil {
				return nil, err
			}
			digests[rel] = digest
		default:
			hashable = append(hashable, rel)
		}
	}

	if len(hashable) == 0 {
		return digests, nil
	}

	var stdin strings.Builder
	for _, rel := range hashable {
		stdin.WriteString(rel)
		stdin.WriteByte('\n')
	}
	out, err := runGitStdin(ctx, top, []byte(stdin.String()), "hash-object", "--stdin-paths")
	if err != nil {
		return nil, fmt.Errorf("hash working tree files: %w", err)
	}

	lines := strings.Fields(string(out))
	if len(lines) != len(hashable) {
		// Never guess at an alignment: a digest attributed to the wrong file
		// would report agreement about something never examined.
		return nil, fmt.Errorf("hashed %d files but got %d digests", len(hashable), len(lines))
	}
	for i, rel := range hashable {
		digests[rel] = lines[i]
	}
	return digests, nil
}

// rawDigest hashes bytes directly, for the paths git cannot be handed.
func rawDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read a file in the working tree: %w", pathSafe(err))
	}
	defer func() {
		// Read-only handle: nothing was buffered, so there is no write to lose.
		_ = f.Close()
	}()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read a file in the working tree: %w", pathSafe(err))
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
