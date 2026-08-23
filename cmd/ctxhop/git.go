package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/gitstate"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const gitPreviewTimeout = 2 * time.Minute

type gitOptions struct {
	action  string
	session string
	json    bool
}

type gitSource struct {
	device  syncer.MetadataRef
	updated time.Time
}

type gitEntryReport struct {
	XY           string `json:"xy"`
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
}

type gitStashReport struct {
	Ref     string `json:"ref"`
	Subject string `json:"subject,omitempty"`
}

type gitSubmoduleReport struct {
	Path        string `json:"path"`
	Recorded    string `json:"recorded,omitempty"`
	Head        string `json:"head,omitempty"`
	Initialized bool   `json:"initialized"`
	Clean       bool   `json:"clean"`
	Status      string `json:"status,omitempty"`
}

type gitTransferReport struct {
	Requested        bool   `json:"requested"`
	CommitRange      string `json:"commitRange,omitempty"`
	CommitBytes      int64  `json:"commitBytes,omitempty"`
	CommitDigest     string `json:"commitDigest,omitempty"`
	WorktreeBase     string `json:"worktreeBase,omitempty"`
	WorktreeStashRef string `json:"worktreeStashRef,omitempty"`
	WorktreeBytes    int64  `json:"worktreeBytes,omitempty"`
	WorktreeDigest   string `json:"worktreeDigest,omitempty"`
	Reason           string `json:"reason,omitempty"`
	BodyAvailable    bool   `json:"bodyAvailable"`
}

type gitPreviewReport struct {
	Scope                 string               `json:"scope"`
	Session               string               `json:"session"`
	Agent                 string               `json:"agent,omitempty"`
	NativeID              string               `json:"nativeId,omitempty"`
	SourceDevice          string               `json:"sourceDevice,omitempty"`
	Mode                  string               `json:"mode"`
	SourceHead            string               `json:"sourceHead,omitempty"`
	SourceBranch          string               `json:"sourceBranch,omitempty"`
	SourceDetached        bool                 `json:"sourceDetached"`
	SourceRebase          bool                 `json:"sourceRebase"`
	SourceRebaseKind      string               `json:"sourceRebaseKind,omitempty"`
	SourceUpstream        string               `json:"sourceUpstream,omitempty"`
	SourceUpstreamHead    string               `json:"sourceUpstreamHead,omitempty"`
	Ahead                 uint64               `json:"ahead"`
	Behind                uint64               `json:"behind"`
	SourceClean           bool                 `json:"sourceClean"`
	SensitiveOmitted      bool                 `json:"sensitiveOmitted,omitempty"`
	Dirty                 []gitEntryReport     `json:"dirty,omitempty"`
	Stashes               []gitStashReport     `json:"stashes,omitempty"`
	Submodules            []gitSubmoduleReport `json:"submodules,omitempty"`
	Transfer              gitTransferReport    `json:"transfer"`
	CurrentHead           string               `json:"currentHead,omitempty"`
	CurrentBranch         string               `json:"currentBranch,omitempty"`
	CurrentDetached       bool                 `json:"currentDetached"`
	CurrentRebase         bool                 `json:"currentRebase"`
	CurrentRebaseKind     string               `json:"currentRebaseKind,omitempty"`
	CurrentClean          bool                 `json:"currentClean"`
	CommitReady           bool                 `json:"commitReady"`
	WorktreeReady         bool                 `json:"worktreeReady"`
	CommitRef             string               `json:"commitRef,omitempty"`
	WorktreeRef           string               `json:"worktreeRef,omitempty"`
	WorktreeApplyStarted  bool                 `json:"worktreeApplyStarted"`
	WorktreeApplied       bool                 `json:"worktreeApplied"`
	ManualCleanupRequired bool                 `json:"manualCleanupRequired"`
	Conflicts             []string             `json:"conflicts,omitempty"`
	Status                string               `json:"status"`
	Notes                 []string             `json:"notes"`
}

