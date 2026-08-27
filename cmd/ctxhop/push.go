package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/gitstate"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
	workspacepkg "github.com/CCCCY-ci/ctxhop/internal/workspace"
)

const pushTimeout = 10 * time.Minute

const maxPushSessionWorkers = 4

type pushOptions struct {
	hook      bool
	session   string
	workspace bool
	gitStash  string
}

type pushSummary struct {
	Pushed          int
	Failed          int
	Skipped         int
	NoLocalSessions bool

	// pushedSessions is local metadata used to register successful v1 pushes
	// in the Phase 1 logical Session registry. It never carries session body
	// bytes and is intentionally omitted from command output.
	pushedSessions *[]pushedNativeSession

	// failureDetails contains only fixed stage names and finite failure
	// classes. It never carries session content, local paths, credentials or
	// provider response text.
	failureDetails string
}

func (s *pushSummary) fail(stage string, err error) {
	s.failContext("", stage, err)
}

func (s *pushSummary) failContext(agent, stage string, err error) {
	s.Failed++
	if errors.Is(err, syncer.ErrQueueItemBlocked) {
		stage = "queue-blocked"
	}

	detail := fmt.Sprintf("push failure: stage=%s", stage)
	classText := ""
	if pushFailureStageHasClass(stage) {
		class := classifyPushFailure(err)
		if class == syncer.FailureNone {
			class = syncer.FailureUnknown
		}
		classText = string(class)
		switch {
		case errors.Is(err, syncer.ErrQueueItemBlocked):
			classText = "blocked"
		case errors.Is(err, context.DeadlineExceeded):
			classText = "timeout"
		case errors.Is(err, context.Canceled):
			classText = "cancelled"
		}
		detail = fmt.Sprintf("%s, class=%s; run 'ctxhop doctor'", detail, classText)
	}
	if s.failureDetails != "" {
		s.failureDetails += "\n"
	}
	s.failureDetails += detail
	logPushFailure(agent, stage, classText, err)
}

