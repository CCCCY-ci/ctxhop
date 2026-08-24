package project

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	digestAbsent    = "<absent>"
	digestDirectory = "<directory>"
)

// Fingerprint records the Git state and the content digests relevant to one
// session. It contains paths and hashes only; it never carries file contents
// and therefore does not turn the workspace check into a code synchronizer.
type Fingerprint struct {
	Head   string            `json:"head"`
	Branch string            `json:"branch"`
	Dirty  []string          `json:"dirty"`
	Files  map[string]string `json:"files"`
}

// Verdict is the result of comparing a target workspace with a Fingerprint.
type Verdict int

const (
	Consistent Verdict = iota
	Explainable
	Divergent
)

// String returns the stable user-facing name of a verdict.
func (v Verdict) String() string {
	switch v {
	case Consistent:
		return "consistent"
	case Explainable:
		return "explainable"
	case Divergent:
		return "divergent"
	default:
		return "unknown"
	}
}

// MarshalJSON keeps reports readable while the in-memory type remains an
// integer enum, which also makes invalid zero-value handling explicit to Go
// callers.
func (v Verdict) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// FileReport describes one file whose current content differs from the
// session's recorded view.
type FileReport struct {
	Path string `json:"path"`
	Note string `json:"note"`
}

// Difference is kept as a descriptive alias for callers that prefer the
// domain term over the report-oriented name.
type Difference = FileReport

// Report is the complete result of a workspace comparison.
type Report struct {
	Verdict Verdict      `json:"verdict"`
	Files   []FileReport `json:"files,omitempty"`
}

// Capture records the relevant workspace state for a project directory.
// Git repositories include Git-wide dirty-file coverage; non-Git roots use
// the L3 touched-file fallback.
func Capture(ctx context.Context, dir string, touched []string) (Fingerprint, error) {
	root, gitBacked, err := fingerprintRoot(ctx, dir)
	if err != nil {
		return Fingerprint{}, err
	}

	var head, branch string
	var dirty []string
	if gitBacked {
		head, branch, err = headAndBranch(ctx, root)
		if err != nil {
			return Fingerprint{}, fmt.Errorf("read Git state: %w", err)
		}
		dirty, err = dirtyPaths(ctx, root)
		if err != nil {
			return Fingerprint{}, fmt.Errorf("read Git working tree state: %w", err)
		}
	}

	paths := unionPaths(relativeTouched(root, dir, touched), dirty)
	files, err := digestAllForFingerprint(ctx, root, paths, gitBacked)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("capture workspace digests: %w", err)
	}

	return Fingerprint{
		Head:   head,
		Branch: branch,
		Dirty:  append([]string(nil), dirty...),
		Files:  files,
	}, nil
}

// Compare checks the current project against a previously captured
// Fingerprint. Git-backed workspaces include Git state and dirty-file checks;
// non-Git workspaces use the L3 touched-file fallback. It never returns a
// Consistent report when a required check failed (BR-12).
func Compare(ctx context.Context, dir string, fp Fingerprint) (Report, error) {
	root, gitBacked, err := fingerprintRoot(ctx, dir)
	if err != nil {
		return Report{}, err
	}

	var head, branch string
	var dirty []string
	if gitBacked {
		head, branch, err = headAndBranch(ctx, root)
		if err != nil {
			return Report{}, fmt.Errorf("read Git state: %w", err)
		}
		dirty, err = dirtyPaths(ctx, root)
		if err != nil {
			return Report{}, fmt.Errorf("read Git working tree state: %w", err)
		}
	}
	current, err := digestAllForFingerprint(ctx, root, sortedDigestPaths(fp.Files), gitBacked)
	if err != nil {
		return Report{}, fmt.Errorf("compare workspace digests: %w", err)
	}

	dirtySet := make(map[string]bool, len(dirty))
	for _, path := range dirty {
		dirtySet[path] = true
	}

	descendant := head == fp.Head
	if !descendant && head != "" && fp.Head != "" {
		descendant = isAncestor(ctx, root, fp.Head, head)
	}

	report := Report{Verdict: Consistent}
	if fp.Head != head && !descendant {
		report.Verdict = Divergent
	}

	for _, path := range sortedDigestPaths(fp.Files) {
		want := fp.Files[path]
		got := current[path]
		if want == got {
			continue
		}

		note := "the file differs from the state recorded by the session"
		explainable := descendant && !dirtySet[path] && fp.Head != "" && head != "" && changedBetween(ctx, root, fp.Head, head, path)
		if explainable {
			note = "a later Git commit changed this file after the session was recorded"
			if report.Verdict == Consistent {
				report.Verdict = Explainable
			}
		} else {
			report.Verdict = Divergent
		}
		report.Files = append(report.Files, FileReport{Path: path, Note: note})
	}

	// A branch rename with the same commit and file contents is harmless but
	// still worth surfacing: the session was recorded on a differently named
	// branch, even though no file is stale.
	if report.Verdict == Consistent && fp.Branch != branch && fp.Head == head {
		report.Verdict = Explainable
	}
	return report, nil
}

