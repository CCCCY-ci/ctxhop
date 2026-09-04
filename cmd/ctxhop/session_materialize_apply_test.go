package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestApplyMaterializeExecutionWaitsForPushLock(t *testing.T) {
	configDir := t.TempDir()
	lock, err := syncer.AcquireLocalFileLock(context.Background(), filepath.Join(configDir, "push.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	agentHome := t.TempDir()
	execution := materializeExecution{
		Preview: syncflow.MaterializePreview{
			Coverage:      sessionhub.Coverage{SelectedIDs: []string{"contribution"}},
			SelectedHeads: []string{"head"},
			Sources: []syncflow.MaterializeSourceSummary{{
				ContributionID: "contribution",
				SourceAgent:    "codex",
				ReplicaID:      "replica",
				StartRecord:    0,
				EndRecord:      1,
				RecordCount:    1,
				ContextItems:   1,
				SourceFormat:   "codex-jsonl",
			}},
			TargetAgent:          "claude-code",
			TargetNativeID:       "ctxhop-target",
			SourceViewVersion:    adapter.MaterializeViewVersion,
			TargetAdapterVersion: adapter.ClaudeMaterializeAdapterVersion,
			SelectedRecordCount:  1,
			ContextItems:         1,
			Stats:                adapter.MaterializeStats{Converted: 1},
			EncodedRecords:       [][]byte{[]byte(`{"type":"user"}`)},
		},
		Target: adapter.AgentSessions{
			Layout:       adapter.Layout{Home: agentHome},
			Installation: adapter.Installation{DataDir: agentHome},
		},
		TargetCapability: adapter.Layout{Home: agentHome},
		ConfigDir:        configDir,
		ProjectRoot:      `C:\project`,
		IdentifierKey:    []byte(strings.Repeat("k", 32)),
		LocalDeviceID:    "device",
		HubID:            "hub",
		ProjectID:        "project",
		SessionID:        "session",
		TransactionID:    "transaction",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := applyMaterializeExecution(ctx, execution, io.Discard, false); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materialize lock error = %v, want context deadline exceeded", err)
	}
}

func TestApplyMaterializeExecutionCommitsLocalStateAndRetriesIdempotently(t *testing.T) {
	configDir := t.TempDir()
	agentHome := t.TempDir()
	identifierKey := []byte(strings.Repeat("k", 32))
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := sessionhub.DeriveProjectKey(identifierKey, hubID, "github.com/example/app")
	if err != nil {
		t.Fatal(err)
	}
	const (
		projectRoot = `C:\project`
		targetID    = "ctxhop-target"
		sessionID   = "session"
	)
	record := []byte(`{"type":"user","sessionId":"ctxhop-target","timestamp":"2026-08-29T00:00:00Z","cwd":"C:\\project","message":{"role":"user","content":"hello"},"completed":true}`)
	preview := syncflow.MaterializePreview{
		Coverage:      sessionhub.Coverage{SelectedIDs: []string{"contribution"}},
		SelectedHeads: []string{"head"},
		Sources: []syncflow.MaterializeSourceSummary{{
			ContributionID: "contribution",
			SourceAgent:    "codex",
			ReplicaID:      "replica",
			StartRecord:    0,
			EndRecord:      1,
			RecordCount:    1,
			ContextItems:   1,
			SourceFormat:   "codex-jsonl",
		}},
		TargetAgent:          "claude-code",
		TargetNativeID:       targetID,
		SourceViewVersion:    adapter.MaterializeViewVersion,
		TargetAdapterVersion: adapter.ClaudeMaterializeAdapterVersion,
		SelectedRecordCount:  1,
		ContextItems:         1,
		Stats:                adapter.MaterializeStats{Converted: 1},
		EncodedRecords:       [][]byte{record},
	}
	execution := materializeExecution{
		Report: materializePreviewReport{
			Preview:        false,
			Scope:          "project",
			HubID:          hubID,
			ProjectID:      projectID,
			SessionID:      sessionID,
			ContextPolicy:  materializeContextCausal,
			SelectedHeads:  []string{"head"},
			TargetAgent:    "claude-code",
			TargetNativeID: targetID,
		},
		Preview: preview,
		Target: adapter.AgentSessions{
			Layout:       adapter.Layout{Home: agentHome},
			Installation: adapter.Installation{DataDir: agentHome},
		},
		TargetCapability: adapter.Layout{Home: agentHome},
		ConfigDir:        configDir,
		ProjectRoot:      projectRoot,
		IdentityKind:     sessionhub.ProjectIdentityRemote,
		IdentityValue:    "github.com/example/app",
		IdentifierKey:    identifierKey,
		LocalDeviceID:    "device",
		HubID:            hubID,
		ProjectID:        projectID,
		SessionID:        sessionID,
		TransactionID: materializeRequestID(hubID, projectID, materializeOptions{
			sessionID:     sessionID,
			targetAgent:   "claude-code",
			contextPolicy: materializeContextCausal,
			heads:         []string{"head"},
		}),
	}

	var firstOutput bytes.Buffer
	if err := applyMaterializeExecution(context.Background(), execution, &firstOutput, false); err != nil {
		t.Fatalf("first apply error = %v", err)
	}
	if !strings.Contains(firstOutput.String(), "created-and-committed") {
		t.Fatalf("first output = %q", firstOutput.String())
	}
	layout := execution.Target.Layout.(adapter.Layout)
	data, err := layout.ReadSession(adapter.SessionRef{Agent: "claude-code", NativeID: targetID, ProjectPath: projectRoot})
	if err != nil || len(data.Records) != 1 || string(data.Records[0]) != string(record) {
		t.Fatalf("target session = %+v, err=%v", data, err)
	}
	binding, err := materializeLocalBinding(execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionhub.LoadLocalBinding(configDir, hubID, projectID, sessionID, binding.ReplicaID, "claude-code"); err != nil {
		t.Fatalf("local binding was not committed: %v", err)
	}
	transaction, err := sessionhub.LoadMaterializeTransaction(configDir, hubID, projectID, sessionID, execution.TransactionID)
	if err != nil || transaction.State != sessionhub.MaterializeTransactionCommitted {
		t.Fatalf("transaction = %+v, err=%v", transaction, err)
	}
	registry, err := sessionhub.LoadRegistry(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.FindSessionByNative(projectID, "claude-code", targetID, ""); !ok {
		t.Fatal("registry did not bind target native session")
	}

	var secondOutput bytes.Buffer
	if err := applyMaterializeExecution(context.Background(), execution, &secondOutput, false); err != nil {
		t.Fatalf("retry apply error = %v", err)
	}
	if !strings.Contains(secondOutput.String(), "already-committed") {
		t.Fatalf("retry output = %q", secondOutput.String())
	}
}

func TestMaterializeBindingUsesReplicaCanonicalBytes(t *testing.T) {
	const (
		projectRoot = `C:\project`
		targetID    = "ctxhop-target"
	)
	rawRecords := [][]byte{
		[]byte(`{"type":"user","cwd":"C:\\project","message":{"role":"user","content":"hello"},"timestamp":"2026-09-04T08:00:01Z"}`),
	}
	execution := materializeExecution{
		Preview: syncflow.MaterializePreview{
			SelectedHeads:        []string{"head"},
			TargetAgent:          "claude-code",
			TargetNativeID:       targetID,
			SourceViewVersion:    adapter.MaterializeViewVersion,
			TargetAdapterVersion: adapter.ClaudeMaterializeAdapterVersion,
			EncodedRecords:       rawRecords,
		},
		Target: adapter.AgentSessions{
			Installation: adapter.Installation{
				DataDir:       `C:\agent`,
				Compatibility: adapter.CompatFull,
			},
		},
		ProjectRoot:   projectRoot,
		IdentifierKey: []byte(strings.Repeat("k", 32)),
		LocalDeviceID: "device",
		HubID:         "hub",
		ProjectID:     "project",
		SessionID:     "session",
	}

	canonicalRecords, err := canonicalizeMaterializeTarget(execution)
	if err != nil {
		t.Fatalf("canonicalizeMaterializeTarget() error = %v", err)
	}
	if len(canonicalRecords) != len(rawRecords) || bytes.Equal(canonicalRecords[0], rawRecords[0]) {
		t.Fatalf("canonical records = %q, raw = %q; expected a distinct canonical byte stream", canonicalRecords[0], rawRecords[0])
	}

	binding, err := materializeLocalBindingForRecords(execution, canonicalRecords)
	if err != nil {
		t.Fatalf("materializeLocalBindingForRecords() error = %v", err)
	}
	if err := syncflow.ValidateMaterializedPushPreflight(binding, canonicalRecords); err != nil {
		t.Fatalf("canonical materialized preflight error = %v", err)
	}
	if err := syncflow.ValidateMaterializedPushPreflight(binding, rawRecords); !errors.Is(err, syncflow.ErrMaterializePrefixRewrite) {
		t.Fatalf("raw materialized preflight error = %v, want ErrMaterializePrefixRewrite", err)
	}
}
