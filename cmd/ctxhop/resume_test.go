package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestParseResumeOptionsUsesPreviewAndWorkspaceScopes(t *testing.T) {
	options, err := parseResumeOptions([]string{"--preview", "--workspace", "native-session"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.preview || !options.workspace || options.session != "native-session" {
		t.Fatalf("options = %+v", options)
	}
	if _, err := parseResumeOptions([]string{"--apply", "native-session"}); err == nil {
		t.Fatal("resume accepted the removed --apply flag")
	}
	options, err = parseResumeOptions([]string{"logical-session", "--agent", " claude-code ", "--replica=replica-1"})
	if err != nil {
		t.Fatal(err)
	}
	if options.session != "logical-session" || options.agent != "claude-code" || options.replica != "replica-1" {
		t.Fatalf("interspersed options = %+v", options)
	}
	if _, err := parseResumeOptions([]string{"--agent="}); err == nil {
		t.Fatal("resume accepted an empty --agent value")
	}
}

func TestSafeResumePlanErrorPrioritizesContextFailure(t *testing.T) {
	timeout := safeResumePlanError(errors.Join(syncer.ErrIncompleteRemoteSession, context.DeadlineExceeded))
	if got, want := timeout.Error(), "resume: timed out while downloading the remote session; retry on a stable connection"; got != want {
		t.Fatalf("timeout error = %q, want %q", got, want)
	}
	cancelled := safeResumePlanError(errors.Join(syncer.ErrIncompleteRemoteSession, context.Canceled))
	if got, want := cancelled.Error(), "resume: remote session download was cancelled"; got != want {
		t.Fatalf("cancelled error = %q, want %q", got, want)
	}
}

func TestSelectResumeAgentUsesRemoteCodexMetadata(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))

	agent, err := selectResumeAgent(context.Background(), t.TempDir(), syncflow.SessionSummary{
		Agent:    "codex",
		NativeID: "codex-session",
	})
	if err != nil {
		t.Fatalf("selectResumeAgent: %v", err)
	}
	if agent.Layout == nil || agent.Layout.Name() != "codex" {
		t.Fatalf("selected agent = %+v", agent.Layout)
	}
}
func TestCollectResumeRestoresFingerprintCheckedSession(t *testing.T) {
	projectRoot := t.TempDir()
	runResumeGit(t, projectRoot, "init", "-q")
	runResumeGit(t, projectRoot, "remote", "add", "origin", "https://github.com/example/project.git")
	homeA := t.TempDir()
	homeB := t.TempDir()
	remoteRoot := t.TempDir()
	configDir := t.TempDir()
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	cleanupResumeRemoteRoot(t, remoteRoot)

	keyfile, recovery, err := crypto.NewKeyfile("passphrase")
	if err != nil || recovery == "" {
		t.Fatalf("new keyfile = %v, recovery empty=%v", err, recovery == "")
	}
	if err := syncer.PublishKeyfile(context.Background(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	dataKey, err := keyfile.UnlockWithPassphrase("passphrase")
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}

	c := config.New()
	c.Remote = config.Remote{Type: "dir", Path: remoteRoot}
	c.IdentityPublic = public.Bytes()
	secrets := &config.Secrets{IdentifierKey: identifierKey}
	if err := config.SaveSecrets(configDir, secrets); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	if _, err := config.EnsureDeviceID(configDir, c, identifierKey); err != nil {
		t.Fatal(err)
	}

	layoutA := adapter.Layout{Home: homeA}
	ref := adapter.SessionRef{
		Agent:     "claude-code",
		NativeID:  "native-one",
		Title:     "resume me",
		CreatedAt: time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC),
	}
	sessionPath := layoutA.SessionFile(projectRoot, ref.NativeID)
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	record := []byte(`{"type":"user","cwd":"` + filepath.ToSlash(projectRoot) + `","timestamp":"2026-08-15T02:00:00Z","message":{"role":"user","content":"resume"}}`)
	if err := os.WriteFile(sessionPath, append(record, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	projectID, err := crypto.ProjectID(identifierKey, "github.com/example/project")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := syncer.NewQueueStore(configDir)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := syncflow.NewQueuedPusher(queue, syncer.DefaultRetryPolicy(), classifyPushFailure)
	if err != nil {
		t.Fatal(err)
	}
	installation := adapter.Installation{DataDir: homeA, Compatibility: adapter.CompatFull}
	space := adapter.PathSpace{ProjectRoot: projectRoot, AgentHome: homeA}
	publicKey, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	deviceID := c.Device.ID
	summary := pushDiscoveredSessions(context.Background(), deviceID, identifierKey, projectID, layoutA, installation, space, store, publicKey, pusher, configDir, projectRoot, []adapter.SessionRef{ref})
	if summary.Pushed != 1 || summary.Failed != 0 {
		t.Fatalf("push summary = %+v", summary)
	}
	legacySessionID, err := crypto.SessionID(identifierKey, projectID, ref.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	nativeData, err := layoutA.ReadSession(adapter.SessionRef{NativeID: ref.NativeID, ProjectPath: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := publishNativeReplica(context.Background(), configDir, deviceID, identifierKey, "github.com/example/project", layoutA, installation, store, publicKey, configDir, ref, legacySessionID, nativeData, space, nil); err != nil {
		t.Fatalf("publish v2 NativeReplica: %v", err)
	}

	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", homeB)
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	v2ProjectID, err := sessionhub.DeriveProjectKey(identifierKey, hubID, "github.com/example/project")
	if err != nil {
		t.Fatal(err)
	}
	logicalSessionID, err := sessionhub.DeriveNativeLogicalSessionKey(identifierKey, v2ProjectID, "claude-code", ref.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	var v2PreviewOutput bytes.Buffer
	v2Preview, err := collectResume(context.Background(), c, configDir, projectRoot, resumeOptions{
		allowLimited: true,
		session:      logicalSessionID,
		agent:        "claude-code",
		preview:      true,
	}, strings.NewReader("passphrase\n"), &v2PreviewOutput)
	if err != nil {
		t.Fatalf("Session Hub preview resume: %v\noutput: %s", err, v2PreviewOutput.String())
	}
	if !v2Preview.Preview || v2Preview.LogicalSession != logicalSessionID || v2Preview.Agent != "claude-code" || v2Preview.ReplicaID == "" || v2Preview.Workspace != "consistent" {
		t.Fatalf("Session Hub preview report = %+v", v2Preview)
	}
	candidateOptions := resumeOptions{allowLimited: true, session: ref.NativeID}
	previewOptions := candidateOptions
	previewOptions.preview = true
	var previewOutput bytes.Buffer
	preview, err := collectResume(context.Background(), c, configDir, projectRoot, previewOptions, strings.NewReader("passphrase\n"), &previewOutput)
	if err != nil {
		t.Fatalf("preview resume: %v\noutput: %s", err, previewOutput.String())
	}
	if !preview.Preview || preview.Environment == nil || preview.Workspace != "consistent" {
		t.Fatalf("preview report = %+v", preview)
	}
	if _, err := os.Stat((adapter.Layout{Home: homeB}).SessionFile(projectRoot, ref.NativeID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created a native session, stat err = %v", err)
	}

	v2Report, err := collectResume(context.Background(), c, configDir, projectRoot, resumeOptions{
		allowLimited: true,
		session:      logicalSessionID,
		agent:        "claude-code",
	}, strings.NewReader("passphrase\n"), io.Discard)
	if err != nil {
		t.Fatalf("Session Hub resume: %v", err)
	}
	if v2Report.LogicalSession != logicalSessionID || v2Report.Agent != "claude-code" || v2Report.ReplicaID == "" || v2Report.Workspace != "consistent" || v2Report.LocalState != "missing" || v2Report.RemoteRecords == 0 {
		t.Fatalf("Session Hub resume report = %+v", v2Report)
	}
	binding, err := sessionhub.LoadLocalBinding(configDir, hubID, v2ProjectID, logicalSessionID, v2Report.ReplicaID, "claude-code")
	if err != nil {
		t.Fatalf("load LocalBinding: %v", err)
	}
	if binding.NativeSessionID != ref.NativeID || binding.Origin.Kind != sessionhub.ReplicaOriginSameAgentRestore || binding.ReplicaCursor.RecordCount == 0 || binding.ReplicaCursor.HeadDigest == "" {
		t.Fatalf("LocalBinding = %+v", binding)
	}
	v2ExactPreview, err := collectResume(context.Background(), c, configDir, projectRoot, resumeOptions{
		allowLimited: true,
		session:      logicalSessionID,
		agent:        "claude-code",
		preview:      true,
	}, strings.NewReader("passphrase\n"), io.Discard)
	if err != nil {
		t.Fatalf("Session Hub exact preview: %v", err)
	}
	if v2ExactPreview.LocalState != "exact" || v2ExactPreview.LocalRecords != v2ExactPreview.RemoteRecords || v2ExactPreview.AppendRecords != 0 {
		t.Fatalf("Session Hub exact state = %+v", v2ExactPreview)
	}

	var output bytes.Buffer
	report, err := collectResume(context.Background(), c, configDir, projectRoot, candidateOptions, strings.NewReader("passphrase\n"), &output)
	if err != nil {
		t.Fatalf("collect resume: %v\noutput: %s", err, output.String())
	}
	if report.Session != ref.NativeID || report.Workspace != "consistent" || report.Replaced {
		t.Fatalf("resume report = %+v", report)
	}
	restored, err := os.ReadFile((adapter.Layout{Home: homeB}).SessionFile(projectRoot, ref.NativeID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, []byte(`"content":"resume"`)) {
		t.Fatalf("restored session = %s", restored)
	}
}

func cleanupResumeRemoteRoot(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		var lastErr error
		for attempt := 0; attempt < 20; attempt++ {
			lastErr = os.RemoveAll(root)
			if lastErr == nil || errors.Is(lastErr, os.ErrNotExist) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Errorf("remove resume test remote root: %v", lastErr)
	})
}

func TestCollectResumeRoundTripsCodexContinuationBackToOriginalDevice(t *testing.T) {
	projectA := t.TempDir()
	projectB := t.TempDir()
	homeA := t.TempDir()
	homeB := t.TempDir()
	configA := t.TempDir()
	configB := t.TempDir()
	remoteRoot := t.TempDir()
	store, err := remote.NewDir(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}

	keyfile, recovery, err := crypto.NewKeyfile("passphrase")
	if err != nil || recovery == "" {
		t.Fatalf("new keyfile = %v, recovery empty=%v", err, recovery == "")
	}
	if err := syncer.PublishKeyfile(context.Background(), store, keyfile); err != nil {
		t.Fatal(err)
	}
	dataKey, err := keyfile.UnlockWithPassphrase("passphrase")
	if err != nil {
		t.Fatal(err)
	}
	defer dataKey.Close()
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}

	const identity = "manual:codex-roundtrip"
	cA := newCodexRoundTripConfig(t, configA, remoteRoot, public, identifierKey, "devicea", projectA, identity)
	cB := newCodexRoundTripConfig(t, configB, remoteRoot, public, identifierKey, "deviceb", projectB, identity)
	projectID, err := crypto.ProjectID(identifierKey, identity)
	if err != nil {
		t.Fatal(err)
	}

	const nativeID = "codex-roundtrip-session"
	layoutA := adapter.CodexLayout{Home: homeA}
	initial := [][]byte{
		codexRoundTripRecord(t, "2026-08-25T10:00:00Z", "session_meta", map[string]any{
			"id": nativeID, "cwd": projectA, "cli_version": "0.149.0",
		}),
		codexRoundTripRecord(t, "2026-08-25T10:01:00Z", "event_msg", map[string]any{"text": "from-a"}),
	}
	writeCodexRoundTripSession(t, layoutA, projectA, nativeID, initial)

	installationA := adapter.Installation{DataDir: homeA, Version: "0.149.0", Compatibility: adapter.CompatFull}
	spaceA := adapter.PathSpace{ProjectRoot: projectA, AgentHome: homeA}
	pusherA := newCodexRoundTripPusher(t, configA)
	refsA, err := layoutA.DiscoverSessions(projectA)
	if err != nil {
		t.Fatal(err)
	}
	if summary := pushDiscoveredSessions(context.Background(), cA.Device.ID, identifierKey, projectID, layoutA, installationA, spaceA, store, public, pusherA, configA, projectA, refsA); summary.Pushed != 1 || summary.Failed != 0 {
		t.Fatalf("device A initial push = %+v", summary)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "missing-claude"))
	t.Setenv("CODEX_HOME", homeB)
	if _, err := collectResume(context.Background(), cB, configB, projectB, resumeOptions{session: nativeID, allowDivergent: true}, strings.NewReader("passphrase\n"), io.Discard); err != nil {
		t.Fatalf("device B resume: %v", err)
	}

	layoutB := adapter.CodexLayout{Home: homeB}
	installationB := adapter.Installation{DataDir: homeB, Version: "0.149.0", Compatibility: adapter.CompatFull}
	spaceB := adapter.PathSpace{ProjectRoot: projectB, AgentHome: homeB}
	appendAndPushCodexRoundTripSession(t, layoutB, projectB, nativeID, "2026-08-25T10:02:00Z", "from-b", cB, identifierKey, projectID, installationB, spaceB, store, public, configB)

	t.Setenv("CODEX_HOME", homeA)
	reportA, err := collectResume(context.Background(), cA, configA, projectA, resumeOptions{session: nativeID, allowDivergent: true}, strings.NewReader("passphrase\n"), io.Discard)
	if err != nil {
		t.Fatalf("device A second resume: %v", err)
	}
	if !reportA.Merged && !reportA.Replaced {
		t.Fatalf("device A did not update its existing session: %+v", reportA)
	}
	appendAndPushCodexRoundTripSession(t, layoutA, projectA, nativeID, "2026-08-25T10:03:00Z", "from-a-2", cA, identifierKey, projectID, installationA, spaceA, store, public, configA)

	t.Setenv("CODEX_HOME", homeB)
	if _, err := collectResume(context.Background(), cB, configB, projectB, resumeOptions{session: nativeID, allowDivergent: true}, strings.NewReader("passphrase\n"), io.Discard); err != nil {
		t.Fatalf("device B second resume: %v", err)
	}
	appendAndPushCodexRoundTripSession(t, layoutB, projectB, nativeID, "2026-08-25T10:04:00Z", "from-b-2", cB, identifierKey, projectID, installationB, spaceB, store, public, configB)

	t.Setenv("CODEX_HOME", homeA)
	reportA, err = collectResume(context.Background(), cA, configA, projectA, resumeOptions{session: nativeID, allowDivergent: true}, strings.NewReader("passphrase\n"), io.Discard)
	if err != nil {
		t.Fatalf("device A third resume: %v", err)
	}
	if !reportA.Merged && !reportA.Replaced {
		t.Fatalf("device A did not update its session after multiple round trips: %+v", reportA)
	}
	dataA, err := layoutA.ReadSession(adapter.SessionRef{NativeID: nativeID, ProjectPath: projectA})
	if err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(dataA.Records, []byte("\n"))
	for _, marker := range []string{"from-a", "from-b", "from-a-2", "from-b-2"} {
		if !bytes.Contains(joined, []byte(marker)) {
			t.Fatalf("device A lost %s after multiple round trips: %s", marker, joined)
		}
	}
}

func appendAndPushCodexRoundTripSession(t *testing.T, layout adapter.CodexLayout, projectRoot, nativeID, timestamp, text string, c *config.Config, identifierKey []byte, projectID string, installation adapter.Installation, space adapter.PathSpace, store remote.Remote, public *ecdh.PublicKey, stateRoot string) {
	t.Helper()
	data, err := layout.ReadSession(adapter.SessionRef{NativeID: nativeID, ProjectPath: projectRoot})
	if err != nil {
		t.Fatalf("read %s session before append: %v", text, err)
	}
	continued := append(append([][]byte(nil), data.Records...), codexRoundTripRecord(t, timestamp, "event_msg", map[string]any{"text": text}))
	if err := layout.ReplaceSession(projectRoot, nativeID, continued); err != nil {
		t.Fatalf("append %s conversation: %v", text, err)
	}
	pusher := newCodexRoundTripPusher(t, stateRoot)
	refs, err := layout.DiscoverSessions(projectRoot)
	if err != nil {
		t.Fatalf("discover %s session: %v", text, err)
	}
	summary := pushDiscoveredSessions(context.Background(), c.Device.ID, identifierKey, projectID, layout, installation, space, store, public, pusher, stateRoot, projectRoot, refs)
	if summary.Pushed != 1 || summary.Failed != 0 {
		t.Fatalf("push %s continuation = %+v", text, summary)
	}
}

func newCodexRoundTripConfig(t *testing.T, dir, remoteRoot string, public *ecdh.PublicKey, identifierKey []byte, deviceID, projectRoot, identity string) *config.Config {
	t.Helper()
	c := config.New()
	c.Device = config.Device{ID: deviceID, Name: deviceID}
	c.Remote = config.Remote{Type: "dir", Path: remoteRoot}
	c.IdentityPublic = public.Bytes()
	c.Projects.Bindings = []config.Binding{{Identity: identity, LocalRoot: projectRoot}}
	if err := config.SaveSecrets(dir, &config.Secrets{IdentifierKey: identifierKey}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	return c
}

func newCodexRoundTripPusher(t *testing.T, stateRoot string) syncflow.QueuedPusher {
	t.Helper()
	queue, err := syncer.NewQueueStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	pusher, err := syncflow.NewQueuedPusher(queue, syncer.DefaultRetryPolicy(), classifyPushFailure)
	if err != nil {
		t.Fatal(err)
	}
	return pusher
}

func writeCodexRoundTripSession(t *testing.T, layout adapter.CodexLayout, projectRoot, sessionID string, records [][]byte) {
	t.Helper()
	path := filepath.Join(layout.Home, "sessions", "2026", "08", "25", "rollout-2026-08-25-10-00-00-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for _, record := range records {
		data.Write(record)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func codexRoundTripRecord(t *testing.T, timestamp, recordType string, payload map[string]any) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]any{"timestamp": timestamp, "type": recordType, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func runResumeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}
