package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/environment"
)

func cloneMaterializeEnvironmentContents(contents []environment.ComponentContent) []environment.ComponentContent {
	if contents == nil {
		return nil
	}
	result := make([]environment.ComponentContent, 0, len(contents))
	for _, content := range contents {
		result = append(result, environment.ComponentContent{
			Component: content.Component,
			Content:   bytes.Clone(content.Content),
		})
	}
	return result
}

// applyMaterializeEnvironment applies only portable filtered components. It
// deliberately leaves platform-specific, review-required and unsupported
// components untouched; those need an explicit environment workflow rather
// than a cross-Agent session command silently changing local configuration.
func applyMaterializeEnvironment(execution materializeExecution) (string, error) {
	if len(execution.EnvironmentContents) == 0 {
		return "requested; no filtered component bodies available", nil
	}
	backupRoot := filepath.Join(execution.ConfigDir, "state", "v2", "hubs", execution.HubID, "projects", execution.ProjectID, "sessions", execution.SessionID, "environment-backups", execution.TransactionID)
	var applied, unchanged, skipped, manual int
	for _, content := range execution.EnvironmentContents {
		if content.Component.Portability != "portable" {
			skipped++
			continue
		}
		state, err := applyMaterializeEnvironmentComponent(content, execution, backupRoot)
		if err != nil {
			return "", fmt.Errorf("environment component %q: %w", content.Component.Name, err)
		}
		switch state.State {
		case environment.ComponentStateApplied:
			applied++
		case environment.ComponentStateUnchanged:
			unchanged++
		case environment.ComponentStateManual, environment.ComponentStateUnavailable:
			manual++
		case environment.ComponentStateMissing, environment.ComponentStateChanged:
			manual++
		default:
			return "", fmt.Errorf("environment component %q returned unsafe state %q", content.Component.Name, state.State)
		}
	}
	return fmt.Sprintf("applied=%d unchanged=%d skipped=%d manual=%d", applied, unchanged, skipped, manual), nil
}

func applyMaterializeEnvironmentComponent(content environment.ComponentContent, execution materializeExecution, backupRoot string) (environment.LocalComponentState, error) {
	switch execution.Preview.TargetAgent {
	case "codex":
		if content.Component.Kind == "mcp" || content.Component.Kind == "settings" {
			return environment.ApplyConfigComponent(content, "codex", execution.Target.Installation.DataDir, execution.ProjectRoot, backupRoot)
		}
		return environment.ApplyComponent(content, "codex", execution.Target.Installation.DataDir, execution.ProjectRoot, backupRoot)
	case "claude-code":
		return environment.ApplyClaudeComponent(content, execution.Target.Installation.DataDir, execution.ProjectRoot, backupRoot)
	default:
		return environment.LocalComponentState{State: environment.ComponentStateManual, Reason: "target Agent has no safe environment apply adapter"}, nil
	}
}

// launchMaterializedSession starts no process implicitly. --launch is an
// explicit request, but the built-in adapters intentionally do not own an
// Agent process lifecycle. Return the exact native command users can run so
// the operation remains deterministic in terminals, scripts and CI.
func launchMaterializedSession(execution materializeExecution) string {
	command := materializeLaunchCommand(execution.Preview.TargetAgent, execution.Preview.TargetNativeID)
	if command == "" {
		return "not-started; target Agent has no supported launch command"
	}
	if _, err := exec.LookPath(strings.Fields(command)[0]); err != nil {
		return "not-started; run " + command
	}
	return "manual; run " + command
}

func materializeLaunchCommand(agent, nativeID string) string {
	switch agent {
	case "codex":
		return "codex resume " + nativeID
	case "claude-code":
		return "claude --resume " + nativeID
	default:
		return ""
	}
}
