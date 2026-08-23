package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
)

func TestParseProjectOptions(t *testing.T) {
	bind, err := parseProjectOptions([]string{"bind", "--name", "local-app", "--path", "."})
	if err != nil {
		t.Fatal(err)
	}
	if bind.action != projectActionBind || bind.name != "local-app" || bind.path != "." {
		t.Fatalf("bind options = %+v", bind)
	}

	mode, err := parseProjectOptions([]string{"mode", "push-only", "--identity", "github.com/example/app"})
	if err != nil {
		t.Fatal(err)
	}
	if mode.action != projectActionMode || mode.mode != projectModePushOnly || mode.identity != "github.com/example/app" {
		t.Fatalf("mode options = %+v", mode)
	}

	list, err := parseProjectOptions([]string{"list", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if list.action != projectActionList || !list.json {
		t.Fatalf("list options = %+v", list)
	}

	if _, err := parseProjectOptions([]string{"bind", "--name", "app", "--identity", "manual:app"}); err == nil {
		t.Fatal("expected conflicting bind target error")
	}
}

func TestProjectBindingAndModes(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	c := config.New()

	changed, err := bindProject(c, "github.com/example/app", root)
	if err != nil || !changed {
		t.Fatalf("first bind = %v, %v", changed, err)
	}
	changed, err = bindProject(c, "github.com/example/app", root)
	if err != nil || changed {
		t.Fatalf("idempotent bind = %v, %v", changed, err)
	}
	if _, err := bindProject(c, "github.com/other/app", root); err == nil {
		t.Fatal("expected root conflict")
	}
	changed, err = bindProject(c, "github.com/example/app", other)
	if err != nil || !changed {
		t.Fatalf("second root bind = %v, %v", changed, err)
	}

	changed, err = setProjectMode(c, "github.com/example/app", projectModePushOnly)
	if err != nil || !changed {
		t.Fatalf("push-only mode = %v, %v", changed, err)
	}
	if len(c.Projects.PushOnly) != 1 || len(c.Projects.Excluded) != 0 {
		t.Fatalf("push-only policy = %+v", c.Projects)
	}
	changed, err = setProjectMode(c, "github.com/example/app", projectModeExcluded)
	if err != nil || !changed {
		t.Fatalf("excluded mode = %v, %v", changed, err)
	}
	if len(c.Projects.PushOnly) != 0 || len(c.Projects.Excluded) != 1 {
		t.Fatalf("excluded policy = %+v", c.Projects)
	}

	removed, err := unbindProject(c, "", root)
	if err != nil || removed != 1 {
		t.Fatalf("unbind root = %d, %v", removed, err)
	}
	removed, err = unbindProject(c, "github.com/example/app", "")
	if err != nil || removed != 1 {
		t.Fatalf("unbind identity = %d, %v", removed, err)
	}
}

func TestProjectListOutputIsDeterministic(t *testing.T) {
	root := filepath.Clean(filepath.Join(t.TempDir(), "repo"))
	c := config.New()
	c.Projects.Bindings = []config.Binding{{Identity: "z-project", LocalRoot: root}}
	c.Projects.Excluded = []string{"a-project"}

	report := collectProjectList(c)
	if len(report.Projects) != 2 || report.Projects[0].Identity != "a-project" || report.Projects[1].Identity != "z-project" {
		t.Fatalf("report = %+v", report)
	}

	var output bytes.Buffer
	if err := writeProjectListJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	var decoded projectListReport
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope != "global" || len(decoded.Projects) != 2 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if !strings.Contains(output.String(), `"mode": "excluded"`) {
		t.Fatalf("JSON = %s", output.String())
	}
}
