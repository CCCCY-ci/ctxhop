package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// IdentityKind describes the source of a cross-device project identity.
type IdentityKind string

const (
	KindNone   IdentityKind = "none"
	KindRemote IdentityKind = "remote"
	KindManual IdentityKind = "manual"
)

// Identity is the stable value used to match the same project on another
// device. A local path is intentionally not an identity: it has no meaning on
// the other machine.
type Identity struct {
	Kind  IdentityKind `json:"kind"`
	Value string       `json:"value"`
}

// Stable reports whether the identity is safe to use for cross-device
// matching.
func (i Identity) Stable() bool {
	return i.Kind != KindNone && strings.TrimSpace(i.Value) != ""
}

// Project is the result of identifying one local directory.
type Project struct {
	Root     string   `json:"root"`
	Identity Identity `json:"identity"`
	Reason   string   `json:"reason"`
}

// Identify finds the repository root and, when possible, derives a stable
// identity from its Git remote. It returns an unstable Project rather than an
// error for an ordinary directory or a missing Git installation: those are
// expected environments in which cross-device matching simply cannot apply.
func Identify(ctx context.Context, dir string) (Project, error) {
	root, err := absoluteDirectory(dir)
	if err != nil {
		return Project{}, err
	}

	top, err := runGit(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return Project{}, ctx.Err()
		}
		if gitUnavailable(err) {
			return Project{
				Root:   root,
				Reason: "git is unavailable; this directory has no automatic project identity",
			}, nil
		}
		return Project{
			Root:   root,
			Reason: "this directory is not a Git repository; bind it manually if it should be synchronized",
		}, nil
	}

	repoRoot, err := absoluteDirectory(top)
	if err != nil {
		return Project{}, fmt.Errorf("read the Git project root: %w", err)
	}

	remotes, err := remoteNames(ctx, repoRoot)
	if err != nil {
		if ctx.Err() != nil {
			return Project{}, ctx.Err()
		}
		return Project{
			Root:   repoRoot,
			Reason: "Git remotes could not be read; bind the project manually",
		}, nil
	}
	if len(remotes) == 0 {
		return Project{Root: repoRoot, Reason: "the repository has no remote; bind the project manually"}, nil
	}

	name := remotes[0]
	if !containsString(remotes, "origin") && len(remotes) != 1 {
		return Project{Root: repoRoot, Reason: "the repository has multiple remotes and no unambiguous project identity"}, nil
	}
	if containsString(remotes, "origin") {
		name = "origin"
	}

	raw, err := runGit(ctx, repoRoot, "remote", "get-url", name)
	if err != nil {
		if ctx.Err() != nil {
			return Project{}, ctx.Err()
		}
		return Project{Root: repoRoot, Reason: "the selected Git remote could not be read; bind the project manually"}, nil
	}
	canonical, err := CanonicalizeRemote(raw)
	if err != nil {
		return Project{Root: repoRoot, Reason: "the selected Git remote has no cross-device identity; bind the project manually"}, nil
	}

	return Project{
		Root:     repoRoot,
		Identity: Identity{Kind: KindRemote, Value: canonical},
	}, nil
}

// ManualIdentity creates an identity for a project that has no usable Git
// remote. The prefix keeps a hand-entered value distinct from a remote
// identity even when their visible names happen to match.
func ManualIdentity(name string) (Identity, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsRune(name, 0) {
		return Identity{}, errors.New("project: manual identity name is required")
	}
	return Identity{Kind: KindManual, Value: "manual:" + name}, nil
}

func absoluteDirectory(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("locate the project directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("the project directory cannot be read: %w", pathSafe(err))
	}
	if !info.IsDir() {
		return "", errors.New("the project path is not a directory")
	}
	return filepath.Clean(absolute), nil
}

func remoteNames(ctx context.Context, root string) ([]string, error) {
	text, err := runGit(ctx, root, "remote")
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
