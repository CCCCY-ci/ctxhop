package project

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// sessionRepo builds a repository with a committed file the session touched,
// and returns the repo and the touched set as the adapter would supply it.
func sessionRepo(t *testing.T) (repo string, touched []string) {
	t.Helper()
	repo = newRepo(t)
	write(t, filepath.Join(repo, "src", "app.go"), "package app\n")
	write(t, filepath.Join(repo, "src", "util.go"), "package app\n\nfunc U() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "session work")
	return repo, []string{filepath.Join(repo, "src", "app.go")}
}

func capture(t *testing.T, repo string, touched []string) Fingerprint {
	t.Helper()
	fp, err := Capture(context.Background(), repo, touched)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return fp
}

func compare(t *testing.T, repo string, fp Fingerprint) Report {
	t.Helper()
	r, err := Compare(context.Background(), repo, fp)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return r
}

// TestScenarioMatrix reproduces PoC-2 §4. The three cases expected to stay
// quiet are the point of the whole design: a check that cries about unrelated
// edits is one users learn to ignore.
func TestScenarioMatrix(t *testing.T) {
	cases := map[string]struct {
		// before runs while the session is still notionally in progress, so a
		// scenario can leave uncommitted work for the fingerprint to record.
		before func(t *testing.T, repo string)
		change func(t *testing.T, repo string)
		want   Verdict
	}{
		"nothing changed": {
			change: func(*testing.T, string) {},
			want:   Consistent,
		},
		"an untouched file was edited": {
			change: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "src", "util.go"), "package app\n\nfunc U() { return }\n")
			},
			want: Consistent,
		},
		"five unrelated files appeared": {
			change: func(t *testing.T, repo string) {
				for _, n := range []string{"a", "b", "c", "d", "e"} {
					write(t, filepath.Join(repo, "notes", n+".md"), "new\n")
				}
			},
			want: Consistent,
		},
		"the session's own uncommitted work was committed": {
			before: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc WrittenBySession() {}\n")
			},
			change: func(t *testing.T, repo string) {
				git(t, repo, "add", "-A")
				git(t, repo, "commit", "-qm", "commit the session's work")
				write(t, filepath.Join(repo, "unrelated.txt"), "later\n")
				git(t, repo, "add", "-A")
				git(t, repo, "commit", "-qm", "later work")
			},
			want: Consistent,
		},
		"a touched file was edited": {
			change: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc New() {}\n")
			},
			want: Divergent,
		},
		"a touched file was reformatted": {
			change: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "src", "app.go"), "package app\n\n")
			},
			want: Divergent,
		},
		"a touched file was deleted": {
			change: func(t *testing.T, repo string) {
				if err := os.Remove(filepath.Join(repo, "src", "app.go")); err != nil {
					t.Fatal(err)
				}
			},
			want: Divergent,
		},
		"later commits changed a touched file": {
			change: func(t *testing.T, repo string) {
				write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc Later() {}\n")
				git(t, repo, "add", "-A")
				git(t, repo, "commit", "-qm", "later change to a touched file")
			},
			want: Explainable,
		},
		"the branch does not contain the session's starting point": {
			change: func(t *testing.T, repo string) {
				git(t, repo, "checkout", "-q", "-b", "other", "HEAD~1")
			},
			want: Divergent,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo, touched := sessionRepo(t)
			if tc.before != nil {
				tc.before(t, repo)
			}
			fp := capture(t, repo, touched)

			tc.change(t, repo)

			got := compare(t, repo, fp)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %v, want %v (files: %+v)", got.Verdict, tc.want, got.Files)
			}
		})
	}
}

// TestExplainableStillNamesTheFiles: the difference is accounted for, but the
// agent's picture of that file is stale all the same, and a bare "fine" would
// hide it (PoC-2 §4.1).
func TestExplainableStillNamesTheFiles(t *testing.T) {
	repo, touched := sessionRepo(t)
	fp := capture(t, repo, touched)

	write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc Later() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "later")

	report := compare(t, repo, fp)
	if report.Verdict != Explainable {
		t.Fatalf("verdict = %v", report.Verdict)
	}
	if len(report.Files) == 0 {
		t.Fatal("an explainable verdict listed no files")
	}
	if report.Files[0].Path != "src/app.go" {
		t.Errorf("listed %q", report.Files[0].Path)
	}
	if report.Files[0].Note == "" {
		t.Error("no explanation was given")
	}
}