func appendGitConflicts(conflicts []string, kinds ...string) []string {
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		found := false
		for _, existing := range conflicts {
			if existing == kind {
				found = true
				break
			}
		}
		if !found {
			conflicts = append(conflicts, kind)
		}
	}
	return conflicts
}

func runGit(args []string) error {
	return runGitWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runGitWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseGitOptions(args)
	if err != nil {
		return err
	}
	if input == nil || output == nil || prompt == nil {
		return errors.New("git: input, output and prompt are required")
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitPreviewTimeout)
	defer cancel()
	state, err := collectEnvironmentContext(ctx, c, configDir, ".", input, prompt)
	if err != nil {
		return err
	}
	defer state.Access.close()
	session := findEnvironmentSession(state.List.Sessions, options.session)
	if session == nil {
		return fmt.Errorf("git %s: session %q was not found in the current project", options.action, options.session)
	}
	return runGitWithState(ctx, state, session, options, output)
}

func runGitWithState(ctx context.Context, state environmentContext, session *listSession, options gitOptions, output io.Writer) error {
	if session == nil {
		return errors.New("git: session is required")
	}
	if output == nil {
		return errors.New("git: output is required")
	}
	source, device, err := readGitStateForSession(ctx, state, session)
	if err != nil {
		return err
	}
	report := buildGitPreviewReport(state, session, source, device)
	var transfer *gitstate.Transfer
	if source.Transfer.CommitBytes != 0 || source.Transfer.WorktreeBytes != 0 {
		layout, layoutErr := syncer.NewObjectLayout(state.ProjectID, session.RemoteID, device.DeviceID)
		if layoutErr != nil {
			return fmt.Errorf("git %s: %w", options.action, layoutErr)
		}
		loaded, readErr := syncer.ReadGitTransfer(ctx, state.Access.Store, layout, state.Access.Identities)
		if errors.Is(readErr, remote.ErrNotFound) {
			report.Status = "transfer-missing"
			report.Conflicts = appendGitConflicts(report.Conflicts, gitstate.ConflictTransferMissing)
			report.Notes = append(report.Notes, "the source recorded a Git transfer but its encrypted body is not available")
		} else if readErr != nil {
			return fmt.Errorf("git %s: read encrypted Git transfer: %w", options.action, readErr)
		} else {
			transfer = &loaded
			report.Transfer.BodyAvailable = true
		}
	}
	var priorApply *gitstate.ApplyRecord
	if options.action == "apply" && transfer != nil {
		record, found, recordErr := gitstate.FindMatchingApplyRecord(state.ConfigDir, state.ProjectID, session.RemoteID, source)
		if recordErr != nil {
			return fmt.Errorf("resume Git: read local restore record: %w", recordErr)
		}
		if found {
			priorApply = &record
		}
	}
	if source.Mode == gitstate.ModeGit && report.Status != "transfer-missing" {
		preview, previewErr := gitstate.PreviewTransfer(ctx, state.CurrentRoot, source, transfer)
		if previewErr != nil {
			return fmt.Errorf("git %s: inspect target Git state: %w", options.action, previewErr)
		}
		applyGitPreview(&report, preview)
	}
	var applyErr error
	if options.action == "apply" {
		if source.Mode != gitstate.ModeGit || transfer == nil {
			if report.Status == "transfer-missing" {
				report.Notes = append(report.Notes, "the explicit Git transfer body is missing; no local Git state changed")
				applyErr = errors.New("resume Git: encrypted Git transfer body is unavailable")
			} else if report.Status == gitstate.ApplyConflict {
				report.Notes = append(report.Notes, "Git restore was blocked by the preflight conflict; no local Git state changed")
				applyErr = errors.New("resume Git: Git transfer is not safe for the current repository state")
			} else {
				report.Status = gitstate.ApplyNoChange
				report.Notes = append(report.Notes, "no explicit Git transfer body is available; no local Git state changed")
			}
		} else if gitApplyRetryBlocked(priorApply, report.Status) {
			report.Status = gitstate.ApplyPartial
			report.CommitRef = priorApply.CommitRef
			report.WorktreeRef = priorApply.WorktreeRef
			report.WorktreeApplyStarted = priorApply.WorktreeApplyStarted
			report.WorktreeApplied = priorApply.WorktreeApplied
			report.ManualCleanupRequired = true
			report.Conflicts = appendGitConflicts(report.Conflicts, gitstate.ConflictPartialApply)
			report.Notes = append(report.Notes, "a previous application of this exact Git transfer did not complete; inspect 'git status' and clean up manually before retrying; no new Git state was changed")
			applyErr = errors.New("resume Git: previous restore requires manual cleanup")
		} else if priorApply != nil && priorApply.Status == gitstate.ApplyApplied {
			report.Status = gitstate.ApplyAlreadyApplied
			report.CommitRef = priorApply.CommitRef
			report.WorktreeRef = priorApply.WorktreeRef
			report.WorktreeApplyStarted = priorApply.WorktreeApplyStarted
			report.WorktreeApplied = priorApply.WorktreeApplied
			report.Notes = append(report.Notes, fmt.Sprintf("this exact Git transfer was already applied at %s; no local Git state was changed", priorApply.AppliedAt.UTC().Format(time.RFC3339)))
			if priorApply.CommitRef != "" {
				report.Notes = append(report.Notes, gitManualIntegrationNote(source, priorApply.CommitRef, priorApply.Branch))
			}
		} else {
			if priorApply != nil && priorApply.ManualCleanupRequired {
				report.Notes = append(report.Notes, "a previous partial restore was recorded; the target now passes Git preflight, retrying the transfer")
			}
			result, resultErr := gitstate.ApplyTransfer(ctx, state.CurrentRoot, source, *transfer)
			report.Status = result.Status
			report.CommitRef = result.CommitRef
			report.WorktreeRef = result.WorktreeRef
			report.WorktreeApplyStarted = result.WorktreeApplyStarted
			report.WorktreeApplied = result.WorktreeApplied
			report.ManualCleanupRequired = result.ManualCleanupRequired
			report.Conflicts = appendGitConflicts(report.Conflicts, result.Conflicts...)
			report.CurrentHead = result.CurrentHead
			report.CurrentBranch = result.CurrentBranch
			report.Notes = append(report.Notes, result.Notes...)
			if result.CommitRef != "" {
				report.Notes = append(report.Notes, gitManualIntegrationNote(source, result.CommitRef, result.CurrentBranch))
			}
			recordErr := gitstate.WriteApplyRecord(state.ConfigDir, gitstate.ApplyRecord{
				Version: gitstate.Version, AppliedAt: time.Now().UTC(), ProjectID: state.ProjectID,
				SessionID: session.RemoteID, ProjectIdentity: state.ProjectIdentity,
				SourceHead: source.Repository.Head, SourceBase: source.Repository.UpstreamHead,
				SourceBranch: source.Repository.Branch, CurrentHead: result.CurrentHead, Branch: result.CurrentBranch,
				CommitDigest: source.Transfer.CommitDigest, WorktreeDigest: source.Transfer.WorktreeDigest,
				WorktreeStashRef: source.Transfer.WorktreeStashRef,
				CommitRef:        result.CommitRef, WorktreeRef: result.WorktreeRef,
				WorktreeApplyStarted: result.WorktreeApplyStarted, WorktreeApplied: result.WorktreeApplied,
				ManualCleanupRequired: result.ManualCleanupRequired, Status: result.Status,
			})
			if recordErr != nil {
				applyErr = errors.Join(resultErr, fmt.Errorf("resume Git: write local recovery record: %w", recordErr))
			} else {
				applyErr = resultErr
			}
		}
	}
	if options.json {
		err = writeGitPreviewJSON(output, report)
	} else {
		err = writeGitPreviewText(output, report)
	}
	if err != nil {
		return err
	}
	return applyErr
}

