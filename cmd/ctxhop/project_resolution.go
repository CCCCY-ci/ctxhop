package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/project"
)

var errProjectBindingConflict = errors.New("project: current directory has conflicting project bindings")

// resolveCurrentProject applies the one project-identity rule used by all
// current-project commands. A stable Git identity wins; an explicit binding is
// consulted only when Git cannot provide one. Falling back to a machine-local
// path would make two devices write to different namespaces, so an unresolved
// directory remains unstable.
func resolveCurrentProject(ctx context.Context, c *config.Config, dir string) (project.Project, error) {
	if ctx == nil {
		return project.Project{}, errors.New("project: context is required")
	}
	if c == nil {
		return project.Project{}, errors.New("project: configuration is unavailable")
	}

	current, err := project.Identify(ctx, dir)
	if err != nil {
		return project.Project{}, err
	}
	if current.Identity.Stable() {
		return current, nil
	}

	var boundValue string
	for _, binding := range c.Projects.Bindings {
		if !projectBindingContains(binding.LocalRoot, current.Root) {
			continue
		}
		value := strings.TrimSpace(binding.Identity)
		if value == "" {
			return project.Project{}, errors.New("project: current directory has an empty project binding")
		}
		if boundValue == "" {
			boundValue = value
			continue
		}
		if boundValue != value {
			return project.Project{}, errProjectBindingConflict
		}
	}
	if boundValue == "" {
		return current, nil
	}
	if manualIdentityHasMultipleRoots(c, boundValue) {
		return project.Project{}, fmt.Errorf("project: manual identity %q is bound to multiple local roots; unbind the duplicate or use a unique --name for each project", boundValue)
	}

	identity, err := project.IdentityFromValue(boundValue)
	if err != nil {
		return project.Project{}, err
	}
	current.Identity = identity
	current.Reason = ""
	return current, nil
}

func manualIdentityHasMultipleRoots(c *config.Config, identity string) bool {
	if c == nil || !strings.HasPrefix(identity, "manual:") {
		return false
	}
	roots := make(map[string]struct{})
	for _, binding := range c.Projects.Bindings {
		if binding.Identity != identity {
			continue
		}
		root := normalizedProjectRoot(binding.LocalRoot)
		if root == "" {
			continue
		}
		if filepath.VolumeName(root) != "" {
			root = strings.ToLower(root)
		}
		roots[root] = struct{}{}
	}
	return len(roots) > 1
}

func projectBindingContains(bindingRoot, currentRoot string) bool {
	bindingRoot = normalizedProjectRoot(bindingRoot)
	currentRoot = normalizedProjectRoot(currentRoot)
	if bindingRoot == "" || currentRoot == "" {
		return false
	}
	if sameProjectRoot(bindingRoot, currentRoot) {
		return true
	}
	if filepath.VolumeName(bindingRoot) != "" || filepath.VolumeName(currentRoot) != "" {
		bindingRoot = strings.ToLower(bindingRoot)
		currentRoot = strings.ToLower(currentRoot)
	}
	relative, err := filepath.Rel(bindingRoot, currentRoot)
	if err != nil || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