// TestCommittedAndThenEditedAgainIsNotExplainable tightens PoC-2: a file that
// commits changed *and* that has uncommitted edits on top is not accounted for
// by those commits, and saying it is would overstate what is known.
func TestCommittedAndThenEditedAgainIsNotExplainable(t *testing.T) {
	repo, touched := sessionRepo(t)
	fp := capture(t, repo, touched)

	write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc Later() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "later")
	write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc EvenLater() {}\n")

	report := compare(t, repo, fp)
	if report.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent: commits do not explain an uncommitted edit", report.Verdict)
	}
}

// TestNonAsciiFilenamesAreSeen is the regression test for the escaping trap.
// Without -z, git reports 中文名.txt as "\344\270\255\346\226\207\345\220\215.txt",
// which matches no real filename - so a modified file would be silently
// reported as unchanged, and the check would quietly always say yes
// (spec §4.1).
func TestNonAsciiFilenamesAreSeen(t *testing.T) {
	repo := newRepo(t)
	awkward := []string{"中文名.txt", "with space.txt", "café.txt", "emoji-🙂.txt"}
	for _, name := range awkward {
		write(t, filepath.Join(repo, name), "original\n")
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "awkward names")

	var touched []string
	for _, name := range awkward {
		touched = append(touched, filepath.Join(repo, name))
	}
	fp := capture(t, repo, touched)

	// Every one of them must have been fingerprinted under its real name.
	for _, name := range awkward {
		if _, ok := fp.Files[name]; !ok {
			t.Errorf("%q was not fingerprinted; keys are %v", name, sortedKeys(fp.Files))
		}
	}

	for _, name := range awkward {
		write(t, filepath.Join(repo, name), "changed\n")
	}

	report := compare(t, repo, fp)
	if report.Verdict != Divergent {
		t.Fatalf("verdict = %v, want Divergent: every file was modified", report.Verdict)
	}
	if len(report.Files) != len(awkward) {
		t.Errorf("reported %d files, want %d: %+v", len(report.Files), len(awkward), report.Files)
	}
}

// TestDirtySetSeesShellChanges is why the fingerprint is anchored on git: a
// change the session never recorded still has to be caught (PoC-2 §3).
func TestDirtySetSeesShellChanges(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "touched-by-shell.txt"), "shell wrote this\n")

	fp := capture(t, repo, nil)
	if !slices.Contains(fp.Dirty, "touched-by-shell.txt") {
		t.Fatalf("the dirty set missed an untracked file: %v", fp.Dirty)
	}
	if _, ok := fp.Files["touched-by-shell.txt"]; !ok {
		t.Error("a dirty file was not fingerprinted")
	}

	write(t, filepath.Join(repo, "touched-by-shell.txt"), "changed again\n")
	if got := compare(t, repo, fp); got.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", got.Verdict)
	}
}

// TestRenamesReportBothPaths. In -z mode the new path arrives first and the old
// one follows as a separate field - the opposite order from the readable form,
// which is an easy way to record the wrong one (spec §4.1).
func TestRenamesReportBothPaths(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "before.txt"), "content\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "add")
	git(t, repo, "mv", "before.txt", "after.txt")

	dirty, err := dirtyPaths(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"before.txt", "after.txt"} {
		if !slices.Contains(dirty, want) {
			t.Errorf("the dirty set %v is missing %q", dirty, want)
		}
	}
}

// TestAFilenameContainingAnArrowIsNotARename guards the parser against the
// readable format's separator appearing inside a real name.
//
// Synthetic input is faithful here, unlike for the escaping tests: -z emits the
// name raw, so these bytes are exactly what git produces. Windows forbids '>'
// in filenames, so this is the only way to cover it on every platform.
func TestAFilenameContainingAnArrowIsNotARename(t *testing.T) {
	dirty, err := parsePorcelainZ([]byte("?? a -> b.txt\x00 M plain.txt\x00"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a -> b.txt", "plain.txt"} {
		if !slices.Contains(dirty, want) {
			t.Errorf("the dirty set %v is missing %q", dirty, want)
		}
	}
}

// TestAnArrowInARealFilename runs the same case through git itself where the
// filesystem permits it, since only that proves the -z output really is raw.
func TestAnArrowInARealFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows forbids '>' in filenames")
	}
	repo := newRepo(t)
	write(t, filepath.Join(repo, "a -> b.txt"), "content\n")

	dirty, err := dirtyPaths(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(dirty, "a -> b.txt") {
		t.Errorf("the dirty set %v lost a filename containing an arrow", dirty)
	}
}

