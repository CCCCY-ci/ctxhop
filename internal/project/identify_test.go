package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs a command in dir and fails the test if it does not succeed. Fixtures
// are real repositories: a hand-built .git directory would let this package
// agree with a model of git rather than with git.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	full := append([]string{
		"-c", "user.email=test@example.invalid",
		"-c", "user.name=Test",
		"-c", "init.defaultBranch=main",
		"-c", "commit.gpgsign=false",
		"-c", "protocol.file.allow=always",
	}, args...)

	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimRight(string(out), "\r\n")
}

// newRepo returns an initialised repository with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "README.md"), "fixture\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "initial")
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sameDir compares paths by identity rather than by string. git resolves
// symlinks and normalises case, so the temp directory it reports back is often
// spelled differently from the one the test created.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func identify(t *testing.T, dir string) Project {
	t.Helper()
	p, err := Identify(context.Background(), dir)
	if err != nil {
		t.Fatalf("Identify(%s): %v", filepath.Base(dir), err)
	}
	return p
}

func TestIdentifyUsesTheCanonicalRemote(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "git@github.com:user/example.git")

	p := identify(t, repo)
	if p.Identity.Kind != KindRemote {
		t.Fatalf("kind = %v, reason = %q", p.Identity.Kind, p.Reason)
	}
	if p.Identity.Value != "github.com/user/example" {
		t.Errorf("identity = %q", p.Identity.Value)
	}
	if !sameDir(t, p.Root, repo) {
		t.Errorf("root = %q, want the repository root", p.Root)
	}
}

// TestIdentifyFromASubdirectoryFindsTheRoot matters because the agent runs
// wherever the user happens to be, which is rarely the top of the repository.
func TestIdentifyFromASubdirectoryFindsTheRoot(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")
	deep := filepath.Join(repo, "src", "internal", "pkg")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	p := identify(t, deep)
	if !sameDir(t, p.Root, repo) {
		t.Errorf("root = %q, want the repository root", p.Root)
	}
	if p.Identity.Value != "github.com/user/example" {
		t.Errorf("identity = %q", p.Identity.Value)
	}
}

func TestIdentifyPrefersOrigin(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "upstream", "https://github.com/upstream/example.git")
	git(t, repo, "remote", "add", "origin", "https://github.com/me/example.git")

	p := identify(t, repo)
	if p.Identity.Value != "github.com/me/example" {
		t.Errorf("identity = %q, want the origin remote", p.Identity.Value)
	}
}

func TestIdentifyUsesASoleRemoteWhateverItIsCalled(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "upstream", "https://github.com/upstream/example.git")

	p := identify(t, repo)
	if p.Identity.Value != "github.com/upstream/example" {
		t.Errorf("identity = %q", p.Identity.Value)
	}
}

// TestIdentifyRefusesToPickBetweenRemotes is the BR-12 case: guessing would
// restore one project's sessions into another, and there is no way to undo
// having written them.
func TestIdentifyRefusesToPickBetweenRemotes(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "alpha", "https://github.com/a/example.git")
	git(t, repo, "remote", "add", "beta", "https://github.com/b/example.git")

	p := identify(t, repo)
	if p.Identity.Stable() {
		t.Errorf("picked %q from two ambiguous remotes", p.Identity.Value)
	}
	if p.Reason == "" {
		t.Error("no reason was given for refusing")
	}
}

func TestDirectoriesWithNoAutomaticIdentity(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"not a repository": func(t *testing.T) string { return t.TempDir() },
		"no remote":        func(t *testing.T) string { return newRepo(t) },
		"local path remote": func(t *testing.T) string {
			repo := newRepo(t)
			other := newRepo(t)
			git(t, repo, "remote", "add", "origin", other)
			return repo
		},
		"bare repository": func(t *testing.T) string {
			dir := t.TempDir()
			git(t, dir, "init", "-q", "--bare")
			return dir
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			p := identify(t, build(t))
			if p.Identity.Stable() {
				t.Errorf("got identity %q, want none", p.Identity.Value)
			}
			if p.Reason == "" {
				t.Error("an unstable identity must explain itself")
			}
			if p.Root == "" {
				t.Error("root should still be reported")
			}
		})
	}
}

