package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Binding is the user's explicit statement that a project identity belongs to
// a local directory. Its value is intentionally kept independent from config,
// so the project layer can be used without creating a configuration cycle.
type Binding struct {
	Identity  string `json:"identity"`
	LocalRoot string `json:"localRoot"`
}

// These sentinels distinguish an absent project from an unsafe ambiguity. The
// latter must never be resolved by choosing the first directory returned by a
// scan (BR-12).
var (
	ErrProjectNotHere   = errors.New("project: the project is not present on this device")
	ErrAmbiguousProject = errors.New("project: more than one local directory matches the project")
)

// Locate returns every local root that matches id. Explicit bindings are
// included alongside scanned candidates; duplicate spellings of one directory
// are collapsed by filesystem identity.
func Locate(ctx context.Context, id Identity, candidates []string, bindings []Binding) ([]string, error) {
	if !id.Stable() {
		return nil, errors.New("project: an unstable identity cannot be located")
	}

	var roots []string
	bound, err := boundRoots(id, bindings)
	if err != nil {
		return nil, err
	}
	for _, root := range bound {
		roots = appendUniqueDirectory(roots, root)
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !directoryExists(candidate) {
			continue
		}

		project, err := Identify(ctx, candidate)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// A candidate can disappear or become unreadable while a scan is
			// running. It is not evidence that the project is absent, but it
			// also cannot be returned as a match.
			continue
		}
		if !project.Identity.Stable() || project.Identity.Value != id.Value {
			continue
		}
		roots = appendUniqueDirectory(roots, project.Root)
	}

	if len(roots) == 0 {
		return nil, ErrProjectNotHere
	}
	return roots, nil
}

// Resolve returns one local root for id. A valid explicit binding wins over a
// scan, while multiple real matches remain an error for the caller to resolve
// rather than a guess for this package to make.
func Resolve(ctx context.Context, id Identity, candidates []string, bindings []Binding) (string, error) {
	if !id.Stable() {
		return "", errors.New("project: an unstable identity cannot be resolved")
	}

	bound, err := boundRoots(id, bindings)
	if err != nil {
		return "", err
	}
	if len(bound) > 1 {
		return "", ErrAmbiguousProject
	}
	if len(bound) == 1 {
		return bound[0], nil
	}

	roots, err := Locate(ctx, id, candidates, nil)
	if err != nil {
		return "", err
	}
	if len(roots) != 1 {
		return "", ErrAmbiguousProject
	}
	return roots[0], nil
}

func boundRoots(id Identity, bindings []Binding) ([]string, error) {
	var roots []string
	for _, binding := range bindings {
		if binding.Identity != id.Value {
			continue
		}
		if strings.TrimSpace(binding.LocalRoot) == "" {
			return nil, errors.New("project: a project binding has no local directory")
		}
		root, err := absoluteDirectory(binding.LocalRoot)
		if err != nil {
			return nil, fmt.Errorf("the project binding is no longer usable: %w", err)
		}
		roots = appendUniqueDirectory(roots, root)
	}
	return roots, nil
}

func directoryExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func appendUniqueDirectory(roots []string, candidate string) []string {
	for _, existing := range roots {
		if sameDirectory(existing, candidate) {
			return roots
		}
	}
	return append(roots, filepath.Clean(candidate))
}

func sameDirectory(a, b string) bool {
	left, leftErr := os.Stat(a)
	right, rightErr := os.Stat(b)
	if leftErr == nil && rightErr == nil && os.SameFile(left, right) {
		return true
	}
	if filepath.VolumeName(a) != "" || filepath.VolumeName(b) != "" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
