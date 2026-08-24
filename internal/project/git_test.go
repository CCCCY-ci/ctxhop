package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWithoutGitTheAnswerIsNoIdentityNotAnError. A machine without git is a
// machine where cross-device matching does not apply; treating it as a fault
// would make doctor shout about something the user may not care about.
func TestWithoutGitTheAnswerIsNoIdentityNotAnError(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("PATH", t.TempDir())

	p, err := Identify(context.Background(), repo)
	if err != nil {
		t.Fatalf("a missing git produced an error: %v", err)
	}
	if p.Identity.Stable() {
		t.Error("an identity was produced without git")
	}
	if !strings.Contains(p.Reason, "git") {
		t.Errorf("the reason should say git is missing: %q", p.Reason)
	}
	if p.Root == "" {
		t.Error("the directory should still be reported")
	}
}

// TestNoGitSubcommandTouchesTheNetwork guards a redline rather than a
// behaviour: apart from the storage backend the user configured, the program
// must not reach the network at all (P7, tooling.md §3.1). `git fetch` would be
// exactly that, and it is one careless edit away at any time - so the guard is
// on the source, where a future change would trip it.
func TestNoGitSubcommandTouchesTheNetwork(t *testing.T) {
	forbidden := []string{"fetch", "ls-remote", "pull", "push", "clone", "submodule update"}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, word := range forbidden {
			if bytes.Contains(src, []byte(`"`+word+`"`)) {
				t.Errorf("%s passes %q to git, which would reach the network", name, word)
			}
		}
	}
}

// TestEveryInvocationRefusesToWriteTheIndex. `git status` refreshes and rewrites
// .git/index on what is meant to be a read; measured on git 2.55. The user's
// repository is not ours to modify (spec §5.1).
func TestEveryInvocationRefusesToWriteTheIndex(t *testing.T) {
	repo := newRepo(t)
	index := filepath.Join(repo, ".git", "index")

	// Make the working tree stale so a refresh would have something to write.
	write(t, filepath.Join(repo, "README.md"), "changed\n")
	if err := os.Chtimes(filepath.Join(repo, "README.md"), timeInFuture(), timeInFuture()); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGitRaw(context.Background(), repo, "status", "--porcelain", "-z"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}

	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("reading status rewrote the repository's index")
	}
}

func TestRunGitHonoursACancelledContext(t *testing.T) {
	repo := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := runGit(ctx, repo, "status", "--porcelain"); err == nil {
		t.Error("a cancelled context still ran git")
	}
}

func TestRunGitKeepsTheInheritedEnvironment(t *testing.T) {
	// Replacing the environment instead of adding to it would strip PATH and,
	// on Windows, SystemRoot - git needs both to find its own helpers.
	repo := newRepo(t)
	if _, err := runGit(context.Background(), repo, "rev-parse", "--show-toplevel"); err != nil {
		t.Fatalf("git could not run with the environment we give it: %v", err)
	}
}

func TestSplitNUL(t *testing.T) {
	got := splitNUL([]byte("a\x00b b\x00\x00中文\x00"))
	want := []string{"a", "b b", "中文"}

	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(splitNUL(nil)) != 0 {
		t.Error("empty output produced fields")
	}
}

func timeInFuture() time.Time { return time.Now().Add(2 * time.Second) }

// TestGitErrorNeverRendersStderr is the entire reason gitError exists. git
// prints paths, branch names and sometimes remote addresses on stderr, and this
// text reaches the user (BR-09, code_style §5.2).
func TestGitErrorNeverRendersStderr(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "very-private-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Not a repository, so git fails and says so on stderr, naming the path.
	_, err := runGit(context.Background(), dir, "rev-parse", "--is-bare-repository")
	if err == nil {
		t.Fatal("expected git to refuse a plain directory")
	}

	var ge *gitError
	if !errors.As(err, &ge) {
		t.Fatalf("got %T, want *gitError", err)
	}
	if ge.stderr == "" {
		t.Fatal("stderr was not captured, so this test proves nothing")
	}
	for _, leak := range []string{"very-private-project", dir, ge.stderr} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("Error() leaks %q: %v", leak, err)
		}
	}
	// Formatting verbs must not reach the struct fields either.
	if strings.Contains(fmt.Sprintf("%v|%s|%+v", err, err, err), "very-private-project") {
		t.Error("a formatting verb exposed the path")
	}
}

func TestPathSafeLeavesOtherErrorsAlone(t *testing.T) {
	plain := errors.New("nothing to strip")
	if got := pathSafe(plain); got != plain {
		t.Errorf("pathSafe rewrote an unrelated error: %v", got)
	}

	// And it keeps the cause identifiable, or callers lose errors.Is.
	_, err := os.Stat(filepath.Join(t.TempDir(), "absent"))
	safe := pathSafe(err)
	if !errors.Is(safe, os.ErrNotExist) {
		t.Errorf("pathSafe lost the cause: %v", safe)
	}
	if strings.Contains(safe.Error(), "absent") {
		t.Errorf("pathSafe kept the path: %v", safe)
	}
}

func TestDubiousOwnershipOnlyMatchesGitErrors(t *testing.T) {
	// The guard must not misread an unrelated failure as an ownership refusal,
	// which would send the user to edit safe.directory for no reason.
	if dubiousOwnership(errors.New("something else entirely")) {
		t.Error("a plain error was read as an ownership refusal")
	}
	if dubiousOwnership(&gitError{subcommand: "status", stderr: "fatal: not a git repository"}) {
		t.Error("an unrelated git failure was read as an ownership refusal")
	}
	if !dubiousOwnership(&gitError{subcommand: "status", stderr: "fatal: detected dubious ownership in repository at ..."}) {
		t.Error("an ownership refusal was not recognised")
	}
}
