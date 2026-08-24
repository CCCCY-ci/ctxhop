package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/gitstate"
)

type projectStateReport struct {
	Scope     string                  `json:"scope"`
	Session   string                  `json:"session"`
	Agent     string                  `json:"agent,omitempty"`
	NativeID  string                  `json:"nativeId,omitempty"`
	Workspace *workspacePreviewReport `json:"workspace,omitempty"`
	Git       *gitPreviewReport       `json:"git,omitempty"`
	Conflicts []string                `json:"conflicts,omitempty"`
	Status    string                  `json:"status"`
	Notes     []string                `json:"notes"`
}
type projectStateInspection struct {
	Report       projectStateReport
	WorkspaceErr error
	GitErr       error
}

func inspectProjectState(ctx context.Context, state environmentContext, session *listSession) (projectStateInspection, error) {
	if session == nil {
		return projectStateInspection{}, errors.New("workspace: session is required")
	}
	workspace, workspaceErr := loadProjectWorkspaceReport(ctx, state, session)
	git, gitErr := loadProjectGitReport(ctx, state, session)
	report := newProjectStateReport(state, session, workspace, git)
	if workspaceErr != nil && !projectStateExpectedMissingWorkspace(git, workspaceErr) {
		report.Notes = append(report.Notes, "workspace state is unavailable; no workspace files were changed")
	}
	if gitErr != nil {
		report.Notes = append(report.Notes, "Git state is unavailable; no Git refs or files were changed")
	}
	if workspace == nil && git == nil {
		report.Status = "unavailable"
	}
	if workspaceErr == nil && gitErr == nil {
		report.Notes = append(report.Notes, "workspace files and Git state are reviewed as one workspace operation")
	}
	if projectStateHasPathOverlap(workspace, git) {
		report.Notes = append(report.Notes, "paths present in both reports are handled Git-first; workspace restore then verifies or fills the same source state")
	}
	report.Conflicts = projectStateConflicts(workspace, git)
	if len(report.Conflicts) != 0 {
		report.Status = "attention"
	}
	return projectStateInspection{Report: report, WorkspaceErr: workspaceErr, GitErr: gitErr}, nil
}

func applyProjectState(ctx context.Context, state environmentContext, session *listSession, inspection *projectStateInspection) error {
	if inspection == nil {
		return errors.New("workspace restore: inspection is required")
	}
	report := &inspection.Report
	if inspection.WorkspaceErr != nil && inspection.GitErr != nil {
		return errors.Join(inspection.WorkspaceErr, inspection.GitErr)
	}
	if len(report.Conflicts) != 0 {
		report.Status = "blocked"
		report.Notes = append(report.Notes, "workspace restore stopped during preflight; no workspace files or Git state were changed")
		return errors.New("workspace restore: preflight conflict; no local project state was changed")
	}

	// Git preflight requires a clean target. Apply it before workspace file
	// bodies, then let the workspace layer report any remaining file-level
	// conflict. Both layers retain their own recovery logic.
	if inspection.GitErr == nil {
		appliedGit, err := applyProjectGitState(ctx, state, session)
		if appliedGit != nil {
			report.Git = appliedGit
		}
		if err != nil {
			report.Status = "partial"
			return err
		}
	}
	if inspection.WorkspaceErr == nil {
		appliedWorkspace, err := applyProjectWorkspaceState(ctx, state, session)
		if appliedWorkspace != nil {
			report.Workspace = appliedWorkspace
		}
		if err != nil {
			report.Status = "partial"
			return err
		}
	}
	report.Status = projectStateAppliedStatus(*report)
	report.Notes = append(report.Notes, "workspace restore completed; existing files were backed up by the applicable layer")
	return nil
}

func newProjectStateReport(state environmentContext, session *listSession, workspace *workspacePreviewReport, git *gitPreviewReport) projectStateReport {
	report := projectStateReport{
		Scope:     state.List.Scope,
		Session:   session.RemoteID,
		Agent:     session.Agent,
		NativeID:  session.NativeID,
		Workspace: workspace,
		Git:       git,
		Status:    "observed-only",
		Notes: []string{
			"workspace preview is read-only; no local workspace files, Git refs, branches, commits or stashes have been changed",
		},
	}
	return report
}

func loadProjectWorkspaceReport(ctx context.Context, state environmentContext, session *listSession) (*workspacePreviewReport, error) {
	var output bytes.Buffer
	err := runWorkspaceWithState(ctx, state, session, workspaceOptions{action: "preview", session: session.RemoteID, json: true}, &output)
	if err != nil {
		return nil, err
	}
	var report workspacePreviewReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		return nil, fmt.Errorf("workspace: decode workspace preview: %w", err)
	}
	return &report, nil
}

func loadProjectGitReport(ctx context.Context, state environmentContext, session *listSession) (*gitPreviewReport, error) {
	var output bytes.Buffer
	err := runGitWithState(ctx, state, session, gitOptions{action: "preview", session: session.RemoteID, json: true}, &output)
	if err != nil {
		return nil, err
	}
	var report gitPreviewReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		return nil, fmt.Errorf("workspace: decode Git preview: %w", err)
	}
	return &report, nil
}