func TestParsePorcelainZRejectsTruncatedOutput(t *testing.T) {
	// A rename that promises an origin path and does not deliver one must not
	// be read as a file named after the status column.
	if _, err := parsePorcelainZ([]byte("R  after.txt\x00")); err == nil {
		t.Error("a truncated rename entry was accepted")
	}
	if _, err := parsePorcelainZ([]byte("x\x00")); err == nil {
		t.Error("a malformed entry was accepted")
	}
}

// TestFingerprintSurvivesStorage: it is captured on one machine, encrypted,
// uploaded, and compared on another.
func TestFingerprintSurvivesStorage(t *testing.T) {
	repo, touched := sessionRepo(t)
	write(t, filepath.Join(repo, "中文 name.txt"), "x\n")
	fp := capture(t, repo, touched)

	data, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	var back Fingerprint
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}

	if got := compare(t, repo, back); got.Verdict != Consistent {
		t.Errorf("a round-tripped fingerprint reported %v: %+v", got.Verdict, got.Files)
	}

	// Paths must travel in slash form, or a fingerprint taken on Windows would
	// never match anything on a POSIX machine.
	for path := range back.Files {
		if strings.ContainsRune(path, '\\') {
			t.Errorf("fingerprinted path %q carries a backslash", path)
		}
	}
	for _, p := range back.Dirty {
		if strings.ContainsRune(p, '\\') {
			t.Errorf("dirty path %q carries a backslash", p)
		}
	}
	if _, ok := back.Files["src/app.go"]; !ok {
		t.Errorf("paths are not slash-separated: %v", sortedKeys(back.Files))
	}
}

func TestPathsOutsideTheProjectAreDropped(t *testing.T) {
	// They cannot be compared on another machine, and guessing at a mapping is
	// exactly the speculative rewriting §9.3 forbids.
	repo, _ := sessionRepo(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	write(t, outside, "not part of the project\n")

	fp := capture(t, repo, []string{outside, filepath.Join(repo, "src", "app.go")})

	for path := range fp.Files {
		if filepath.IsAbs(path) || path == "elsewhere.txt" {
			t.Errorf("a path outside the project was fingerprinted: %q", path)
		}
	}
	if _, ok := fp.Files["src/app.go"]; !ok {
		t.Error("the project's own file was dropped")
	}
}

func TestCaptureAndCompareSupportsNonGitL3(t *testing.T) {
	plain := t.TempDir()
	touched := filepath.Join(plain, "notes.txt")
	write(t, touched, "initial\n")

	fp, err := Capture(context.Background(), plain, []string{touched})
	if err != nil {
		t.Fatalf("Capture without Git: %v", err)
	}
	if fp.Head != "" || fp.Branch != "" || len(fp.Dirty) != 0 {
		t.Fatalf("non-Git fingerprint carried Git state: %+v", fp)
	}
	if _, ok := fp.Files["notes.txt"]; !ok {
		t.Fatalf("non-Git fingerprint omitted touched file: %+v", fp.Files)
	}

	report, err := Compare(context.Background(), plain, fp)
	if err != nil {
		t.Fatalf("Compare without Git: %v", err)
	}
	if report.Verdict != Consistent {
		t.Fatalf("unchanged non-Git workspace = %v, want consistent", report.Verdict)
	}

	write(t, touched, "changed\n")
	report, err = Compare(context.Background(), plain, fp)
	if err != nil {
		t.Fatalf("Compare changed non-Git workspace: %v", err)
	}
	if report.Verdict != Divergent || len(report.Files) != 1 {
		t.Fatalf("changed non-Git workspace = %+v, want one divergent file", report)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report, err := Compare(ctx, plain, fp); err == nil && report.Verdict == Consistent {
		t.Error("a cancelled comparison reported consistency")
	}
}

func TestVerdictNames(t *testing.T) {
	for v, want := range map[Verdict]string{
		Consistent:  "consistent",
		Explainable: "explainable",
		Divergent:   "divergent",
	} {
		if v.String() != want {
			t.Errorf("%d.String() = %q, want %q", v, v.String(), want)
		}
	}
}

// TestAFileReplacedByADirectory. Reading it back would otherwise fail as "is a
// directory" and abort the whole comparison; a path whose very kind changed is
// a difference, not an error.
func TestAFileReplacedByADirectory(t *testing.T) {
	repo, touched := sessionRepo(t)
	fp := capture(t, repo, touched)

	app := filepath.Join(repo, "src", "app.go")
	if err := os.Remove(app); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}

	report := compare(t, repo, fp)
	if report.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", report.Verdict)
	}
}

