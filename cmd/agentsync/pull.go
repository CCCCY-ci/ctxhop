package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

const pullCheckTimeout = 15 * time.Second

const (
	pullCheckModeMetadataOnly = "metadata-only"
	pullCheckResultUpToDate   = "up-to-date"
	pullCheckResultUpdates    = "updates-available"
	pullCheckResultAttention  = "attention-required"
)

type pullOptions struct {
	check bool
	json  bool
}

type pullCheckReport struct {
	Scope    string                 `json:"scope"`
	Mode     string                 `json:"mode"`
	Result   string                 `json:"result"`
	Sessions pullCheckSessionCounts `json:"sessions"`
}

type pullCheckSessionCounts struct {
	Checked         int `json:"checked"`
	ForeignUpdates  int `json:"foreignUpdates"`
	ForeignBranches int `json:"foreignBranches"`
	Unchanged       int `json:"unchanged"`
	Attention       int `json:"attention"`
}

func init() {
	for i := range commands {
		if commands[i].name == "pull" {
			commands[i].run = runPull
		}
	}
}

func runPull(args []string) error {
	return runPullWithIO(args, os.Stdin, os.Stdout, os.Stderr)
}

func runPullWithIO(args []string, input io.Reader, output io.Writer, prompt io.Writer) error {
	options, err := parsePullOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("pull: input is required")
	}
	if output == nil {
		return errors.New("pull: output is required")
	}
	if prompt == nil {
		return errors.New("pull: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pullCheckTimeout)
	defer cancel()
	report, err := collectPullCheck(ctx, c, configDir, ".", input, prompt)
	if err != nil {
		return err
	}
	if options.json {
		return writePullCheckJSON(output, report)
	}
	return writePullCheckText(output, report)
}

func parsePullOptions(args []string) (pullOptions, error) {
	flags := flag.NewFlagSet("pull", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	check := flags.Bool("check", false, "check encrypted remote metadata without restoring sessions")
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return pullOptions{}, fmt.Errorf("pull: %w", err)
	}
	if flags.NArg() != 0 {
		return pullOptions{}, fmt.Errorf("pull: unexpected argument %q", flags.Arg(0))
	}
	if !*check {
		return pullOptions{}, errors.New("pull: --check is required")
	}
	return pullOptions{check: *check, json: *jsonOutput}, nil
}

// collectPullCheck performs a project-level metadata-only check. It never
// reads encrypted shard bodies, writes Agent data, or persists observed tips.
func collectPullCheck(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, prompt io.Writer) (pullCheckReport, error) {
	if ctx == nil {
		return pullCheckReport{}, errors.New("pull: context is required")
	}
	if c == nil {
		return pullCheckReport{}, errors.New("pull: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: %w", err)
	}
	if err := devicePullError("pull", c); err != nil {
		return pullCheckReport{}, err
	}
	if input == nil {
		return pullCheckReport{}, errors.New("pull: input is required")
	}
	if prompt == nil {
		return pullCheckReport{}, errors.New("pull: prompt output is required")
	}

	current, err := project.Identify(ctx, projectDir)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return pullCheckReport{}, fmt.Errorf("pull: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return pullCheckReport{}, errors.New("pull: project is excluded from synchronization")
	case projectModePushOnly:
		return pullCheckReport{}, errors.New("pull: project is configured as push-only; remote sessions are unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return pullCheckReport{}, errors.New("pull: encryption identity is not configured; run 'agentsync init'")
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: derive project identity: %w", err)
	}
	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: configure backend: %s", safeBackendSetupError(err))
	}
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: read remote keyfile: %w", err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: validate remote identity: %w", err)
	}
	if !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return pullCheckReport{}, errors.New("pull: remote encryption identity does not match this configuration")
	}

	passphrase, err := readCommandPassphrase(input, prompt, "pull")
	if err != nil {
		return pullCheckReport{}, err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: open remote identity: %w", err)
	}

	remoteSessions, err := syncer.FetchProjectMetadata(ctx, store, projectID, identity)
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return newPullCheckReport(pullCheckSessionCounts{}), nil
	}
	if err != nil {
		return pullCheckReport{}, fmt.Errorf("pull: read encrypted session metadata: %w", err)
	}

	counts, err := inspectPullCheckSessions(ctx, configDir, projectID, c.Device.ID, remoteSessions)
	if err != nil {
		return pullCheckReport{}, err
	}
	return newPullCheckReport(counts), nil
}

func inspectPullCheckSessions(ctx context.Context, stateRoot, projectID, localDeviceID string, sessions []syncer.ProjectMetadataRef) (pullCheckSessionCounts, error) {
	var counts pullCheckSessionCounts
	for _, session := range sessions {
		counts.Checked++
		plan, err := loadPullCheckPlan(ctx, stateRoot, projectID, localDeviceID, session)
		if err != nil {
			if ctx.Err() != nil {
				return pullCheckSessionCounts{}, fmt.Errorf("pull: inspect session metadata: %w", ctx.Err())
			}
			counts.Attention++
			continue
		}
		if plan.HasForeignChanges() {
			counts.ForeignUpdates++
			counts.ForeignBranches += len(plan.Foreign)
		} else {
			counts.Unchanged++
		}
	}
	return counts, nil
}

func loadPullCheckPlan(ctx context.Context, stateRoot, projectID, localDeviceID string, session syncer.ProjectMetadataRef) (syncflow.PullPlan, error) {
	layout, err := syncer.NewObjectLayout(projectID, session.SessionID, localDeviceID)
	if err != nil {
		return syncflow.PullPlan{}, err
	}
	cursorStore, err := syncer.NewCursorStore(stateRoot, layout)
	if err != nil {
		return syncflow.PullPlan{}, err
	}
	cursor := syncer.NewPushCursor()
	savedCursor, err := cursorStore.Load(ctx)
	if errors.Is(err, syncer.ErrNoPushCursor) {
	} else if err != nil {
		return syncflow.PullPlan{}, err
	} else {
		cursor = savedCursor
	}

	pullTipStore, err := syncer.NewPullTipStore(stateRoot, layout)
	if err != nil {
		return syncflow.PullPlan{}, err
	}
	observed, err := syncflow.LoadObservedTips(ctx, pullTipStore)
	if err != nil {
		return syncflow.PullPlan{}, err
	}
	return syncflow.PlanPull(session.Devices, syncflow.PullOptions{
		LocalDeviceID: localDeviceID,
		LocalCursor:   cursor,
		Observed:      observed,
	})
}

func newPullCheckReport(counts pullCheckSessionCounts) pullCheckReport {
	result := pullCheckResultUpToDate
	if counts.Attention > 0 {
		result = pullCheckResultAttention
	} else if counts.ForeignUpdates > 0 {
		result = pullCheckResultUpdates
	}
	return pullCheckReport{
		Scope:    "project",
		Mode:     pullCheckModeMetadataOnly,
		Result:   result,
		Sessions: counts,
	}
}

func writePullCheckJSON(w io.Writer, report pullCheckReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writePullCheckText(w io.Writer, report pullCheckReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "check: %s\n", report.Mode); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "result: %s\n", report.Result); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "sessions: checked=%d foreign-updates=%d foreign-branches=%d unchanged=%d attention=%d\n",
		report.Sessions.Checked,
		report.Sessions.ForeignUpdates,
		report.Sessions.ForeignBranches,
		report.Sessions.Unchanged,
		report.Sessions.Attention)
	return err
}