func gitManualIntegrationNote(source gitstate.State, commitRef, targetBranch string) string {
	note := fmt.Sprintf("manual integration pending: inspect %s with 'git log --oneline --reverse %s'", commitRef, commitRef)
	if source.Repository.UpstreamHead != "" {
		note += fmt.Sprintf("; source base is %s", source.Repository.UpstreamHead)
	}
	if targetBranch != "" {
		note += fmt.Sprintf("; integrate on branch %s with normal Git operations", targetBranch)
	} else {
		note += "; integrate on the current branch with normal Git operations"
	}
	return note + "; CtxHop will not merge, rebase, cherry-pick or push"
}

func gitApplyRetryBlocked(priorApply *gitstate.ApplyRecord, status string) bool {
	return priorApply != nil && priorApply.ManualCleanupRequired && status == gitstate.ApplyConflict
}

func parseGitOptions(args []string) (gitOptions, error) {
	if len(args) == 0 {
		return gitOptions{}, errors.New("git: expected 'preview|apply <SESSION_ID>'")
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	if action != "preview" && action != "apply" {
		return gitOptions{}, fmt.Errorf("git: unknown action %q; expected preview or apply", action)
	}
	flags := flag.NewFlagSet("git "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return gitOptions{}, fmt.Errorf("git %s: %w", action, err)
	}
	if flags.NArg() != 1 {
		return gitOptions{}, fmt.Errorf("git %s: expected one session ID", action)
	}
	session := strings.TrimSpace(flags.Arg(0))
	if session == "" || strings.ContainsRune(session, 0) {
		return gitOptions{}, errors.New("git: session ID is invalid")
	}
	return gitOptions{action: action, session: session, json: *jsonOutput}, nil
}

func readGitStateForSession(ctx context.Context, state environmentContext, session *listSession) (gitstate.State, syncer.MetadataRef, error) {
	group, ok := findEnvironmentGroup(state.RemoteSessions, session)
	if !ok {
		return gitstate.State{}, syncer.MetadataRef{}, errors.New("git: remote session metadata was not found")
	}
	candidates := make([]gitSource, 0, len(group.Devices))
	for _, device := range group.Devices {
		updated := time.Time{}
		if summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload); err == nil {
			updated = summary.UpdatedAt
		}
		candidates = append(candidates, gitSource{device: device, updated: updated})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].updated.Equal(candidates[j].updated) {
			return candidates[i].device.DeviceID < candidates[j].device.DeviceID
		}
		return candidates[i].updated.After(candidates[j].updated)
	})
	var lastErr error
	for _, candidate := range candidates {
		layout, err := syncer.NewObjectLayout(state.ProjectID, group.SessionID, candidate.device.DeviceID)
		if err != nil {
			lastErr = err
			continue
		}
		gitState, readErr := syncer.ReadGitState(ctx, state.Access.Store, layout, state.Access.Identities)
		if errors.Is(readErr, remote.ErrNotFound) {
			continue
		}
		if readErr != nil {
			lastErr = fmt.Errorf("git: read encrypted Git state: %w", readErr)
			continue
		}
		if gitState.ProjectIdentity != "" && gitState.ProjectIdentity != state.ProjectIdentity {
			lastErr = errors.New("git: remote Git state belongs to a different project identity")
			continue
		}
		if gitState.SessionRecordCount != 0 && gitState.SessionRecordCount != candidate.device.Metadata.RecordCount {
			lastErr = errors.New("git: remote Git state is older than session metadata")
			continue
		}
		if gitState.SessionHeadDigest != "" && gitState.SessionHeadDigest != fmt.Sprintf("%x", candidate.device.Metadata.HeadDigest) {
			lastErr = errors.New("git: remote Git state does not match session metadata")
			continue
		}
		return gitState, candidate.device, nil
	}
	if lastErr != nil {
		return gitstate.State{}, syncer.MetadataRef{}, lastErr
	}
	return gitstate.State{}, syncer.MetadataRef{}, errors.New("git: no Git state is available; run 'ctxhop push' on the source device")
}

