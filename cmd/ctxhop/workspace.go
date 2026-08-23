package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
	workspacepkg "github.com/CCCCY-ci/ctxhop/internal/workspace"
)

const workspacePreviewTimeout = 30 * time.Second

type workspaceOptions struct {
	action  string
	session string
	json    bool
}

type workspaceFileDescriptor struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type workspaceChange struct {
	File   workspaceFileDescriptor `json:"file"`
	Path   string                  `json:"path,omitempty"`
	State  string                  `json:"state"`
	Backup string                  `json:"backup,omitempty"`
	Reason string                  `json:"reason,omitempty"`
}

type workspacePreviewReport struct {
	Scope       string            `json:"scope"`
	Session     string            `json:"session"`
	Agent       string            `json:"agent,omitempty"`
	NativeID    string            `json:"nativeId,omitempty"`
	Mode        string            `json:"mode"`
	Coverage    string            `json:"coverage,omitempty"`
	Head        string            `json:"head,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	RecordCount uint64            `json:"recordCount"`
	HeadDigest  string            `json:"headDigest,omitempty"`
	Dirty       []string          `json:"dirty,omitempty"`
	Complete    bool              `json:"complete"`
	Warnings    []string          `json:"warnings,omitempty"`
	Conflicts   []string          `json:"conflicts,omitempty"`
	Changes     []workspaceChange `json:"changes,omitempty"`
	Status      string            `json:"status"`
	Notes       []string          `json:"notes"`
}

func appendWorkspaceConflict(conflicts []string, kind string) []string {
	if kind == "" {
		return conflicts
	}
	for _, existing := range conflicts {
		if existing == kind {
			return conflicts
		}
	}
	return append(conflicts, kind)
}

func workspaceConflictKind(state string) string {
	switch state {
	case workspacepkg.StateConflict:
		return "target-conflict"
	case workspacepkg.StateManual:
		return "manual-item"
	case workspacepkg.StateUnavailable:
		return "target-unavailable"
	case workspacepkg.StateFailed:
		return "unsafe-path"
	default:
		return ""
	}
}

type workspaceSource struct {
	device  syncer.MetadataRef
	updated time.Time
}

func runWorkspace(args []string) error {
	return runWorkspaceWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runWorkspaceWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseWorkspaceOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("workspace: input is required")
	}
	if output == nil {
		return errors.New("workspace: output is required")
	}
	if prompt == nil {
		return errors.New("workspace: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspacePreviewTimeout)
	defer cancel()

	state, err := collectEnvironmentContext(ctx, c, configDir, ".", input, prompt)
	if err != nil {
		return err
	}
	defer state.Access.close()

	session := findEnvironmentSession(state.List.Sessions, options.session)
	if session == nil {
		return fmt.Errorf("workspace %s: session %q was not found in the current project", options.action, options.session)
	}
	return runWorkspaceWithState(ctx, state, session, options, output)
}

func runWorkspaceWithState(ctx context.Context, state environmentContext, session *listSession, options workspaceOptions, output io.Writer) error {
	if session == nil {
		return errors.New("workspace: session is required")
	}
	if output == nil {
		return errors.New("workspace: output is required")
	}
	snapshot, err := readWorkspaceSnapshotForSession(ctx, state, session)
	if err != nil {
		return err
	}
	report := buildWorkspacePreviewReport(ctx, state, session, snapshot)
	var applyErr error
	if options.action == "apply" {
		applyErr = applyWorkspaceSnapshot(state, snapshot, &report)
	}

	if options.json {
		err = writeWorkspacePreviewJSON(output, report)
	} else {
		err = writeWorkspacePreviewText(output, report)
	}
	if err != nil {
		return err
	}
	return applyErr
}

func parseWorkspaceOptions(args []string) (workspaceOptions, error) {
	if len(args) == 0 {
		return workspaceOptions{}, errors.New("workspace: expected 'preview|apply <SESSION_ID>'")
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	if action != "preview" && action != "apply" {
		return workspaceOptions{}, fmt.Errorf("workspace: unknown action %q; expected preview or apply", action)
	}
	flags := flag.NewFlagSet("workspace "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return workspaceOptions{}, fmt.Errorf("workspace %s: %w", action, err)
	}
	if flags.NArg() != 1 {
		return workspaceOptions{}, fmt.Errorf("workspace %s: expected one session ID", action)
	}
	session := strings.TrimSpace(flags.Arg(0))
	if session == "" || strings.ContainsRune(session, 0) {
		return workspaceOptions{}, errors.New("workspace: session ID is invalid")
	}
	return workspaceOptions{action: action, session: session, json: *jsonOutput}, nil
}

func readWorkspaceSnapshotForSession(ctx context.Context, state environmentContext, session *listSession) (workspacepkg.Snapshot, error) {
	group, ok := findEnvironmentGroup(state.RemoteSessions, session)
	if !ok {
		return workspacepkg.Snapshot{}, errors.New("workspace: remote session metadata was not found")
	}
	candidates := make([]workspaceSource, 0, len(group.Devices))
	for _, device := range group.Devices {
		updated := time.Time{}
		if summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload); err == nil {
			updated = summary.UpdatedAt
		}
		candidates = append(candidates, workspaceSource{device: device, updated: updated})
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
		snapshot, readErr := syncer.ReadWorkspaceSnapshot(ctx, state.Access.Store, layout, state.Access.Identities)
		if errors.Is(readErr, remote.ErrNotFound) {
			continue
		}
		if readErr != nil {
			lastErr = fmt.Errorf("workspace: read encrypted snapshot: %w", readErr)
			continue
		}
		if !workspaceSnapshotMatchesMetadata(snapshot, candidate.device.Metadata) {
			lastErr = errors.New("workspace: remote snapshot is older than its session metadata")
			continue
		}
		return snapshot, nil
	}
	if lastErr != nil {
		return workspacepkg.Snapshot{}, lastErr
	}
	return workspacepkg.Snapshot{}, errors.New("workspace: no explicit workspace snapshot is available; run 'ctxhop push --workspace' on the source device")
}

func workspaceSnapshotMatchesMetadata(snapshot workspacepkg.Snapshot, metadata syncer.Metadata) bool {
	if snapshot.RecordCount != metadata.RecordCount {
		return false
	}
	if metadata.RecordCount == 0 {
		return snapshot.HeadDigest == ""
	}
	return strings.EqualFold(snapshot.HeadDigest, fmt.Sprintf("%x", metadata.HeadDigest))
}

func buildWorkspacePreviewReport(ctx context.Context, state environmentContext, session *listSession, snapshot workspacepkg.Snapshot) workspacePreviewReport {
	coverage := snapshot.Coverage
	if coverage == "" {
		coverage = workspacepkg.CoverageFingerprint
	}
	report := workspacePreviewReport{
		Scope:       state.List.Scope,
		Session:     session.RemoteID,
		Agent:       session.Agent,
		NativeID:    session.NativeID,
		Mode:        snapshot.Mode,
		Coverage:    coverage,
		Head:        snapshot.Head,
		Branch:      snapshot.Branch,
		RecordCount: snapshot.RecordCount,
		HeadDigest:  snapshot.HeadDigest,
		Dirty:       append([]string(nil), snapshot.Dirty...),
		Complete:    snapshot.Complete,
		Warnings:    append([]string(nil), snapshot.Warnings...),
		Status:      "observed-only",
		Notes: []string{
			"only files selected by the session fingerprint are included; file bodies are never shown in this report",
			"no local files or Git state were changed",
		},
	}
	if coverage == workspacepkg.CoverageDirectory {
		report.Notes[0] = "the source is a filtered directory snapshot; file bodies are never shown in this report"
	}
	for _, source := range snapshot.Files {
		local := workspacepkg.InspectFile(source, state.CurrentRoot)
		report.Changes = append(report.Changes, workspaceChange{
			File: workspaceFileDescriptor{
				Path:      source.Path,
				Digest:    source.Digest,
				Kind:      source.Kind,
				Available: source.Available,
				Reason:    source.Reason,
			},
			Path:   local.Path,
			State:  local.State,
			Backup: local.Backup,
			Reason: local.Reason,
		})
	}
	if coverage == workspacepkg.CoverageDirectory {
		scanCtx, cancel := context.WithTimeout(ctx, workspacePreviewTimeout)
		scan, scanErr := workspacepkg.ScanDirectory(scanCtx, state.CurrentRoot)
		cancel()
		if scanErr != nil {
			report.Complete = false
			report.Warnings = append(report.Warnings, "the target directory could not be fully inspected")
		} else {
			if !scan.Complete {
				report.Complete = false
			}
			report.Warnings = append(report.Warnings, scan.Warnings...)
			known := make(map[string]bool, len(snapshot.Files)+len(snapshot.Omitted))
			for _, source := range snapshot.Files {
				known[source.Path] = true
			}
			for _, path := range snapshot.Omitted {
				known[path] = true
			}
			for _, path := range scan.Paths {
				if known[path] {
					continue
				}
				source := workspacepkg.File{Path: path, Digest: "<absent>", Kind: workspacepkg.KindAbsent}
				local := workspacepkg.InspectFile(source, state.CurrentRoot)
				report.Changes = append(report.Changes, workspaceChange{
					File: workspaceFileDescriptor{Path: path, Digest: "<absent>", Kind: workspacepkg.KindAbsent},
					Path: local.Path, State: local.State, Reason: local.Reason,
				})
			}
			if len(report.Changes) != len(snapshot.Files) {
				report.Notes = append(report.Notes, "local-only files are deletion candidates; restore will not delete them automatically")
			}
		}
	}
	for _, change := range report.Changes {
		report.Conflicts = appendWorkspaceConflict(report.Conflicts, workspaceConflictKind(change.State))
	}
	if !snapshot.Complete {
		report.Notes = append(report.Notes, "some file bodies were omitted by the safety or size limits and require manual handling")
	}
	if len(report.Changes) == 0 {
		if coverage == workspacepkg.CoverageDirectory {
			report.Notes = append(report.Notes, "the directory snapshot contains no eligible files")
		} else {
			report.Notes = append(report.Notes, "the snapshot contains no fingerprinted workspace entries")
		}
	}
	return report
}

func applyWorkspaceSnapshot(state environmentContext, snapshot workspacepkg.Snapshot, report *workspacePreviewReport) error {
	if report == nil {
		return errors.New("workspace restore: report is required")
	}
	backupRoot := filepath.Join(
		state.ConfigDir,
		"state",
		"workspace-backups",
		state.ProjectID,
		report.Session,
		time.Now().UTC().Format("20060102T150405.000000000Z"),
	)
	var applyErrors []error
	applied := 0
	for index := range snapshot.Files {
		if index >= len(report.Changes) {
			break
		}
		change := &report.Changes[index]
		local, err := workspacepkg.ApplyFile(snapshot.Files[index], state.CurrentRoot, backupRoot)
		change.Path = local.Path
		change.State = local.State
		change.Backup = local.Backup
		change.Reason = local.Reason
		if err != nil {
			report.Conflicts = appendWorkspaceConflict(report.Conflicts, "apply-failed")
			applyErrors = append(applyErrors, fmt.Errorf("%s: %w", safeListText(snapshot.Files[index].Path), err))
			continue
		}
		report.Conflicts = appendWorkspaceConflict(report.Conflicts, workspaceConflictKind(local.State))
		if local.State == workspacepkg.StateApplied {
			applied++
		}
	}
	switch {
	case len(applyErrors) != 0 && applied != 0:
		report.Status = "partial"
	case len(applyErrors) != 0:
		report.Status = "failed"
	case applied != 0 && len(report.Conflicts) != 0:
		report.Status = "partial"
	case applied != 0:
		report.Status = "applied"
	case len(report.Conflicts) != 0:
		report.Status = "attention"
	default:
		report.Status = "no-changes"
	}
	if applied != 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("applied %d workspace item(s); existing files were backed up before replacement", applied))
	}
	report.Notes = append(report.Notes, "absent source files are not deleted locally; no branch switch, commit, stash, or Git command was performed")
	if len(applyErrors) != 0 {
		return errors.Join(applyErrors...)
	}
	return nil
}

func writeWorkspacePreviewJSON(w io.Writer, report workspacePreviewReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeWorkspacePreviewText(w io.Writer, report workspacePreviewReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if report.NativeID != "" {
		if _, err := fmt.Fprintf(w, "native session: %s\n", safeListText(report.NativeID)); err != nil {
			return err
		}
	}
	if report.Agent != "" {
		if _, err := fmt.Fprintf(w, "agent: %s\n", safeListText(report.Agent)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "mode: %s\n", safeListText(report.Mode)); err != nil {
		return err
	}
	if report.Coverage != "" {
		if _, err := fmt.Fprintf(w, "coverage: %s\n", safeListText(report.Coverage)); err != nil {
			return err
		}
	}
	if report.Head != "" {
		if _, err := fmt.Fprintf(w, "source head: %s\n", safeListText(report.Head)); err != nil {
			return err
		}
	}
	if report.Branch != "" {
		if _, err := fmt.Fprintf(w, "source branch: %s\n", safeListText(report.Branch)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "record count: %d\n", report.RecordCount); err != nil {
		return err
	}
	if report.HeadDigest != "" {
		if _, err := fmt.Fprintf(w, "head digest: %s\n", safeListText(report.HeadDigest)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "complete: %t\n", report.Complete); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "dirty paths: %d\n", len(report.Dirty)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "changes: %d\n", len(report.Changes)); err != nil {
		return err
	}
	for _, change := range report.Changes {
		line := fmt.Sprintf("- change file=%s kind=%s state=%s available=%t",
			safeListText(change.File.Path),
			safeListText(change.File.Kind),
			safeListText(change.State),
			change.File.Available,
		)
		if change.Path != "" {
			line += " path=" + safeListText(change.Path)
		}
		if change.Backup != "" {
			line += " backup=" + safeListText(change.Backup)
		}
		if change.Reason != "" {
			line += " reason=" + safeListText(change.Reason)
		} else if change.File.Reason != "" {
			line += " reason=" + safeListText(change.File.Reason)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	if len(report.Warnings) != 0 {
		for _, warning := range report.Warnings {
			if _, err := fmt.Fprintf(w, "warning: %s\n", safeListText(warning)); err != nil {
				return err
			}
		}
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