// TestAFileTheSessionDeletedComingBack: absence has to be recorded, not
// omitted. A file the session deleted is still consistent while it stays gone,
// and a difference once it returns.
func TestAFileTheSessionDeletedComingBack(t *testing.T) {
	repo := newRepo(t)
	gone := filepath.Join(repo, "removed-by-session.txt")
	touched := []string{gone}

	fp := capture(t, repo, touched)
	if fp.Files["removed-by-session.txt"] != digestAbsent {
		t.Fatalf("an absent file was recorded as %q", fp.Files["removed-by-session.txt"])
	}

	if got := compare(t, repo, fp); got.Verdict != Consistent {
		t.Errorf("a file that stayed absent reported %v", got.Verdict)
	}

	write(t, gone, "someone put it back\n")
	got := compare(t, repo, fp)
	if got.Verdict != Divergent {
		t.Fatalf("a file that came back reported %v", got.Verdict)
	}
	if len(got.Files) != 1 || got.Files[0].Note == "" {
		t.Errorf("the returning file was not explained: %+v", got.Files)
	}
}

// TestCaptureFromASubdirectory covers the worst of the review findings. git
// reports status paths relative to the repository root whatever directory it
// was invoked from, so joining them onto a subdirectory produced paths that
// exist nowhere - "absent" on both sides, which compares equal, so the check
// reported consistency forever.
func TestCaptureFromASubdirectory(t *testing.T) {
	repo, _ := sessionRepo(t)
	sub := filepath.Join(repo, "src")

	fp, err := Capture(context.Background(), sub, []string{filepath.Join(repo, "src", "app.go")})
	if err != nil {
		t.Fatalf("Capture from a subdirectory: %v", err)
	}
	if _, ok := fp.Files["src/app.go"]; !ok {
		t.Fatalf("paths are not relative to the repository root: %v", sortedKeys(fp.Files))
	}
	for _, digest := range fp.Files {
		if digest == digestAbsent {
			t.Fatalf("a file present on disk was fingerprinted as absent: %v", fp.Files)
		}
	}

	write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc Changed() {}\n")

	report, err := Compare(context.Background(), sub, fp)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent; a real change went unnoticed", report.Verdict)
	}
}

// TestARepositoryWithNoCommitsYet: `rev-parse HEAD` exits 128 before the first
// commit, so the very first session of a new project could never have been
// fingerprinted.
func TestARepositoryWithNoCommitsYet(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	write(t, filepath.Join(repo, "start.txt"), "first\n")

	fp, err := Capture(context.Background(), repo, []string{filepath.Join(repo, "start.txt")})
	if err != nil {
		t.Fatalf("a repository with no commits could not be fingerprinted: %v", err)
	}
	if fp.Head != "" {
		t.Errorf("head = %q, want empty", fp.Head)
	}

	if got := compare(t, repo, fp); got.Verdict != Consistent {
		t.Errorf("verdict = %v, want Consistent", got.Verdict)
	}

	write(t, filepath.Join(repo, "start.txt"), "changed\n")
	if got := compare(t, repo, fp); got.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", got.Verdict)
	}
}

// TestFilesInsideAnUntrackedDirectoryAreSeen. git collapses an untracked
// directory into a single entry ending in "/", so everything inside it would
// have been fingerprinted as one "directory" value and compared equal forever.
func TestFilesInsideAnUntrackedDirectoryAreSeen(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "scratch", "notes.md"), "original\n")
	write(t, filepath.Join(repo, "scratch", "deep", "more.md"), "original\n")

	fp := capture(t, repo, nil)
	for _, want := range []string{"scratch/notes.md", "scratch/deep/more.md"} {
		if _, ok := fp.Files[want]; !ok {
			t.Errorf("%q was not fingerprinted; got %v", want, sortedKeys(fp.Files))
		}
	}

	write(t, filepath.Join(repo, "scratch", "notes.md"), "changed\n")
	if got := compare(t, repo, fp); got.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", got.Verdict)
	}
}

