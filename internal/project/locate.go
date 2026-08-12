package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrProjectNotHere reports that nothing on this machine belongs to the
// identity being looked for.
var ErrProjectNotHere = errors.New("project: no directory on this device matches")

// ErrAmbiguousProject reports several local directories for one identity.
var ErrAmbiguousProject = errors.New("project: several directories match; bind the one you mean")

// Binding is a directory the user has declared to be a given project.
//
// This package defines the shape and the lookup; config stores it. Neither
// package then depends on the other.
type Binding struct {
	Identity  string
	LocalRoot string
}

// Locate returns every local root belonging to id.
//
// A binding wins over anything discovered by scanning: what the user stated
// beats what the tool inferred. When a binding names a directory that is no
// longer there, that is an error rather than a reason to fall back to the scan
// - quietly choosing somewhere else would restore sessions into a working tree
// the user did not mean (spec §3).
//
// Several roots for one identity is normal rather than exceptional: linked
// worktrees share their repository's remote (spec §3.1).
func Locate(ctx context.Context, id Identity, candidates []string, bindings []Binding) ([]string, error) {
	if !id.Stable() {
		return nil, fmt.Errorf("%w: this project has no cross-device identity", ErrProjectNotHere)
	}

	var roots []string
	bound := false
	for _, b := range bindings {
		if b.Identity != id.Value {
			continue
		}
		bound = true
		info, err := os.Stat(b.LocalRoot)
		if err != nil {
			return nil, fmt.Errorf("the directory bound to this project cannot be read: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: the bound path is not a directory", ErrProjectNotHere)
		}
		roots = appendUnique(roots, filepath.Clean(b.LocalRoot))
	}
	if bound {
		return roots, nil
	}

	for _, dir := range candidates {
		p, err := Identify(ctx, dir)
		if err != nil {
			// A candidate that vanished or cannot be read is not a failure of
			// the search; the list comes from scanning a disk that changes.
			continue
		}
		if p.Identity.Stable() && p.Identity.Value == id.Value {
			roots = appendUnique(roots, p.Root)
		}
	}
	if len(roots) == 0 {
		return nil, ErrProjectNotHere
	}
	return roots, nil
}

// Resolve returns the one local root for id, and refuses when there is not
// exactly one.
//
// This is the call the sync layer makes. Refusing is the point: with two
// worktrees of one repository, picking either would write a session into the
// one the user is not looking at, and there is no undoing having written it
// (BR-12).
func Resolve(ctx context.Context, id Identity, candidates []string, bindings []Binding) (string, error) {
	roots, err := Locate(ctx, id, candidates, bindings)
	if err != nil {
		return "", err
	}
	if len(roots) > 1 {
		return "", fmt.Errorf("%w (%d found)", ErrAmbiguousProject, len(roots))
	}
	return roots[0], nil
}

// appendUnique adds root unless the list already holds the same directory.
//
// Comparison is by file identity, not by string: the same directory reaches
// here spelled differently depending on whether it came from a binding the user
// typed or from git, and on Windows the two often differ in case alone.
func appendUnique(roots []string, root string) []string {
	info, err := os.Stat(root)
	if err != nil {
		return roots
	}
	for _, existing := range roots {
		other, err := os.Stat(existing)
		if err == nil && os.SameFile(info, other) {
			return roots
		}
	}
	return append(roots, root)
}
