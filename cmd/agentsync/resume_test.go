package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

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

	t.Setenv("AGENTSYNC_CONFIG_DIR", configDir)
	t.Setenv("CLAUDE_CONFIG_DIR", homeB)
	candidateOptions := resumeOptions{allowLimited: true, session: ref.NativeID}
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

func runResumeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, output)
	}
}