// TestDigestsUseGitsNotionOfContent is what makes a fingerprint portable. With
// core.autocrlf=true, the Git for Windows default, the same commit checks out
// as CRLF on Windows and LF elsewhere; hashing raw bytes would report every
// text file as modified whenever a fingerprint crossed platforms.
func TestDigestsUseGitsNotionOfContent(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "core.autocrlf", "true")
	write(t, filepath.Join(repo, "text.txt"), "line1\nline2\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "add text")

	fp := capture(t, repo, []string{filepath.Join(repo, "text.txt")})

	// The blob git itself stored, which is identical on every platform.
	want := git(t, repo, "rev-parse", ":text.txt")
	if fp.Files["text.txt"] != want {
		t.Errorf("digest = %q, want the git blob %q", fp.Files["text.txt"], want)
	}
}

// TestPathspecsAreLiteral: a filename holding glob characters must not match
// its neighbours, or a commit touching the neighbour would explain away a real
// divergence.
func TestPathspecsAreLiteral(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "rep[1].md"), "original\n")
	write(t, filepath.Join(repo, "rep1.md"), "original\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "both")

	fp := capture(t, repo, []string{filepath.Join(repo, "rep[1].md")})

	// Change the fingerprinted file in the working tree, and commit a change to
	// its glob-neighbour. Only the neighbour is explained by a commit.
	write(t, filepath.Join(repo, "rep1.md"), "committed change\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "change the neighbour")
	write(t, filepath.Join(repo, "rep[1].md"), "uncommitted change\n")

	report := compare(t, repo, fp)
	if report.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent: the neighbour's commit is not an explanation", report.Verdict)
	}
}

func TestErrorsDoNotNamePaths(t *testing.T) {
	// These strings reach the user, and an absolute path names both the person
	// and what they work on (BR-09).
	secret := filepath.Join(t.TempDir(), "very-private-project")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Capture(context.Background(), secret, nil); err != nil {
		t.Fatalf("non-Git L3 capture failed: %v", err)
	}

	_, err := Identify(context.Background(), filepath.Join(secret, "missing-child"))
	if err == nil {
		t.Fatal("a missing directory was identified")
	}
	if strings.Contains(err.Error(), "very-private-project") {
		t.Errorf("the error names the project: %v", err)
	}
}

func TestRawDigestHashesContent(t *testing.T) {
	// The fallback for names git's --stdin-paths cannot carry. It still has to
	// produce a stable, content-dependent value.
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	write(t, a, "content\n")

	first, err := rawDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	again, err := rawDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("the same bytes hashed differently")
	}

	write(t, a, "different\n")
	changed, err := rawDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Error("different bytes hashed the same")
	}

	if _, err := rawDigest(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing file hashed successfully")
	} else if strings.Contains(err.Error(), "absent") {
		t.Errorf("the error names the path: %v", err)
	}
}

// TestAFilenameWithANewline exercises the fallback end to end where the
// filesystem allows such a name.
func TestAFilenameWithANewline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows forbids newlines in filenames")
	}
	repo := newRepo(t)
	name := "odd\nname.txt"
	write(t, filepath.Join(repo, name), "original\n")

	fp := capture(t, repo, []string{filepath.Join(repo, name)})
	if _, ok := fp.Files[name]; !ok {
		t.Fatalf("the file was not fingerprinted: %v", sortedKeys(fp.Files))
	}

	write(t, filepath.Join(repo, name), "changed\n")
	if got := compare(t, repo, fp); got.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", got.Verdict)
	}
}

// TestADetachedHeadStillWorks: agents run wherever the user left the
// repository, and a detached head is an ordinary place to be.
func TestADetachedHeadStillWorks(t *testing.T) {
	repo, touched := sessionRepo(t)
	git(t, repo, "checkout", "-q", "--detach")

	fp := capture(t, repo, touched)
	if fp.Head == "" {
		t.Error("no head was recorded on a detached checkout")
	}
	if fp.Branch != "" {
		t.Errorf("branch = %q, want empty rather than the word HEAD", fp.Branch)
	}

	if got := compare(t, repo, fp); got.Verdict != Consistent {
		t.Errorf("verdict = %v, want Consistent", got.Verdict)
	}

	write(t, filepath.Join(repo, "src", "app.go"), "package app\n\nfunc Changed() {}\n")
	if got := compare(t, repo, fp); got.Verdict != Divergent {
		t.Errorf("verdict = %v, want Divergent", got.Verdict)
	}
}

