package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/gitstate"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
	workspacepkg "github.com/CCCCY-ci/agentsync/internal/workspace"
)

const pushTimeout = 5 * time.Minute

type pushOptions struct {
	hook             bool
	session          string
	includeWorkspace bool
	includeGitState  bool
	gitStash         string
}

type pushSummary struct {
	Pushed          int
	Failed          int
	Skipped         int
	NoLocalSessions bool

	// failureDetails contains only fixed stage names and finite failure
	// classes. It never carries session content, local paths, credentials or
	// provider response text.
	failureDetails string
}

func (s *pushSummary) fail(stage string, err error) {
	s.Failed++

	detail := fmt.Sprintf("push failure: stage=%s", stage)
	switch stage {
	case "remote-push", "environment-record", "workspace-record", "git-state-record", "git-transfer-record", "device-record", "project-record":
		class := classifyPushFailure(err)
		if class == syncer.FailureNone {
			class = syncer.FailureUnknown
		}
		classText := string(class)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			classText = "timeout"
		case errors.Is(err, context.Canceled):
			classText = "cancelled"
		}
		detail = fmt.Sprintf("%s, class=%s; run 'agentsync doctor'", detail, classText)
	}
	if s.failureDetails != "" {
		s.failureDetails += "\n"
	}
	s.failureDetails += detail
}

func writePushFailureDetails(output io.Writer, details string) error {
	if details == "" {
		return nil
	}
	_, err := fmt.Fprintln(output, details)
	return err
}

func writePushSummary(output io.Writer, summary pushSummary) error {
	if _, err := fmt.Fprintf(output, "pushed: %d, failed: %d, skipped: %d\n", summary.Pushed, summary.Failed, summary.Skipped); err != nil {
		return err
	}
	if summary.NoLocalSessions {
		if _, err := fmt.Fprintln(output, "no local sessions found for this project; start Claude Code or Codex in this directory before pushing"); err != nil {
			return err
		}
	}
	return writePushFailureDetails(output, summary.failureDetails)
}

func init() {
	for i := range commands {
		if commands[i].name == "push" {
			commands[i].run = runPush
		}
	}
}

func runPush(args []string) error {
	return runPushWithIO(args, os.Stdout)
}

func runPushWithIO(args []string, output io.Writer) error {
	options, err := parsePushOptions(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("push: output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	summary, err := collectPush(ctx, c, configDir, ".", options)
	if err != nil {
		return err
	}
	if options.hook {
		return nil
	}
	if err := writePushSummary(output, summary); err != nil {
		return err
	}
	if summary.Failed != 0 {
		return errors.New("push: one or more sessions could not be synchronized")
	}
	return nil
}

func parsePushOptions(args []string) (pushOptions, error) {
	flags := flag.NewFlagSet("push", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options pushOptions
	flags.BoolVar(&options.hook, "agentsync-hook", false, "mark an automatic hook invocation")
	flags.StringVar(&options.session, "session", "", "push one native session ID")
	flags.BoolVar(&options.includeWorkspace, "include-workspace", false, "upload a filtered workspace snapshot; no-Git projects scan the safe project directory")
	flags.BoolVar(&options.includeGitState, "include-git-state", false, "upload explicit Git commit and worktree transfer data")
	flags.StringVar(&options.gitStash, "git-stash", "", "select an existing Git stash for the worktree transfer")
	if err := flags.Parse(args); err != nil {
		return pushOptions{}, fmt.Errorf("push: %w", err)
	}
	if flags.NArg() > 1 {
		return pushOptions{}, fmt.Errorf("push: unexpected argument %q", flags.Arg(1))
	}
	if flags.NArg() == 1 {
		if options.session != "" {
			return pushOptions{}, errors.New("push: specify a session either as an argument or with --session, not both")
		}
		options.session = flags.Arg(0)
	}
	if strings.ContainsRune(options.session, 0) {
		return pushOptions{}, errors.New("push: session ID contains an invalid character")
	}
	if options.gitStash != "" {
		if err := gitstate.ValidateStashRef(options.gitStash); err != nil {
			return pushOptions{}, fmt.Errorf("push: --git-stash: %w", err)
		}
		// Selecting a stash is itself the explicit opt-in for Git transfer data.
		options.includeGitState = true
	}
	return options, nil
}

func collectPush(ctx context.Context, c *config.Config, configDir, projectDir string, options pushOptions) (pushSummary, error) {
	if c == nil {
		return pushSummary{}, errors.New("push: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return pushSummary{}, fmt.Errorf("push: %w", err)
	}
	if configuredDeviceMode(c) == config.DeviceModeDisabled {
		return pushSummary{Skipped: 1}, nil
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return pushSummary{}, fmt.Errorf("push: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return pushSummary{}, errors.New("push: encryption identity is not configured; run 'agentsync init'")
	}

	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return pushSummary{}, fmt.Errorf("push: %s", reason)
	}
	if projectExcluded(c, current.Identity.Value) {
		return pushSummary{Skipped: 1}, nil
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: derive project identity: %w", err)
	}
	access, err := openAuthorizedDomain(ctx, c, configDir, "push")
	if err != nil {
		return pushSummary{}, err
	}
	defer access.close()
	store := access.Store
	public := access.Public
	queue, err := syncer.NewQueueStore(configDir)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: prepare pending queue: %w", err)
	}
	pusher, err := syncflow.NewQueuedPusher(queue, syncer.DefaultRetryPolicy(), classifyPushFailure)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: prepare pending queue: %w", err)
	}

	agents, err := adapter.DiscoverInstalled(ctx, current.Root)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: discover local agents: %w", err)
	}
	if len(agents) == 0 {
		return pushSummary{}, errors.New("push: no supported coding agent is installed; install Claude Code or Codex CLI")
	}

	var summary pushSummary
	found := false
	for _, agent := range agents {
		refs := agent.Sessions
		if options.session != "" {
			refs = filterPushSession(refs, options.session)
		}
		if len(refs) == 0 {
			continue
		}
		found = true
		space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: agent.Installation.DataDir}
		partial := pushDiscoveredSessionsWithOptions(ctx, c.Device.ID, secrets.IdentifierKey, projectID, current.Identity.Value, agent.Layout, agent.Installation, space, store, public, pusher, configDir, current.Root, refs, pushSessionOptions{includeWorkspace: options.includeWorkspace, includeDirectoryWorkspace: options.includeWorkspace && !current.GitBacked, includeGitTransfer: options.includeGitState, gitStash: options.gitStash})
		summary.Pushed += partial.Pushed
		summary.Failed += partial.Failed
		summary.Skipped += partial.Skipped
		if partial.failureDetails != "" {
			if summary.failureDetails != "" {
				summary.failureDetails += "\n"
			}
			summary.failureDetails += partial.failureDetails
		}
	}
	if options.session != "" && !found {
		return pushSummary{}, errors.New("push: requested session was not found in the current project")
	}
	if !found {
		return pushSummary{NoLocalSessions: true}, nil
	}

	if summary.Failed == 0 {
		if err := publishProjectAnnouncement(ctx, c, current.Identity, projectID, store, public); err != nil {
			summary.fail("project-record", err)
		}
		if err := publishPushDeviceRecord(ctx, c, store, public); err != nil {
			summary.fail("device-record", err)
		}
	}
	return summary, nil
}

