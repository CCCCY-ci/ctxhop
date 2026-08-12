package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Kind is how a project's identity was established (PRD §9.3).
type Kind int

const (
	// KindNone means no identity this build will match across devices. It is a
	// normal state, not a failure: a repository with no remote is an ordinary
	// thing to have, and reporting it as an error would make doctor noisy.
	KindNone Kind = iota
	// KindRemote is an identity derived from a canonicalized git remote.
	KindRemote
	// KindManual is an identity the user bound by hand.
	KindManual
)

// manualPrefix keeps hand-bound names from ever colliding with a canonical
// remote, which can never contain a colon before its first slash.
const manualPrefix = "manual:"

// Identity is the value every device must agree on for one project.
//
// It is plaintext. crypto.ProjectID turns it into the irreversible identifier
// that appears in remote paths; this value must never be written to storage or
// to diagnostics (P6, PRD §8.3).
type Identity struct {
	Kind  Kind
	Value string
}

// Stable reports whether this identity can match across devices on its own.
func (i Identity) Stable() bool { return i.Kind != KindNone }

// ManualIdentity builds the identity for a directory the user bound by name.
//
// Stability is the user's responsibility: the same name has to be entered on
// every device, which is why the CLI must show it back to them (spec §2.4).
func ManualIdentity(name string) (Identity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Identity{}, errors.New("project: a binding name is required")
	}
	return Identity{Kind: KindManual, Value: manualPrefix + name}, nil
}

// Project is what this layer knows about one directory.
type Project struct {
	// Root is the working tree root, or the directory itself when it is not a
	// repository.
	Root string
	// Identity is empty-kinded when nothing stable could be established.
	Identity Identity
	// Reason explains an unstable identity in terms the CLI can show.
	//
	// It must never name a remote, a project or an absolute path: doctor output
	// has to be safe to paste into a public issue (BR-09, code_style §5.2).
	Reason string
}

// Identify determines which project dir belongs to.
//
// The order of preference is fixed by PRD §9.3: a canonicalized git remote
// first, and otherwise nothing automatic. A repository with no usable remote,
// and any directory that is not a repository at all, must be bound by hand
// rather than guessed at - a wrong guess would restore one project's sessions
// into another (BR-12).
func Identify(ctx context.Context, dir string) (Project, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return Project{}, fmt.Errorf("the project directory cannot be read: %w", pathSafe(err))
	}
	if !info.IsDir() {
		return Project{}, fmt.Errorf("project: %s is not a directory", filepath.Base(dir))
	}

	if !gitAvailable() {
		return unstable(dir, "git was not found on PATH, so projects cannot be matched automatically"), nil
	}

	root, ok, err := worktreeRoot(ctx, dir)
	if err != nil {
		return Project{}, err
	}
	if !ok {
		return unstable(dir, "this directory is not inside a git repository"), nil
	}

	name, reason, err := chooseRemote(ctx, root)
	if err != nil {
		return Project{}, err
	}
	if name == "" {
		return unstable(root, reason), nil
	}

	url, err := runGit(ctx, root, "remote", "get-url", name)
	if err != nil {
		return Project{}, fmt.Errorf("read remote url: %w", err)
	}

	canonical, err := CanonicalizeRemote(url)
	if err != nil {
		if errors.Is(err, ErrNoRemoteIdentity) {
			// The reason deliberately does not quote the URL: it may carry a
			// token, and it names the user's project either way.
			return unstable(root, "the remote is not an address other devices could share"), nil
		}
		return Project{}, err
	}

	return Project{Root: root, Identity: Identity{Kind: KindRemote, Value: canonical}}, nil
}

func unstable(root, reason string) Project {
	return Project{Root: root, Identity: Identity{Kind: KindNone}, Reason: reason}
}

// worktreeRoot returns the working tree containing dir.
//
// A bare repository is reported as not a project: with no working tree there is
// nothing for an agent to have had a session in.
func worktreeRoot(ctx context.Context, dir string) (string, bool, error) {
	bare, err := runGit(ctx, dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		if unanswered(err) {
			return "", false, err
		}
		if dubiousOwnership(err) {
			// Exits 128 just like "not a repository", but binding by hand would
			// not help; the fix is a safe.directory entry.
			return "", false, errors.New("git refuses this repository as owned by another account; add it to safe.directory in your git config")
		}
		// git exiting non-zero here is an answer: this is not a repository. It
		// says so on stderr, which is not quoted for the reasons in runGit.
		return "", false, nil
	}
	if bare == "true" {
		return "", false, nil
	}

	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		if unanswered(err) {
			return "", false, err
		}
		return "", false, nil
	}
	if top == "" {
		return "", false, nil
	}
	// git reports forward slashes even on Windows; the rest of the program
	// compares these against filepath-built paths (code_style §3.4).
	return filepath.Clean(filepath.FromSlash(top)), true, nil
}

// pathSafe strips the filename out of a filesystem error while keeping its
// cause, so errors.Is still answers correctly.
//
// A *fs.PathError renders as "stat C:\Users\someone\projects\thing: ...". These
// errors reach the user, and an absolute path names both the person and what
// they work on (BR-09, code_style §5.2).
func pathSafe(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// unanswered reports a failure that means git never got to look, as opposed to
// git looking and saying no.
//
// The difference decides what may be concluded. "git exited non-zero" is an
// answer - this is not a repository. A timeout, a cancellation or a missing
// binary is not an answer at all, and turning it into "not a repository" would
// be a confident statement about something that was never examined. The same
// rule governs the consistency check, where the cost of getting it wrong is
// higher still (spec §7.1).
func unanswered(err error) bool {
	return errors.Is(err, ErrGitUnavailable) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// chooseRemote picks the remote whose URL becomes the identity.
//
// Preferring origin is a decision, not a fallback. Where origin is a fork and
// upstream is the canonical repository, treating the fork as its own project is
// a legitimate way to work, and the tool must not decide otherwise on the user's
// behalf (spec §2.3).
func chooseRemote(ctx context.Context, root string) (name, reason string, err error) {
	out, err := runGit(ctx, root, "remote")
	if err != nil {
		return "", "", fmt.Errorf("list remotes: %w", err)
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}

	switch {
	case len(names) == 0:
		return "", "this repository has no remote, so other devices have nothing to match on", nil
	case len(names) == 1:
		return names[0], "", nil
	}
	for _, n := range names {
		if n == "origin" {
			return "origin", "", nil
		}
	}
	return "", fmt.Sprintf("this repository has %d remotes and none is named origin; bind it by hand", len(names)), nil
}
