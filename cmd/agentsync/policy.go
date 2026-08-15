package main

import "github.com/CCCCY-ci/agentsync/internal/config"

const (
	projectModeExcluded = "excluded"
	projectModePushOnly = "push-only"
)

// projectPullMode returns the configured boundary for remote session reads.
// An empty mode means that the project may inspect and restore remote data.
func projectPullMode(c *config.Config, identity string) string {
	if projectExcluded(c, identity) {
		return projectModeExcluded
	}
	if projectPushOnly(c, identity) {
		return projectModePushOnly
	}
	return ""
}
