package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/project"
)

func TestResolveCurrentProjectUsesManualBinding(t *testing.T) {
	root := t.TempDir()
	c := config.New()
	c.Projects.Bindings = []config.Binding{{
		Identity:  "manual:client-project",
		LocalRoot: filepath.Join(root, "."),
	}}

	current, err := resolveCurrentProject(context.Background(), c, root)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Identity.Stable() || current.Identity.Kind != project.KindManual {
		t.Fatalf("identity = %+v, reason = %q", current.Identity, current.Reason)
	}
	if current.Identity.Value != "manual:client-project" {
		t.Fatalf("identity = %q", current.Identity.Value)
	}
	if current.Reason != "" {
		t.Fatalf("manual binding retained unstable reason %q", current.Reason)
	}
	if !sameProjectRoot(current.Root, root) {
		t.Fatalf("root = %q, want %q", current.Root, root)
	}

	identity, err := resolveProjectPolicyTarget(context.Background(), c, projectOptions{path: root})
	if err != nil {
		t.Fatal(err)
	}
	if identity != "manual:client-project" {
		t.Fatalf("policy identity = %q", identity)
	}
}

func TestResolveCurrentProjectWithoutBindingRemainsUnstable(t *testing.T) {
	current, err := resolveCurrentProject(context.Background(), config.New(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if current.Identity.Stable() {
		t.Fatalf("unexpected identity = %+v", current.Identity)
	}
	if current.Reason == "" {
		t.Fatal("unbound directory has no explanation")
	}
}

func TestResolveCurrentProjectRejectsConflictingBindings(t *testing.T) {
	root := t.TempDir()
	c := config.New()
	c.Projects.Bindings = []config.Binding{
		{Identity: "manual:first", LocalRoot: root},
		{Identity: "manual:second", LocalRoot: filepath.Join(root, ".")},
	}

	_, err := resolveCurrentProject(context.Background(), c, root)
	if !errors.Is(err, errProjectBindingConflict) {
		t.Fatalf("error = %v, want conflicting binding error", err)
	}
}

func TestResolveCurrentProjectRejectsEmptyBinding(t *testing.T) {
	root := t.TempDir()
	c := config.New()
	c.Projects.Bindings = []config.Binding{{LocalRoot: root}}

	_, err := resolveCurrentProject(context.Background(), c, root)
	if err == nil {
		t.Fatal("empty binding was accepted")
	}
}

func TestResolveCurrentProjectUsesBindingForSubdirectory(t *testing.T) {
	root := t.TempDir()
	subdirectory := filepath.Join(root, "src", "client")
	if err := os.MkdirAll(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	c := config.New()
	c.Projects.Bindings = []config.Binding{{
		Identity:  "manual:client-project",
		LocalRoot: root,
	}}

	current, err := resolveCurrentProject(context.Background(), c, subdirectory)
	if err != nil {
		t.Fatal(err)
	}
	if current.Identity.Value != "manual:client-project" || current.Identity.Kind != project.KindManual {
		t.Fatalf("identity = %+v", current.Identity)
	}
}