func pushFailureStageHasClass(stage string) bool {
	switch stage {
	case "context", "session-id", "object-layout", "queue-key", "cursor-store", "cursor", "executor", "session-read", "workspace-fingerprint", "metadata", "remote-push", "replica-push", "queue-blocked", "environment-record", "workspace-record", "git-state-record", "git-transfer-capture", "git-transfer-upload", "git-transfer-record", "device-record", "project-record", "session-registry":
		return true
	default:
		return false
	}
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
	logPushFinished(summary, options.workspace)
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
	flags.BoolVar(&options.hook, "ctxhop-hook", false, "mark an automatic hook invocation")
	flags.BoolVar(&options.workspace, "workspace", false, "include the project workspace and Git state")
	flags.StringVar(&options.gitStash, "git-stash", "", "select an existing Git stash for the worktree transfer")
	if err := flags.Parse(args); err != nil {
		return pushOptions{}, fmt.Errorf("push: %w", err)
	}
	if flags.NArg() > 1 {
		return pushOptions{}, fmt.Errorf("push: unexpected argument %q", flags.Arg(1))
	}
	if flags.NArg() == 1 {
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
		options.workspace = true
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
	pushLock, err := syncer.AcquireLocalFileLock(ctx, filepath.Join(configDir, "push.lock"))
	if err != nil {
		return pushSummary{}, fmt.Errorf("push: acquire local push lock: %w", err)
	}
	defer pushLock.Close()
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return pushSummary{}, fmt.Errorf("push: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return pushSummary{}, errors.New("push: encryption identity is not configured; run 'ctxhop init'")
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
	needsReplicaScope := false
	for _, agent := range agents {
		refs := agent.Sessions
		if options.session != "" {
			refs = filterPushSession(refs, options.session)
		}
		if len(refs) != 0 {
			needsReplicaScope = true
			break
		}
	}
	if needsReplicaScope {
		if err := publishReplicaProjectScope(ctx, c.Device.ID, secrets.IdentifierKey, current.Identity.Value, current.Identity.Kind, store, public); err != nil {
			summary.fail("replica-push", err)
			return summary, nil
		}
	}
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
		partial := pushDiscoveredSessionsWithOptions(ctx, c.Device.ID, secrets.IdentifierKey, projectID, current.Identity.Value, agent.Layout, agent.Installation, space, store, public, pusher, configDir, current.Root, refs, pushSessionOptions{includeWorkspace: options.workspace, includeDirectoryWorkspace: options.workspace && !current.GitBacked, includeGitTransfer: options.workspace, gitStash: options.gitStash, skipConfig: !c.SyncConfigEnabled(), replicaIdentities: access.Identities})
		summary.Pushed += partial.Pushed
		summary.Failed += partial.Failed
		summary.Skipped += partial.Skipped
		if partial.failureDetails != "" {
			if summary.failureDetails != "" {
				summary.failureDetails += "\n"
			}
			summary.failureDetails += partial.failureDetails
		}
		mergePushedSessions(&summary, partial)
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
	if lenPushedSessions(summary) != 0 {
		if err := registerPushedSessions(configDir, secrets.IdentifierKey, c.Device.ID, current.Identity, *summary.pushedSessions); err != nil {
			summary.fail("session-registry", err)
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
	skipConfig                bool
	projectIdentity           string
	replicaIdentities         []*ecdh.PrivateKey
}

type pushedNativeSession struct {
	Agent           string
	NativeID        string
	LegacySessionID string
	Title           string
	CreatedAt       time.Time
}

func addPushedSession(summary *pushSummary, source pushedNativeSession) {
	if summary == nil {
		return
	}
	if summary.pushedSessions == nil {
		summary.pushedSessions = new([]pushedNativeSession)
	}
	*summary.pushedSessions = append(*summary.pushedSessions, source)
}

func mergePushedSessions(summary *pushSummary, partial pushSummary) {
	if summary == nil || partial.pushedSessions == nil || len(*partial.pushedSessions) == 0 {
		return
	}
	if summary.pushedSessions == nil {
		summary.pushedSessions = new([]pushedNativeSession)
	}
	*summary.pushedSessions = append(*summary.pushedSessions, (*partial.pushedSessions)...)
}

func lenPushedSessions(summary pushSummary) int {
	if summary.pushedSessions == nil {
		return 0
	}
	return len(*summary.pushedSessions)
}

func pushDiscoveredSessions(ctx context.Context, deviceID string, identifierKey []byte, projectID string, layout adapter.SessionLayout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef, includeWorkspace ...bool) pushSummary {
	options := pushSessionOptions{}
	if len(includeWorkspace) != 0 {
		options.includeWorkspace = includeWorkspace[0]
	}
	return pushDiscoveredSessionsWithOptions(ctx, deviceID, identifierKey, projectID, "", layout, installation, space, store, public, pusher, stateRoot, projectRoot, refs, options)
}

func pushDiscoveredSessionsWithOptions(ctx context.Context, deviceID string, identifierKey []byte, projectID, projectIdentity string, layout adapter.SessionLayout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, refs []adapter.SessionRef, options pushSessionOptions) pushSummary {
	options.projectIdentity = projectIdentity
	refs = deduplicatePushRefs(refs)
	if len(refs) == 0 {
		return pushSummary{}
	}

	workerCount := maxPushSessionWorkers
	if len(refs) < workerCount {
		workerCount = len(refs)
	}
	type result struct {
		index   int
		summary pushSummary
	}
	jobs := make(chan int)
	results := make(chan result, len(refs))
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				results <- result{
					index:   index,
					summary: pushOneDiscoveredSession(ctx, deviceID, identifierKey, projectID, layout, installation, space, store, public, pusher, stateRoot, projectRoot, refs[index], options),
				}
			}
		}()
	}
	for index := range refs {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)

	partials := make([]pushSummary, len(refs))
	for item := range results {
		partials[item.index] = item.summary
	}
	var summary pushSummary
	for _, partial := range partials {
		summary.Pushed += partial.Pushed
		summary.Failed += partial.Failed
		summary.Skipped += partial.Skipped
		if partial.failureDetails != "" {
			if summary.failureDetails != "" {
				summary.failureDetails += "\n"
			}
			summary.failureDetails += partial.failureDetails
		}
		mergePushedSessions(&summary, partial)
	}
	return summary
}

func pushOneDiscoveredSession(ctx context.Context, deviceID string, identifierKey []byte, projectID string, layout adapter.SessionLayout, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, pusher syncflow.QueuedPusher, stateRoot, projectRoot string, ref adapter.SessionRef, options pushSessionOptions) (summary pushSummary) {
	started := time.Now()
	defer func() {
		logPushSessionFinished(layout.Name(), summary, time.Since(started))
	}()
	fail := func(stage string, err error) {
		summary.failContext(layout.Name(), stage, err)
	}
	if err := ctx.Err(); err != nil {
		fail("context", err)
		return summary
	}
	sessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
	if err != nil {
		fail("session-id", err)
		return summary
	}
	objectLayout, err := syncer.NewObjectLayout(projectID, sessionID, deviceID)
	if err != nil {
		fail("object-layout", err)
		return summary
	}
	key, err := syncer.NewQueueKey(projectID, sessionID, deviceID)
	if err != nil {
		fail("queue-key", err)
		return summary
	}
	cursorStore, err := syncer.NewCursorStore(stateRoot, objectLayout)
	if err != nil {
		fail("cursor-store", err)
		return summary
	}
	cursor, err := cursorStore.Load(ctx)
	if errors.Is(err, syncer.ErrNoPushCursor) {
		cursor = syncer.NewPushCursor()
	} else if err != nil {
		fail("cursor", err)
		return summary
	}
	executor, err := syncer.NewAppendExecutor(store, public, objectLayout, cursorStore, syncer.DefaultPlanOptions())
	if err != nil {
		fail("executor", err)
		return summary
	}
	readRef := ref
	if readRef.ProjectPath == "" {
		readRef.ProjectPath = projectRoot
	}
	data, err := layout.ReadSession(readRef)
	if err != nil {
		fail("session-read", err)
		return summary
	}
	fingerprint, err := capturePushFingerprint(ctx, layout, projectRoot, data)
	if err != nil {
		fail("workspace-fingerprint", err)
		return summary
	}
	payload, err := syncflow.EncodeSessionSummaryWithFingerprint(ref, &fingerprint)
	if err != nil {
		fail("metadata", err)
		return summary
	}
	nextCursor, err := pusher.PushSessionWithMetadata(ctx, key, data, space, installation, executor, cursor, payload)
	if err != nil {
		fail("remote-push", err)
		return summary
	}
	if err := publishNativeReplica(ctx, stateRoot, deviceID, identifierKey, options.projectIdentity, layout, installation, store, public, stateRoot, ref, sessionID, data, space, options.replicaIdentities); err != nil {
		fail("replica-push", err)
		return summary
	}
	environmentCapture := adapter.EnvironmentFor(layout).Capture(data.Records, installation.Version, installation.DataDir, projectRoot, projectID)
	if options.skipConfig {
		environmentCapture = environmentCapture.WithoutConfig()
	}
	if err := syncer.PutEnvironmentManifest(ctx, store, public, objectLayout, environmentCapture.References, environmentCapture.Components); err != nil {
		fail("environment-record", err)
		return summary
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
			fail("workspace-record", captureErr)
			return summary
		}
		snapshot.RecordCount = nextCursor.RecordCount
		if nextCursor.RecordCount != 0 {
			snapshot.HeadDigest = fmt.Sprintf("%x", nextCursor.HeadDigest)
		}
		if publishErr := syncer.PutWorkspaceSnapshot(ctx, store, public, objectLayout, snapshot); publishErr != nil {
			fail("workspace-record", publishErr)
			return summary
		}
	}
	gitState, gitErr := gitstate.Capture(ctx, projectRoot, options.projectIdentity)
	if gitErr != nil {
		fail("git-state-record", gitErr)
		return summary
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
			fail("git-transfer-capture", transferErr)
			return summary
		}
	}
	if publishErr := syncer.PutGitState(ctx, store, public, objectLayout, gitState); publishErr != nil {
		fail("git-state-record", publishErr)
		return summary
	}
	if options.includeGitTransfer && (len(transfer.CommitBundle) != 0 || len(transfer.WorktreeBundle) != 0) {
		if publishErr := syncer.PutGitTransfer(ctx, store, public, objectLayout, transfer); publishErr != nil {
			fail("git-transfer-upload", publishErr)
			return summary
		}
	}
	summary.Pushed++
	agent := ref.Agent
	if agent == "" {
		agent = layout.Name()
	}
	addPushedSession(&summary, pushedNativeSession{
		Agent:           agent,
		NativeID:        ref.NativeID,
		LegacySessionID: sessionID,
		Title:           ref.Title,
		CreatedAt:       ref.CreatedAt,
	})
	return summary
}