func publishProjectAnnouncement(ctx context.Context, c *config.Config, identity project.Identity, projectID string, store remote.Remote, public *ecdh.PublicKey) error {
	if c == nil {
		return errors.New("project record: configuration is unavailable")
	}
	record, err := syncer.NewProjectAnnouncement(
		projectID,
		c.Device.ID,
		string(identity.Kind),
		identity.Value,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("project record: %w", err)
	}
	return syncer.PutProjectAnnouncement(ctx, store, public, record)
}

type pushSessionOptions struct {
	includeWorkspace          bool
	includeDirectoryWorkspace bool
	includeGitTransfer        bool
	gitStash                  string
	projectIdentity           string
}

func pushDiscoveredSessions(ctx context.Context, deviceID string, identifierKey []byte, projectID string, layout adapter.SessionLayout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef, includeWorkspace ...bool) pushSummary {
	options := pushSessionOptions{}
	if len(includeWorkspace) != 0 {
		options.includeWorkspace = includeWorkspace[0]
	}
	return pushDiscoveredSessionsWithOptions(ctx, deviceID, identifierKey, projectID, "", layout, installation, space, store, public, pusher, stateRoot, projectRoot, refs, options)
}

func pushDiscoveredSessionsWithOptions(ctx context.Context, deviceID string, identifierKey []byte, projectID, projectIdentity string, layout adapter.SessionLayout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef, options pushSessionOptions) pushSummary {
	var summary pushSummary
	options.projectIdentity = projectIdentity
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			summary.fail("context", err)
			continue
		}
		sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
		if err != nil {
			summary.fail("session-id", err)
			continue
		}
		objectLayout, err := syncer.NewObjectLayout(projectID, sessionID, deviceID)
		if err != nil {
			summary.fail("object-layout", err)
			continue
		}
		key, err := syncer.NewQueueKey(projectID, sessionID, deviceID)
		if err != nil {
			summary.fail("queue-key", err)
			continue
		}
		cursorStore, err := syncer.NewCursorStore(stateRoot, objectLayout)
		if err != nil {
			summary.fail("cursor-store", err)
			continue
		}
		cursor, err := cursorStore.Load(ctx)
		if errors.Is(err, syncer.ErrNoPushCursor) {
			cursor = syncer.NewPushCursor()
		} else if err != nil {
			summary.fail("cursor", err)
			continue
		}
		executor, err := syncer.NewAppendExecutor(store, public, objectLayout, cursorStore, syncer.DefaultPlanOptions())
		if err != nil {
			summary.fail("executor", err)
			continue
		}
		readRef := ref
		if readRef.ProjectPath == "" {
			readRef.ProjectPath = projectRoot
		}
		data, err := layout.ReadSession(readRef)
		if err != nil {
			summary.fail("session-read", err)
			continue
		}
		fingerprint, err := capturePushFingerprint(ctx, layout, projectRoot, data)
		if err != nil {
			summary.fail("workspace-fingerprint", err)
			continue
		}
		payload, err := syncflow.EncodeSessionSummaryWithFingerprint(ref, &fingerprint)
		if err != nil {
			summary.fail("metadata", err)
			continue
		}
		nextCursor, err := pusher.PushSessionWithMetadata(ctx, key, data, space, installation, executor, cursor, payload)
		if err != nil {
			summary.fail("remote-push", err)
			continue
		}
		environmentCapture := adapter.EnvironmentFor(layout).Capture(data.Records, installation.Version, installation.DataDir, projectRoot, projectID)
		if err := syncer.PutEnvironmentManifest(ctx, store, public, objectLayout, environmentCapture.References, environmentCapture.Components); err != nil {
			summary.fail("environment-record", err)
			continue
		}
		if options.includeWorkspace {
			var snapshot workspacepkg.Snapshot
			var captureErr error
			if options.includeDirectoryWorkspace {
				snapshot, captureErr = workspacepkg.CaptureDirectory(ctx, projectRoot)
			} else {
				snapshot, captureErr = workspacepkg.Capture(ctx, projectRoot, fingerprint)
			}
			if captureErr != nil {
				summary.fail("workspace-record", captureErr)
				continue
			}
			snapshot.RecordCount = nextCursor.RecordCount
			if nextCursor.RecordCount != 0 {
				snapshot.HeadDigest = fmt.Sprintf("%x", nextCursor.HeadDigest)
			}
			if publishErr := syncer.PutWorkspaceSnapshot(ctx, store, public, objectLayout, snapshot); publishErr != nil {
				summary.fail("workspace-record", publishErr)
				continue
			}
		}
		gitState, gitErr := gitstate.Capture(ctx, projectRoot, options.projectIdentity)
		if gitErr != nil {
			summary.fail("git-state-record", gitErr)
			continue
		}
		gitState.SessionRecordCount = nextCursor.RecordCount
		if nextCursor.RecordCount != 0 {
			gitState.SessionHeadDigest = fmt.Sprintf("%x", nextCursor.HeadDigest)
		}
		var transfer gitstate.Transfer
		if options.includeGitTransfer {
			var transferErr error
			gitState, transfer, transferErr = gitstate.CaptureTransferWithOptions(ctx, projectRoot, gitState, gitstate.TransferOptions{StashRef: options.gitStash})
			if transferErr != nil {
				summary.fail("git-transfer-record", transferErr)
				continue
			}
		}
		if publishErr := syncer.PutGitState(ctx, store, public, objectLayout, gitState); publishErr != nil {
			summary.fail("git-state-record", publishErr)
			continue
		}
		if options.includeGitTransfer && (len(transfer.CommitBundle) != 0 || len(transfer.WorktreeBundle) != 0) {
			if publishErr := syncer.PutGitTransfer(ctx, store, public, objectLayout, transfer); publishErr != nil {
				summary.fail("git-transfer-record", publishErr)
				continue
			}
		}
		summary.Pushed++
	}
	return summary
}

