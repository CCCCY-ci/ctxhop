package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func remoteIdentity(t *testing.T, url string) Identity {
	t.Helper()
	value, err := CanonicalizeRemote(url)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{Kind: KindRemote, Value: value}
}

func TestLocateFindsTheProjectAmongCandidates(t *testing.T) {
	wanted := newRepo(t)
	git(t, wanted, "remote", "add", "origin", "https://github.com/user/example.git")
	other := newRepo(t)
	git(t, other, "remote", "add", "origin", "https://github.com/user/unrelated.git")
	plain := t.TempDir()

	id := remoteIdentity(t, "git@github.com:user/example.git")
	root, err := Resolve(context.Background(), id, []string{other, plain, wanted}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !sameDir(t, root, wanted) {
		t.Errorf("resolved to %q, want the matching repository", root)
	}
}

// TestABindingBeatsTheScan: the user's statement about where a project lives
// must win over anything inferred, or binding would not be a remedy for
// anything.
func TestABindingBeatsTheScan(t *testing.T) {
	scanned := newRepo(t)
	git(t, scanned, "remote", "add", "origin", "https://github.com/user/example.git")
	preferred := newRepo(t)
	git(t, preferred, "remote", "add", "origin", "https://github.com/user/example.git")

	id := remoteIdentity(t, "https://github.com/user/example.git")
	bindings := []Binding{{Identity: id.Value, LocalRoot: preferred}}

	root, err := Resolve(context.Background(), id, []string{scanned, preferred}, bindings)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !sameDir(t, root, preferred) {
		t.Errorf("resolved to %q, want the bound directory", root)
	}
}

// TestAStaleBindingIsAnErrorNotAFallback. Falling back to the scan would send
// sessions into a working tree the user did not choose, and writing them there
// cannot be undone (BR-12).
func TestAStaleBindingIsAnErrorNotAFallback(t *testing.T) {
	present := newRepo(t)
	git(t, present, "remote", "add", "origin", "https://github.com/user/example.git")
	gone := filepath.Join(t.TempDir(), "moved-away")

	id := remoteIdentity(t, "https://github.com/user/example.git")
	bindings := []Binding{{Identity: id.Value, LocalRoot: gone}}

	if _, err := Resolve(context.Background(), id, []string{present}, bindings); err == nil {
		t.Fatal("a binding pointing nowhere silently fell back to the scan")
	}
}

// TestTwoWorktreesRefuseToResolve is the case §3.1 exists for: both are real,
// both are correct, and choosing between them is not the tool's to make.
func TestTwoWorktreesRefuseToResolve(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-q", "-b", "side", linked)

	id := remoteIdentity(t, "https://github.com/user/example.git")
	candidates := []string{repo, linked}

	roots, err := Locate(context.Background(), id, candidates, nil)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("Locate found %d roots, want both worktrees", len(roots))
	}

	if _, err := Resolve(context.Background(), id, candidates, nil); !errors.Is(err, ErrAmbiguousProject) {
		t.Errorf("got %v, want ErrAmbiguousProject", err)
	}

	// And a binding is the way out of it.
	bindings := []Binding{{Identity: id.Value, LocalRoot: linked}}
	root, err := Resolve(context.Background(), id, candidates, bindings)
	if err != nil {
		t.Fatalf("binding did not resolve the ambiguity: %v", err)
	}
	if !sameDir(t, root, linked) {
		t.Errorf("resolved to %q, want the bound worktree", root)
	}
}

func TestLocateReportsWhenTheProjectIsNotHere(t *testing.T) {
	elsewhere := newRepo(t)
	git(t, elsewhere, "remote", "add", "origin", "https://github.com/user/other.git")

	id := remoteIdentity(t, "https://github.com/user/example.git")
	if _, err := Locate(context.Background(), id, []string{elsewhere}, nil); !errors.Is(err, ErrProjectNotHere) {
		t.Errorf("got %v, want ErrProjectNotHere", err)
	}
}

func TestLocateRefusesAnIdentityThatCannotMatch(t *testing.T) {
	// Searching for a project with no cross-device identity would compare an
	// empty value against every candidate's empty value and match all of them.
	if _, err := Locate(context.Background(), Identity{Kind: KindNone}, []string{t.TempDir()}, nil); err == nil {
		t.Error("an unstable identity was used to search")
	}
}

func TestLocateSkipsCandidatesItCannotRead(t *testing.T) {
	wanted := newRepo(t)
	git(t, wanted, "remote", "add", "origin", "https://github.com/user/example.git")
	missing := filepath.Join(t.TempDir(), "not-there")

	id := remoteIdentity(t, "https://github.com/user/example.git")
	root, err := Resolve(context.Background(), id, []string{missing, wanted}, nil)
	if err != nil {
		t.Fatalf("a vanished candidate broke the search: %v", err)
	}
	if !sameDir(t, root, wanted) {
		t.Errorf("resolved to %q", root)
	}
}

// TestTheSameDirectoryIsNotCountedTwice: a directory reached once through a
// binding and once through the scan would otherwise look like an ambiguity and
// refuse to resolve.
func TestTheSameDirectoryIsNotCountedTwice(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")

	id := remoteIdentity(t, "https://github.com/user/example.git")
	spellings := []Binding{
		{Identity: id.Value, LocalRoot: repo},
		{Identity: id.Value, LocalRoot: filepath.Join(repo, ".")},
		{Identity: id.Value, LocalRoot: filepath.Join(repo, "sub", "..")},
	}

	root, err := Resolve(context.Background(), id, []string{repo}, spellings)
	if err != nil {
		t.Fatalf("one directory spelled three ways was treated as several: %v", err)
	}
	if !sameDir(t, root, repo) {
		t.Errorf("resolved to %q", root)
	}
}

func TestABoundPathThatIsAFileIsRefused(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := remoteIdentity(t, "https://github.com/user/example.git")

	if _, err := Locate(context.Background(), id, nil, []Binding{{Identity: id.Value, LocalRoot: file}}); err == nil {
		t.Error("a file was accepted as a project root")
	}
}

// TestAnInterruptedScanIsNotAnAbsence. Skipping every candidate that errored
// turned a cancelled or timed-out search into "no directory on this device
// matches" - a confident negative about candidates nobody managed to look at.
// The package draws exactly this distinction elsewhere.
func TestAnInterruptedScanIsNotAnAbsence(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "remote", "add", "origin", "https://github.com/user/example.git")
	id := remoteIdentity(t, "https://github.com/user/example.git")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Locate(ctx, id, []string{repo}, nil)
	if err == nil {
		t.Fatal("a cancelled scan reported a result")
	}
	if errors.Is(err, ErrProjectNotHere) {
		t.Errorf("a cancelled scan reported the project absent: %v", err)
	}
}