// TestARepositoryMidRebase is required by spec §8.4: the check must survive a
// repository caught in the middle of an operation rather than reporting
// something confident about it.
func TestARepositoryMidRebase(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "conflict.txt"), "base\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base")

	git(t, repo, "checkout", "-q", "-b", "side")
	write(t, filepath.Join(repo, "conflict.txt"), "side\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "side change")

	git(t, repo, "checkout", "-q", "main")
	write(t, filepath.Join(repo, "conflict.txt"), "main\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "main change")

	// Deliberately expected to fail: this leaves the repository mid-rebase.
	rebase := exec.Command("git", "-c", "user.email=t@t", "-c", "user.name=T", "rebase", "side")
	rebase.Dir = repo
	if out, err := rebase.CombinedOutput(); err == nil {
		t.Fatalf("the rebase was supposed to conflict: %s", out)
	}

	fp, err := Capture(context.Background(), repo, []string{filepath.Join(repo, "conflict.txt")})
	if err != nil {
		t.Fatalf("a repository mid-rebase could not be fingerprinted: %v", err)
	}
	if !slices.Contains(fp.Dirty, "conflict.txt") {
		t.Errorf("the conflicted file is not in the dirty set: %v", fp.Dirty)
	}
}

// TestTheProjectVanishingMidCheck is spec §8.4's other case. Whatever happens,
// it must not read as consistency.
func TestTheProjectVanishingMidCheck(t *testing.T) {
	repo, touched := sessionRepo(t)
	fp := capture(t, repo, touched)

	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}

	report, err := Compare(context.Background(), repo, fp)
	if err == nil && report.Verdict == Consistent {
		t.Error("a repository that stopped being one reported consistency")
	}
}

// TestNoGitStepReturnsABenignZeroValue keeps the low-level Git helpers fail
// closed while the public fingerprint API falls back to L3 when Git is absent.
func TestNoGitStepReturnsABenignZeroValue(t *testing.T) {
	repo := newRepo(t)
	file := filepath.Join(repo, "f.txt")
	write(t, file, "x\n")
	ctx := context.Background()

	// A PATH with no git on it, restored by t.Setenv when the test ends.
	t.Setenv("PATH", t.TempDir())

	if head, branch, err := headAndBranch(ctx, repo); err == nil {
		t.Errorf("headAndBranch succeeded without git: head=%q branch=%q", head, branch)
	}
	if dirty, err := dirtyPaths(ctx, repo); err == nil {
		t.Errorf("dirtyPaths succeeded without git: %v", dirty)
	}
	if digests, err := digestAll(ctx, repo, []string{"f.txt"}); err == nil {
		t.Errorf("digestAll succeeded without git: %v", digests)
	}
	fp, err := Capture(ctx, repo, []string{file})
	if err != nil {
		t.Fatalf("L3 Capture failed without git: %v", err)
	}
	if fp.Head != "" || fp.Branch != "" || fp.Files["f.txt"] == "" {
		t.Fatalf("L3 fingerprint without git = %+v", fp)
	}

	// These two answer false rather than erroring, which is safe in one
	// direction only: it can downgrade a verdict to Divergent, never lift one
	// to Consistent.
	if isAncestor(ctx, repo, "HEAD", "HEAD") {
		t.Error("isAncestor claimed ancestry without git")
	}
	if changedBetween(ctx, repo, "HEAD~1", "HEAD", "f.txt") {
		t.Error("changedBetween claimed a change without git")
	}
}

// TestTouchedPathsOnAnotherVolumeAreDropped. filepath.Rel fails outright across
// Windows drive letters, so this is not the same code path as a sibling
// directory outside the project.
func TestTouchedPathsOnAnotherVolumeAreDropped(t *testing.T) {
	repo, _ := sessionRepo(t)

	elsewhere := "/somewhere/else/file.txt"
	if runtime.GOOS == "windows" {
		// A drive that is not the one the temp directory lives on.
		elsewhere = `Z:\somewhere\else\file.txt`
		if strings.HasPrefix(strings.ToUpper(repo), "Z:") {
			elsewhere = `Y:\somewhere\else\file.txt`
		}
	}

	fp, err := Capture(context.Background(), repo, []string{elsewhere, filepath.Join(repo, "src", "app.go")})
	if err != nil {
		t.Fatalf("a path on another volume broke the capture: %v", err)
	}
	for path := range fp.Files {
		if strings.Contains(path, "somewhere") {
			t.Errorf("a path outside the project was fingerprinted: %q", path)
		}
	}
	if _, ok := fp.Files["src/app.go"]; !ok {
		t.Error("the project's own file was dropped")
	}
}