func repositoryRoot(ctx context.Context, dir string) (string, error) {
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("identify the Git project: %w", err)
	}
	root, err := absoluteDirectory(top)
	if err != nil {
		return "", fmt.Errorf("read the Git project root: %w", err)
	}
	return root, nil
}

// fingerprintRoot returns the repository root when Git is available and the
// requested directory itself when it is not a repository. The latter is the
// deliberate L3 fallback: a manual project identity can still synchronize the
// files a session touched, but it cannot claim Git-wide dirty-file coverage.
// Any other Git failure remains an error so a damaged or inaccessible
// repository is never silently treated as clean.
func fingerprintRoot(ctx context.Context, dir string) (string, bool, error) {
	root, err := repositoryRoot(ctx, dir)
	if err == nil {
		return root, true, nil
	}
	if !isNonRepositoryError(err) {
		return "", false, err
	}
	root, err = absoluteDirectory(dir)
	if err != nil {
		return "", false, fmt.Errorf("locate non-Git project: %w", err)
	}
	return root, false, nil
}

func isNonRepositoryError(err error) bool {
	if gitUnavailable(err) {
		return true
	}
	var gitErr *gitError
	if !errors.As(err, &gitErr) {
		return false
	}
	message := strings.ToLower(gitErr.stderr)
	return strings.Contains(message, "not a git repository") ||
		strings.Contains(message, "not a repository")
}

func headAndBranch(ctx context.Context, dir string) (string, string, error) {
	if _, err := repositoryRoot(ctx, dir); err != nil {
		return "", "", err
	}

	head, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if noHead(err) {
			head = ""
		} else {
			return "", "", err
		}
	}
	branch, err := runGit(ctx, dir, "branch", "--show-current")
	if err != nil {
		return "", "", err
	}
	return head, branch, nil
}

func noHead(err error) bool {
	var ge *gitError
	if !errors.As(err, &ge) {
		return false
	}
	message := strings.TrimSpace(ge.stderr)
	return message == "" || strings.Contains(message, "Needed a single revision")
}

func dirtyPaths(ctx context.Context, root string) ([]string, error) {
	data, err := runGitRaw(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parsePorcelainZ(data)
}

func parsePorcelainZ(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("project: Git status output was truncated")
	}

	fields := bytes.Split(data, []byte{0})
	var paths []string
	for i := 0; i < len(fields)-1; i++ {
		field := fields[i]
		if len(field) < 4 || field[2] != ' ' {
			return nil, errors.New("project: Git status output was malformed")
		}
		path := string(field[3:])
		if path == "" {
			return nil, errors.New("project: Git status output contained an empty path")
		}
		paths = append(paths, filepath.ToSlash(path))

		if isRenameStatus(field[0]) || isRenameStatus(field[1]) {
			i++
			if i >= len(fields)-1 || len(fields[i]) == 0 {
				return nil, errors.New("project: Git status output omitted a rename path")
			}
			paths = append(paths, filepath.ToSlash(string(fields[i])))
		}
	}
	return uniqueSorted(paths), nil
}

func isRenameStatus(status byte) bool {
	return status == 'R' || status == 'C'
}

func relativeTouched(root, base string, touched []string) []string {
	paths := make([]string, 0, len(touched))
	for _, value := range touched {
		if value == "" {
			continue
		}
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		if relative, ok := relativePath(root, path); ok {
			paths = append(paths, relative)
		}
	}
	return uniqueSorted(paths)
}

func relativePath(root, path string) (string, bool) {
	rootAbs, err := canonicalPath(root)
	if err != nil {
		return "", false
	}
	pathAbs, err := canonicalPath(path)
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return "", false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

// canonicalPath makes comparisons use filesystem identity where the platform
// exposes more than one spelling for the same location. The final path may be
// absent (a session can record a deleted file), so resolve its nearest
// existing parent when the full path cannot be evaluated.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}

	parent := filepath.Dir(abs)
	base := filepath.Base(abs)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return abs, nil
	}
	return filepath.Clean(filepath.Join(resolvedParent, base)), nil
}

