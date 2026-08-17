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
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

const pushTimeout = 5 * time.Minute

type pushOptions struct {
	hook    bool
	session string
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
	case "remote-push", "device-record", "project-record":
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
		if _, err := fmt.Fprintln(output, "no local Claude Code sessions found for this project; start Claude Code in this directory before pushing"); err != nil {
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

	home, err := adapter.DefaultHome()
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: locate Claude Code: %w", err)
	}
	layout := adapter.Layout{Home: home}
	installation, err := layout.Detect(ctx)
	if errors.Is(err, adapter.ErrNotInstalled) {
		return pushSummary{}, errors.New("push: Claude Code is not installed")
	}
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: inspect Claude Code: %w", err)
	}
	refs, err := layout.DiscoverSessions(current.Root)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: discover local sessions: %w", err)
	}
	if options.session != "" {
		refs = filterPushSession(refs, options.session)
		if len(refs) == 0 {
			return pushSummary{}, errors.New("push: requested session was not found in the current project")
		}
	}

	if len(refs) == 0 {
		return pushSummary{NoLocalSessions: true}, nil
	}

	space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installation.DataDir}
	summary := pushDiscoveredSessions(ctx, c.Device.ID, secrets.IdentifierKey, projectID, layout, installation, space, store, public, pusher, configDir, current.Root, refs)
	if len(refs) > 0 && summary.Failed == 0 {
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

func pushDiscoveredSessions(ctx context.Context, deviceID string, identifierKey []byte, projectID string, layout adapter.Layout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef) pushSummary {
	var summary pushSummary
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
		data, err := adapter.ReadSessionFile(layout.SessionFile(projectRoot, ref.NativeID))
		if err != nil {
			summary.fail("session-read", err)
			continue
		}
		fingerprint, err := capturePushFingerprint(ctx, projectRoot, data)
		if err != nil {
			summary.fail("workspace-fingerprint", err)
			continue
		}
		payload, err := syncflow.EncodeSessionSummaryWithFingerprint(ref, &fingerprint)
		if err != nil {
			summary.fail("metadata", err)
			continue
		}
		if _, err := pusher.PushSessionWithMetadata(ctx, key, data, space, installation, executor, cursor, payload); err != nil {
			summary.fail("remote-push", err)
			continue
		}
		summary.Pushed++
	}
	return summary
}

func capturePushFingerprint(ctx context.Context, projectRoot string, data adapter.SessionData) (project.Fingerprint, error) {
	accesses := adapter.TouchedFiles(data.Records, projectRoot)
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