func buildGitPreviewReport(state environmentContext, session *listSession, source gitstate.State, device syncer.MetadataRef) gitPreviewReport {
	report := gitPreviewReport{
		Scope: state.List.Scope, Session: session.RemoteID, Agent: session.Agent, NativeID: session.NativeID,
		SourceDevice: device.DeviceID, Mode: string(source.Mode), SourceHead: source.Repository.Head,
		SourceBranch: source.Repository.Branch, SourceDetached: source.Repository.Detached,
		SourceRebase: source.Repository.RebaseInProgress, SourceRebaseKind: source.Repository.RebaseKind,
		SourceUpstream:     source.Repository.Upstream,
		SourceUpstreamHead: source.Repository.UpstreamHead, Ahead: source.Repository.Ahead, Behind: source.Repository.Behind,
		SourceClean: source.Worktree.Clean, SensitiveOmitted: source.Worktree.SensitiveOmitted,
		Transfer: gitTransferReport{Requested: source.Transfer.Requested, CommitRange: source.Transfer.CommitRange,
			CommitBytes: source.Transfer.CommitBytes, CommitDigest: source.Transfer.CommitDigest,
			WorktreeBase: source.Transfer.WorktreeBase, WorktreeStashRef: source.Transfer.WorktreeStashRef,
			WorktreeBytes: source.Transfer.WorktreeBytes, WorktreeDigest: source.Transfer.WorktreeDigest,
			Reason: source.Transfer.Reason},
		Status: "metadata-only",
		Notes:  []string{"Git metadata is read from the encrypted source-device object; no local Git state has changed"},
	}
	for _, entry := range source.Worktree.Entries {
		report.Dirty = append(report.Dirty, gitEntryReport{XY: entry.XY, Path: entry.Path, OriginalPath: entry.OriginalPath})
	}
	for _, stash := range source.Stashes {
		report.Stashes = append(report.Stashes, gitStashReport{Ref: stash.Ref, Subject: stash.Subject})
	}
	for _, submodule := range source.Repository.Submodules {
		report.Submodules = append(report.Submodules, gitSubmoduleReport{
			Path: submodule.Path, Recorded: submodule.Recorded, Head: submodule.Head,
			Initialized: submodule.Initialized, Clean: submodule.Clean, Status: submodule.Status,
		})
	}
	return report
}

