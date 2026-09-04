package main

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestPublishNativeReplicaMaterializedTargetPublishesOnlySuffix(t *testing.T) {
	ctx := context.Background()
	configDir := t.TempDir()
	projectRoot := t.TempDir()
	agentHome := t.TempDir()
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}
	identities := []*ecdh.PrivateKey{private}
	const (
		projectIdentity = "manual:materialized"
		deviceID        = "device"
		sessionID       = "session"
		nativeID        = "native"
		parentID        = "parent"
	)
	when := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := sessionhub.DeriveProjectKey(identifierKey, hubID, projectIdentity)
	if err != nil {
		t.Fatal(err)
	}
	nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, "codex", nativeID)
	if err != nil {
		t.Fatal(err)
	}
	replicaID, err := sessionhub.DeriveReplicaKey(identifierKey, sessionID, "codex", nativeKey, deviceID, 1)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := sessionhub.NewDefaultRegistry(identifierKey, when)
	if err != nil {
		t.Fatal(err)
	}
	projectRecord, err := registry.EnsureProject(identifierKey, sessionhub.ProjectIdentityManual, projectIdentity, when)
	if err != nil {
		t.Fatal(err)
	}
	if projectRecord.Descriptor.ProjectID != projectID {
		t.Fatalf("project id = %s, want %s", projectRecord.Descriptor.ProjectID, projectID)
	}
	if _, err := registry.EnsureSession(projectID, sessionhub.SessionDescriptor{
		Version: sessionhub.ModelVersion, SessionID: sessionID, ProjectID: projectID, Title: "materialized",
		CreatedAt: when, CreatedBy: sessionhub.SessionCreator{Agent: "codex", DeviceID: deviceID}, Lifecycle: sessionhub.SessionActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindNativeSession(projectID, sessionID, sessionhub.NativeSessionBinding{
		Agent: "codex", NativeSessionID: nativeID, BoundAt: when,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		t.Fatal(err)
	}

	space := adapter.PathSpace{ProjectRoot: projectRoot, AgentHome: agentHome}
	installation := adapter.Installation{DataDir: agentHome, Compatibility: adapter.CompatFull, Version: "1.2.3"}
	ref := adapter.SessionRef{Agent: "codex", NativeID: nativeID, Title: "materialized", CreatedAt: when}
	initialData := adapter.SessionData{Records: [][]byte{
		[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(projectRoot) + `","message":{"role":"user","content":"imported"}}`),
		[]byte(`{"type":"assistant","cwd":"` + filepath.ToSlash(projectRoot) + `","message":{"role":"assistant","content":"suffix one"}}`),
	}}
	initialStream, err := syncflow.CanonicalizeSession(initialData, space, installation)
	if err != nil {
		t.Fatal(err)
	}
	importDigest, err := syncer.DigestRecords(initialStream.Records[:1])
	if err != nil {
		t.Fatal(err)
	}
	emptyDigest := syncer.EmptyDigest()
	binding := sessionhub.LocalBinding{
		Version: sessionhub.ModelVersion, HubID: hubID, ProjectID: projectID, SessionID: sessionID,
		Agent: "codex", NativeSessionID: nativeID, ReplicaID: replicaID, Generation: 1,
		ReplicaCursor:      sessionhub.ReplicaCursor{NextShard: 1, HeadDigest: hex.EncodeToString(emptyDigest[:])},
		LocalSnapshot:      &sessionhub.LocalSessionSnapshot{RecordCount: 1, HeadDigest: hex.EncodeToString(importDigest[:])},
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: 1},
		Origin: sessionhub.BindingOrigin{
			Kind: sessionhub.ReplicaOriginLocalMaterialize, BaseHeads: []string{parentID},
			ImportBoundary: &sessionhub.ImportBoundary{RecordCount: 1, PrefixDigest: "sha256:" + hex.EncodeToString(importDigest[:])},
			Converter:      &sessionhub.ConverterProvenance{SourceViewVersion: 1, TargetAdapterVersion: "1"},
		},
	}
	if err := sessionhub.SaveLocalBinding(configDir, binding); err != nil {
		t.Fatal(err)
	}

	replicaLayout, err := syncer.NewReplicaLayout(hubID, projectID, sessionID, replicaID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	sessionLayout, err := replicaLayout.SessionLayout()
	if err != nil {
		t.Fatal(err)
	}
	parent := sessionhub.Contribution{
		Version: sessionhub.ModelVersion, ContributionID: parentID, SessionID: sessionID,
		Source:  sessionhub.ContributionSource{Agent: "claude-code", ReplicaID: "sourcereplica", DeviceID: "sourcedevice", Generation: 1},
		Parents: []string{},
		Ranges: []sessionhub.RangeRef{{
			ReplicaID: "sourcereplica", StartRecord: 0, EndRecord: 1,
			PrefixDigest: strings.Repeat("0", 64), RangeDigest: strings.Repeat("1", 64),
		}},
		EnvironmentRefs: []string{}, CreatedAt: when,
	}
	if err := syncer.PutContribution(ctx, store, public, sessionLayout, parent); err != nil {
		t.Fatal(err)
	}

	layout := adapter.CodexLayout{Home: agentHome}
	beforeRewrite := materializedRemoteKeys(t, store)
	rewrittenData := initialData
	rewrittenData.Records = append([][]byte(nil), initialData.Records...)
	rewrittenData.Records[0] = []byte(`{"type":"user","cwd":"` + filepath.ToSlash(projectRoot) + `","message":{"role":"user","content":"rewritten imported prefix"}}`)
	err = publishNativeReplica(ctx, configDir, deviceID, identifierKey, projectIdentity, layout, installation, store, public, configDir, ref, "legacy", rewrittenData, space, identities)
	if !errors.Is(err, syncflow.ErrMaterializePrefixRewrite) {
		t.Fatalf("rewritten prefix error = %v, want ErrMaterializePrefixRewrite", err)
	}
	if afterRewrite := materializedRemoteKeys(t, store); !reflect.DeepEqual(afterRewrite, beforeRewrite) {
		t.Fatalf("rewritten prefix changed remote keys: before=%v after=%v", beforeRewrite, afterRewrite)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := publishNativeReplica(ctx, configDir, deviceID, identifierKey, projectIdentity, layout, installation, store, public, configDir, ref, "legacy", initialData, space, identities); err != nil {
			t.Fatalf("publish materialized Replica attempt %d: %v", attempt+1, err)
		}
	}

	snapshot, err := syncer.FetchCompleteReplica(ctx, store, replicaLayout, identities)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Descriptor.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize || len(snapshot.Descriptor.Origin.BaseHeads) != 1 || snapshot.Descriptor.Origin.BaseHeads[0] != parentID {
		t.Fatalf("Replica origin = %+v", snapshot.Descriptor.Origin)
	}
	contributions, err := syncer.FetchSessionContributions(ctx, store, sessionLayout, identities)
	if err != nil {
		t.Fatal(err)
	}
	target := materializedTargetContributions(contributions, replicaID)
	if len(target) != 1 || target[0].Ranges[0].StartRecord != 1 || target[0].Ranges[0].EndRecord != 2 {
		t.Fatalf("first target Contributions = %+v", target)
	}
	firstID := target[0].ContributionID
	visible, err := includeMaterializedCursorContribution(ctx, store, sessionLayout, deviceID, identities, sessionhub.LocalBinding{
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: 2, LastContributionID: firstID},
	}, []sessionhub.Contribution{parent})
	if err != nil || len(visible) != 2 {
		t.Fatalf("direct cursor Contribution recovery = %+v, err=%v", visible, err)
	}

	appendedData := initialData
	appendedData.Records = append(append([][]byte(nil), initialData.Records...), []byte(`{"type":"user","cwd":"`+filepath.ToSlash(projectRoot)+`","message":{"role":"user","content":"suffix two"}}`))
	if err := publishNativeReplica(ctx, configDir, deviceID, identifierKey, projectIdentity, layout, installation, store, public, configDir, ref, "legacy", appendedData, space, identities); err != nil {
		t.Fatalf("publish appended materialized Replica: %v", err)
	}
	contributions, err = syncer.FetchSessionContributions(ctx, store, sessionLayout, identities)
	if err != nil {
		t.Fatal(err)
	}
	target = materializedTargetContributions(contributions, replicaID)
	if len(target) != 2 || target[1].Ranges[0].StartRecord != 2 || target[1].Ranges[0].EndRecord != 3 || len(target[1].Parents) != 1 || target[1].Parents[0] != firstID {
		t.Fatalf("appended target Contributions = %+v", target)
	}
	loaded, err := sessionhub.LoadLocalBinding(configDir, hubID, projectID, sessionID, replicaID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReplicaCursor.RecordCount != 3 || loaded.LocalSnapshot == nil || loaded.LocalSnapshot.RecordCount != 3 || loaded.ContributionCursor.EndRecord != 3 || loaded.ContributionCursor.LastContributionID != target[1].ContributionID {
		t.Fatalf("committed materialized binding = %+v", loaded)
	}
}

func TestLoadMaterializedReplicaBindingFailsClosedWhenTransactionSurvivesSidecar(t *testing.T) {
	root := t.TempDir()
	transactionID := strings.Repeat("a", 64)
	nativeID := "ctxhop-" + transactionID
	transaction := sessionhub.MaterializeTransaction{
		Version: sessionhub.MaterializeTransactionVersion, TransactionID: transactionID,
		HubID: "hub", ProjectID: "project", SessionID: "session", TargetAgent: "codex", TargetNativeID: nativeID,
		ReplicaID: "replica", ContextPolicy: "causal-head", SelectedHeads: []string{"head"}, PreviewDigest: strings.Repeat("b", 64),
		SelectedRecordCount: 1, TargetRecordCount: 1, State: sessionhub.MaterializeTransactionCommitted,
		CreatedAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}
	if err := sessionhub.SaveMaterializeTransaction(root, transaction); err != nil {
		t.Fatal(err)
	}
	_, err := loadMaterializedReplicaBinding(root, "hub", "project", "session", "replica", "codex", nativeID, 1)
	if !errors.Is(err, syncflow.ErrMaterializeBoundaryUnknown) {
		t.Fatalf("missing sidecar error = %v, want ErrMaterializeBoundaryUnknown", err)
	}
}

func materializedTargetContributions(values []sessionhub.Contribution, replicaID string) []sessionhub.Contribution {
	result := make([]sessionhub.Contribution, 0)
	for _, contribution := range values {
		if contribution.Source.ReplicaID == replicaID {
			result = append(result, contribution)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Ranges[0].StartRecord < result[j].Ranges[0].StartRecord
	})
	return result
}

func materializedRemoteKeys(t *testing.T, store remote.Remote) []string {
	t.Helper()
	objects, err := store.List(context.Background(), "v2/")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	sort.Strings(keys)
	return keys
}