func deduplicatePushRefs(refs []adapter.SessionRef) []adapter.SessionRef {
	seen := make(map[string]struct{}, len(refs))
	result := make([]adapter.SessionRef, 0, len(refs))
	for _, ref := range refs {
		if ref.NativeID == "" {
			result = append(result, ref)
			continue
		}
		if _, exists := seen[ref.NativeID]; exists {
			continue
		}
		seen[ref.NativeID] = struct{}{}
		result = append(result, ref)
	}
	return result
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
	case errors.Is(err, syncer.ErrReplicaImmutableConflict), errors.Is(err, syncer.ErrReplicaIdentityMismatch), errors.Is(err, syncer.ErrReplicaIncomplete), errors.Is(err, syncer.ErrReplicaCursorCommit), errors.Is(err, syncer.ErrReplicaObjectTooLarge):
		return syncer.FailureSessionCorrupt
	case errors.Is(err, remote.ErrCredentials):
		return syncer.FailureCredentials
	case errors.Is(err, remote.ErrPermission):
		return syncer.FailurePermission
	case errors.Is(err, remote.ErrStorageFull):
		return syncer.FailureStorageFull
	case errors.Is(err, remote.ErrNetwork), errors.Is(err, remote.ErrTransient):
		return syncer.FailureNetwork
	default:
		return syncer.FailureUnknown
	}
}