func applyGitPreview(report *gitPreviewReport, preview gitstate.ApplyPreview) {
	report.CurrentHead = preview.CurrentHead
	report.CurrentBranch = preview.CurrentBranch
	report.CurrentDetached = preview.CurrentDetached
	report.CurrentRebase = preview.CurrentRebaseInProgress
	report.CurrentRebaseKind = preview.CurrentRebaseKind
	report.CurrentClean = preview.CurrentClean
	report.CommitReady = preview.CommitReady
	report.WorktreeReady = preview.WorktreeReady
	report.Conflicts = appendGitConflicts(report.Conflicts, preview.Conflicts...)
	report.Status = preview.Status
	report.Notes = append(report.Notes, preview.Notes...)
}

func writeGitPreviewJSON(w io.Writer, report gitPreviewReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeGitPreviewText(w io.Writer, report gitPreviewReport) error {
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
	if report.SourceDevice != "" {
		if _, err := fmt.Fprintf(w, "source device: %s\n", safeListText(report.SourceDevice)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "mode: %s\n", safeListText(report.Mode)); err != nil {
		return err
	}
	if report.SourceHead != "" {
		if _, err := fmt.Fprintf(w, "source head: %s\n", safeListText(report.SourceHead)); err != nil {
			return err
		}
	}
	if report.SourceBranch != "" {
		if _, err := fmt.Fprintf(w, "source branch: %s\n", safeListText(report.SourceBranch)); err != nil {
			return err
		}
	}
	if report.SourceDetached {
		if _, err := fmt.Fprintln(w, "source HEAD: detached"); err != nil {
			return err
		}
	}
	if report.SourceRebase {
		if _, err := fmt.Fprintf(w, "source rebase: %s\n", safeListText(report.SourceRebaseKind)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "source ahead/behind: %d/%d\n", report.Ahead, report.Behind); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "source clean: %t\n", report.SourceClean); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "dirty paths: %d\n", len(report.Dirty)); err != nil {
		return err
	}
	for _, entry := range report.Dirty {
		if _, err := fmt.Fprintf(w, "- %s %s", safeListText(entry.XY), safeListText(entry.Path)); err != nil {
			return err
		}
		if entry.OriginalPath != "" {
			if _, err := fmt.Fprintf(w, " from=%s", safeListText(entry.OriginalPath)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "stashes: %d\n", len(report.Stashes)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "submodules: %d\n", len(report.Submodules)); err != nil {
		return err
	}
	for _, submodule := range report.Submodules {
		if _, err := fmt.Fprintf(w, "- submodule %s status=%s initialized=%t clean=%t\n", safeListText(submodule.Path), safeListText(submodule.Status), submodule.Initialized, submodule.Clean); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "transfer requested: %t\n", report.Transfer.Requested); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "transfer body: %t\n", report.Transfer.BodyAvailable); err != nil {
		return err
	}
	if report.Transfer.CommitBytes != 0 || report.Transfer.WorktreeBytes != 0 {
		if _, err := fmt.Fprintf(w, "transfer bytes: commits=%d worktree=%d\n", report.Transfer.CommitBytes, report.Transfer.WorktreeBytes); err != nil {
			return err
		}
	}
	if report.Transfer.WorktreeStashRef != "" {
		if _, err := fmt.Fprintf(w, "selected stash: %s\n", safeListText(report.Transfer.WorktreeStashRef)); err != nil {
			return err
		}
	}
	if report.CurrentHead != "" {
		if _, err := fmt.Fprintf(w, "current head: %s\n", safeListText(report.CurrentHead)); err != nil {
			return err
		}
	}
	if report.CurrentBranch != "" {
		if _, err := fmt.Fprintf(w, "current branch: %s\n", safeListText(report.CurrentBranch)); err != nil {
			return err
		}
	}
	if report.CurrentDetached {
		if _, err := fmt.Fprintln(w, "current HEAD: detached"); err != nil {
			return err
		}
	}
	if report.CurrentRebase {
		if _, err := fmt.Fprintf(w, "current rebase: %s\n", safeListText(report.CurrentRebaseKind)); err != nil {
			return err
		}
	}
	if report.CommitRef != "" {
		if _, err := fmt.Fprintf(w, "commit ref: %s\n", safeListText(report.CommitRef)); err != nil {
			return err
		}
	}
	if report.WorktreeRef != "" {
		if _, err := fmt.Fprintf(w, "worktree ref: %s\n", safeListText(report.WorktreeRef)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "worktree applied: %t\n", report.WorktreeApplied); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "worktree restore started: %t\n", report.WorktreeApplyStarted); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "manual cleanup required: %t\n", report.ManualCleanupRequired); err != nil {
		return err
	}
	if len(report.Conflicts) != 0 {
		if _, err := fmt.Fprintf(w, "conflicts: %s\n", strings.Join(report.Conflicts, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "status: %s\n", safeListText(report.Status)); err != nil {
		return err
	}
	for _, note := range report.Notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", safeListText(note)); err != nil {
			return err
		}
	}
	return nil
}