// TestReasonsAreSafeToPaste enforces BR-09: doctor output goes into public
// issues, so it must not carry paths, project names or remote addresses.
func TestReasonsAreSafeToPaste(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://ghp_TOKEN@github.com/secret-org/secret-app")
	git(t, repo, "remote", "add", "second", "https://github.com/secret-org/other")

	p := identify(t, repo)
	for _, leak := range []string{"secret-org", "secret-app", "ghp_TOKEN", repo, "github.com"} {
		if strings.Contains(p.Reason, leak) {
			t.Errorf("the reason %q leaks %q", p.Reason, leak)
		}
	}
}

func TestIdentifyRejectsWhatItCannotRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Identify(context.Background(), missing); err == nil {
		t.Error("a missing directory was identified")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	write(t, file, "x")
	if _, err := Identify(context.Background(), file); err == nil {
		t.Error("a file was identified as a project")
	}
}

// TestAWorktreeIsIdentifiedAtItsOwnRoot covers the case that later forces the
// mapping layer to allow several roots per identity: linked worktrees share
// the repository's remote (spec §3.1).
func TestAWorktreeIsIdentifiedAtItsOwnRoot(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-q", "-b", "side", linked)

	p := identify(t, linked)
	if p.Identity.Value != "github.com/user/example" {
		t.Errorf("identity = %q, want the same as the main worktree", p.Identity.Value)
	}
	if !sameDir(t, p.Root, linked) {
		t.Errorf("root = %q, want the linked worktree", p.Root)
	}
	if sameDir(t, p.Root, repo) {
		t.Error("the linked worktree resolved to the main worktree")
	}
}

// TestASubmoduleIsItsOwnProject: a submodule has its own remote, so sessions
// held in it belong to it and not to the containing repository.
func TestASubmoduleIsItsOwnProject(t *testing.T) {
	inner := newRepo(t)
	git(t, inner, "remote", "add", "origin", "https://github.com/user/library.git")

	outer := newRepo(t)
	git(t, outer, "remote", "add", "origin", "https://github.com/user/app.git")
	git(t, outer, "submodule", "-q", "add", filepath.ToSlash(inner), "vendor/library")

	// `submodule add` clones from wherever it was pointed, so the fixture's
	// origin would be the local path it was built from. A real checkout has the
	// address it was published under.
	sub := filepath.Join(outer, "vendor", "library")
	git(t, sub, "remote", "set-url", "origin", "https://github.com/user/library.git")

	p := identify(t, sub)
	if p.Identity.Value != "github.com/user/library" {
		t.Errorf("identity = %q, want the submodule's own remote", p.Identity.Value)
	}
	if !sameDir(t, p.Root, sub) {
		t.Errorf("root = %q, want the submodule's own directory", p.Root)
	}
}

// TestASubmoduleClonedFromAPathHasNoIdentity is the state the fixture above
// starts in, and it is worth keeping: a submodule whose origin is a path on
// this machine says nothing about any other machine, so it must be refused
// rather than turned into an identity no second device could reproduce.
func TestASubmoduleClonedFromAPathHasNoIdentity(t *testing.T) {
	inner := newRepo(t)
	outer := newRepo(t)
	git(t, outer, "remote", "add", "origin", "https://github.com/user/app.git")
	git(t, outer, "submodule", "-q", "add", filepath.ToSlash(inner), "vendor/library")

	p := identify(t, filepath.Join(outer, "vendor", "library"))
	if p.Identity.Stable() {
		t.Errorf("a local-path origin produced identity %q", p.Identity.Value)
	}
}

func TestManualIdentity(t *testing.T) {
	id, err := ManualIdentity("my-notes")
	if err != nil {
		t.Fatal(err)
	}
	if !id.Stable() || id.Kind != KindManual {
		t.Errorf("%+v is not a usable manual identity", id)
	}
	// It must be impossible for a hand-typed name to collide with a remote.
	remote, err := CanonicalizeRemote("https://github.com/user/example.git")
	if err != nil {
		t.Fatal(err)
	}
	if id.Value == remote || !strings.HasPrefix(id.Value, "manual:") {
		t.Errorf("manual identity %q is not distinguishable from a remote", id.Value)
	}

	if _, err := ManualIdentity("   "); err == nil {
		t.Error("a blank binding name was accepted")
	}
}

// TestATimeoutIsNotAnAnswer. Reporting a cancelled or timed-out git as "not a
// repository" would state something about a directory nobody managed to look
// at, and the CLI would then tell the user to bind by hand a project that is
// perfectly identifiable (spec §7.1).
func TestATimeoutIsNotAnAnswer(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p, err := Identify(ctx, repo)
	if err == nil {
		t.Fatalf("a cancelled lookup reported kind=%v reason=%q", p.Identity.Kind, p.Reason)
	}
}