func unionPaths(first, second []string) []string {
	paths := make([]string, 0, len(first)+len(second))
	paths = append(paths, first...)
	paths = append(paths, second...)
	return uniqueSorted(paths)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedDigestPaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func digestAll(ctx context.Context, root string, paths []string) (map[string]string, error) {
	return digestAllWithMode(ctx, root, paths, true)
}

func digestAllForFingerprint(ctx context.Context, root string, paths []string, gitBacked bool) (map[string]string, error) {
	return digestAllWithMode(ctx, root, paths, gitBacked)
}

func digestAllWithMode(ctx context.Context, root string, paths []string, gitBacked bool) (map[string]string, error) {
	result := make(map[string]string, len(paths))
	var regular, special []string
	for _, relative := range paths {
		relative, ok := cleanRelativePath(relative)
		if !ok {
			return nil, errors.New("project: fingerprint contains an unsafe path")
		}

		absolute := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Stat(absolute)
		switch {
		case errors.Is(err, os.ErrNotExist):
			result[relative] = digestAbsent
		case err != nil:
			return nil, fmt.Errorf("inspect a fingerprinted file: %w", pathSafe(err))
		case info.IsDir():
			result[relative] = digestDirectory
		case strings.ContainsAny(relative, "\r\n"):
			special = append(special, relative)
		default:
			regular = append(regular, relative)
		}
	}

	if gitBacked {
		if err := hashRegularPaths(ctx, root, regular, result); err != nil {
			return nil, err
		}
		for _, relative := range special {
			digest, err := hashOnePath(ctx, root, relative)
			if err != nil {
				return nil, err
			}
			result[relative] = digest
		}
	} else {
		for _, relative := range append(append([]string(nil), regular...), special...) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			digest, err := rawBlobDigest(filepath.Join(root, filepath.FromSlash(relative)))
			if err != nil {
				return nil, fmt.Errorf("hash a workspace file: %w", err)
			}
			result[relative] = digest
		}
	}
	return result, nil
}

func cleanRelativePath(value string) (string, bool) {
	if value == "" || strings.ContainsRune(value, 0) {
		return "", false
	}
	value = filepath.ToSlash(value)
	if strings.HasPrefix(value, "/") {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", false
	}
	return clean, true
}

func hashRegularPaths(ctx context.Context, root string, paths []string, result map[string]string) error {
	if len(paths) == 0 {
		return nil
	}
	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte('\n')
	}
	data, err := runGitInput(ctx, root, input.Bytes(), "hash-object", "--stdin-paths")
	if err != nil {
		return fmt.Errorf("hash workspace files: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if len(lines) != len(paths) {
		return errors.New("project: Git returned an incomplete workspace digest")
	}
	for i, line := range lines {
		if len(line) != sha1HexLength {
			return errors.New("project: Git returned an invalid workspace digest")
		}
		result[paths[i]] = line
	}
	return nil
}

const sha1HexLength = 40

func hashOnePath(ctx context.Context, root, relative string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("read a workspace file: %w", pathSafe(err))
	}
	data, err = runGitInput(ctx, root, data, "hash-object", "--path="+relative, "--stdin")
	if err != nil {
		return "", fmt.Errorf("hash a workspace file: %w", err)
	}
	digest := strings.TrimSpace(string(data))
	if len(digest) != sha1HexLength {
		return "", errors.New("project: Git returned an invalid workspace digest")
	}
	return digest, nil
}

func rawBlobDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", pathSafe(err)
	}
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(data))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// rawDigest is a content-only fallback for paths Git cannot carry through its
// line-oriented batch input, such as a filename containing a newline.
func rawDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", pathSafe(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func isAncestor(ctx context.Context, root, ancestor, descendant string) bool {
	if ctx.Err() != nil || ancestor == "" || descendant == "" {
		return false
	}
	_, err := runGitRaw(ctx, root, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func changedBetween(ctx context.Context, root, from, to, relative string) bool {
	if ctx.Err() != nil || from == "" || to == "" {
		return false
	}
	_, err := runGitRaw(ctx, root, "diff", "--quiet", from, to, "--", relative)
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}
