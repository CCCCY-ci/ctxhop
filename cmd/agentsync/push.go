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

const pushTimeout = 30 * time.Second

type pushOptions struct {
	hook    bool
	session string
}

type pushSummary struct {
	Pushed  int
	Failed  int
	Skipped int
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
	if _, err := fmt.Fprintf(output, "pushed: %d, failed: %d, skipped: %d\n", summary.Pushed, summary.Failed, summary.Skipped); err != nil {
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
	public, err := ecdh.X25519().NewPublicKey(c.IdentityPublic)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: validate encryption identity: %w", err)
	}
	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: configure backend: %s", safeBackendSetupError(err))
	}
	if _, err := fetchValidatedRemoteKeyfile(ctx, c, store, "push"); err != nil {
		return pushSummary{}, err
	}
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

	space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installation.DataDir}
	summary := pushDiscoveredSessions(ctx, c.Device.ID, secrets.IdentifierKey, projectID, layout, installation, space, store, public, pusher, configDir, current.Root, refs)
	if summary.Pushed > 0 {
		if err := publishPushDeviceRecord(ctx, c, store, public); err != nil {
			summary.Failed++
		}
	}
	return summary, nil
}

func pushDiscoveredSessions(ctx context.Context, deviceID string, identifierKey []byte, projectID string, layout adapter.Layout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef) pushSummary {
	var summary pushSummary
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			summary.Failed++
			continue
		}
		sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
		if err != nil {
			summary.Failed++
			continue
		}
		objectLayout, err := syncer.NewObjectLayout(projectID, sessionID, deviceID)
		if err != nil {
			summary.Failed++
			continue
		}
		key, err := syncer.NewQueueKey(projectID, sessionID, deviceID)
		if err != nil {
			summary.Failed++
			continue
		}
		cursorStore, err := syncer.NewCursorStore(stateRoot, objectLayout)
		if err != nil {
			summary.Failed++
			continue
		}
		cursor, err := cursorStore.Load(ctx)
		if errors.Is(err, syncer.ErrNoPushCursor) {
			cursor = syncer.NewPushCursor()
		} else if err != nil {
			summary.Failed++
			continue
		}
		executor, err := syncer.NewAppendExecutor(store, public, objectLayout, cursorStore, syncer.DefaultPlanOptions())
		if err != nil {
			summary.Failed++
			continue
		}
		data, err := adapter.ReadSessionFile(layout.SessionFile(projectRoot, ref.NativeID))
		if err != nil {
			summary.Failed++
			continue
		}
		fingerprint, err := capturePushFingerprint(ctx, projectRoot, data)
		if err != nil {
			summary.Failed++
			continue
		}
		payload, err := syncflow.EncodeSessionSummaryWithFingerprint(ref, &fingerprint)
		if err != nil {
			summary.Failed++
			continue
		}
		if _, err := pusher.PushSessionWithMetadata(ctx, key, data, space, installation, executor, cursor, payload); err != nil {
			summary.Failed++
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
