package main

import (
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/config"
)

func TestProjectPullModeHonorsExclusionBeforePushOnly(t *testing.T) {
	c := config.New()
	c.Projects.Excluded = []string{"project1"}
	c.Projects.PushOnly = []string{"project1", "project2"}

	if got := projectPullMode(c, "project1"); got != projectModeExcluded {
		t.Fatalf("excluded project mode = %q", got)
	}
	if got := projectPullMode(c, "project2"); got != projectModePushOnly {
		t.Fatalf("push-only project mode = %q", got)
	}
	if got := projectPullMode(c, "project3"); got != "" {
		t.Fatalf("ordinary project mode = %q", got)
	}
}