func capturePushFingerprint(ctx context.Context, layout adapter.SessionLayout, projectRoot string, data adapter.SessionData) (project.Fingerprint, error) {
	accesses := layout.TouchedFiles(data.Records, projectRoot)
	touched := make([]string, 0, len(accesses))
	for _, access := range accesses {
		touched = append(touched, access.Path)
	}
	return project.Capture(ctx, projectRoot, touched)
}

func filterPushSession(refs []adapter.SessionRef, nativeID string) []adapter.SessionRef {
	for _, ref := range refs {
		if ref.NativeID == nativeID {
			return []adapter.SessionRef{ref}
		}
	}
	return nil
}

func projectExcluded(c *config.Config, identity string) bool {
	if c == nil {
		return false
	}
	for _, excluded := range c.Projects.Excluded {
		if excluded == identity {
			return true
		}
	}
	return false
}

func projectPushOnly(c *config.Config, identity string) bool {
	if c == nil {
		return false
	}
	for _, value := range c.Projects.PushOnly {
		if value == identity {
			return true
		}
	}
	return false
}

func classifyPushFailure(err error) syncer.FailureClass {
	if err == nil {
		return syncer.FailureNone
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return syncer.FailureUnknown
	case errors.Is(err, syncflow.ErrSessionNotPushable), errors.Is(err, syncflow.ErrInvalidPathSpace), errors.Is(err, syncflow.ErrInvalidSessionSnapshot):
		return syncer.FailureSessionCorrupt
	case errors.Is(err, syncer.ErrLocalHistoryChanged), errors.Is(err, syncer.ErrInvalidCursorState), errors.Is(err, syncer.ErrCursorCommit):
		return syncer.FailureSessionCorrupt
	default:
		return syncer.FailureNetwork
	}
}