func applyProjectWorkspaceState(ctx context.Context, state environmentContext, session *listSession) (*workspacePreviewReport, error) {
	var output bytes.Buffer
	err := runWorkspaceWithState(ctx, state, session, workspaceOptions{action: "apply", session: session.RemoteID, json: true}, &output)
	var report workspacePreviewReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		if err != nil {
			return nil, errors.Join(err, fmt.Errorf("workspace: decode workspace restore: %w", decodeErr))
		}
		return nil, fmt.Errorf("workspace: decode workspace restore: %w", decodeErr)
	}
	return &report, err
}

func applyProjectGitState(ctx context.Context, state environmentContext, session *listSession) (*gitPreviewReport, error) {
	var output bytes.Buffer
	err := runGitWithState(ctx, state, session, gitOptions{action: "apply", session: session.RemoteID, json: true}, &output)
	var report gitPreviewReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		if err != nil {
			return nil, errors.Join(err, fmt.Errorf("workspace: decode Git restore: %w", decodeErr))
		}
		return nil, fmt.Errorf("workspace: decode Git restore: %w", decodeErr)
	}
	return &report, err
}

func projectStateExpectedMissingWorkspace(git *gitPreviewReport, err error) bool {
	if git == nil || err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no explicit workspace snapshot is available") && git.Mode == string(gitstate.ModeGit)
}

func projectStateConflicts(workspace *workspacePreviewReport, git *gitPreviewReport) []string {
	var conflicts []string
	if workspace != nil {
		for _, conflict := range workspace.Conflicts {
			conflicts = appendProjectStateConflict(conflicts, "workspace:"+conflict)
		}
	}
	if git != nil {
		for _, conflict := range git.Conflicts {
			conflicts = appendProjectStateConflict(conflicts, "git:"+conflict)
		}
	}
	return conflicts
}

func appendProjectStateConflict(conflicts []string, value string) []string {
	if value == "" {
		return conflicts
	}
	for _, existing := range conflicts {
		if existing == value {
			return conflicts
		}
	}
	return append(conflicts, value)
}

func projectStateHasPathOverlap(workspace *workspacePreviewReport, git *gitPreviewReport) bool {
	if workspace == nil || git == nil {
		return false
	}
	paths := make(map[string]struct{}, len(git.Dirty)*2)
	for _, entry := range git.Dirty {
		if path := projectStateComparablePath(entry.Path); path != "" {
			paths[path] = struct{}{}
		}
		if path := projectStateComparablePath(entry.OriginalPath); path != "" {
			paths[path] = struct{}{}
		}
	}
	for _, change := range workspace.Changes {
		if change.State == "local-only" || change.State == "unchanged" {
			continue
		}
		for _, candidate := range []string{change.Path, change.File.Path} {
			if path := projectStateComparablePath(candidate); path != "" {
				if _, found := paths[path]; found {
					return true
				}
			}
		}
	}
	return false
}

func projectStateComparablePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "<absent>" {
		return ""
	}
	path = filepath.ToSlash(filepath.Clean(path))
	return strings.ToLower(path)
}

func projectStateAppliedStatus(report projectStateReport) string {
	workspaceStatus := ""
	gitStatus := ""
	if report.Workspace != nil {
		workspaceStatus = report.Workspace.Status
	}
	if report.Git != nil {
		gitStatus = report.Git.Status
	}
	if workspaceStatus == "partial" || gitStatus == "partial" {
		return "partial"
	}
	if workspaceStatus == "applied" || gitStatus == gitstate.ApplyApplied || gitStatus == gitstate.ApplyAlreadyApplied {
		return "applied"
	}
	if workspaceStatus == "attention" || gitStatus == gitstate.ApplyConflict {
		return "attention"
	}
	return "no-changes"
}

func writeProjectStateJSON(w io.Writer, report projectStateReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeProjectStateText(w io.Writer, report projectStateReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", safeListText(report.Scope)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if report.Agent != "" {
		if _, err := fmt.Fprintf(w, "agent: %s\n", safeListText(report.Agent)); err != nil {
			return err
		}
	}
	if report.NativeID != "" {
		if _, err := fmt.Fprintf(w, "native session: %s\n", safeListText(report.NativeID)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "status: %s\n", safeListText(report.Status)); err != nil {
		return err
	}
	if report.Workspace != nil {
		if _, err := fmt.Fprintln(w, "workspace:"); err != nil {
			return err
		}
		if err := writeWorkspacePreviewText(w, *report.Workspace); err != nil {
			return err
		}
	}
	if report.Git != nil {
		if _, err := fmt.Fprintln(w, "git:"); err != nil {
			return err
		}
		if err := writeGitPreviewText(w, *report.Git); err != nil {
			return err
		}
	}
	if len(report.Conflicts) != 0 {
		if _, err := fmt.Fprintf(w, "project conflicts: %s\n", strings.Join(report.Conflicts, ", ")); err != nil {
			return err
		}
	}
	for _, note := range report.Notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", safeListText(note)); err != nil {
			return err
		}
	}
	return nil
}
