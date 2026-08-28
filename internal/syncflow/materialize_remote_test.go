package syncflow

import (
	"context"
	"crypto/ecdh"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	ctxcrypto "github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestFetchMaterializePreviewReadsSelectedRemoteSourcesWithoutWriting(t *testing.T) {
	dataKey := ctxcrypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessionLayout, err := syncer.NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	replicaLayout, err := syncer.NewReplicaLayout("hub", "project", "session", "replicaclaude", "deviceclaude")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := syncerTestReplicaDescriptor(t, replicaLayout, "claude-code")
	records := [][]byte{[]byte(`{"type":"user","text":"continue"}`)}
	if _, err := syncer.PushReplica(context.Background(), store, public, replicaLayout, descriptor, syncer.NewPushCursor(), records, syncer.ReplicaPushOptions{
		Identities: []*ecdh.PrivateKey{private},
	}); err != nil {
		t.Fatalf("PushReplica: %v", err)
	}
	contribution := materializeSelectionContribution(t, "contribution", nil, descriptor, records, 0, 1)
	if err := syncer.PutContributionWithIdentities(context.Background(), store, public, sessionLayout, contribution, []*ecdh.PrivateKey{private}); err != nil {
		t.Fatalf("PutContribution: %v", err)
	}
	objectsBefore, err := store.List(context.Background(), "v2/hubs/hub/projects/project/sessions/session")
	if err != nil {
		t.Fatal(err)
	}

	target := &previewCaptureCapability{materializeCapabilityStub: materializeCapabilityStub{encoded: testEncodedContext()}}
	request := RemoteMaterializePreviewRequest{
		Store:      store,
		Identities: []*ecdh.PrivateKey{private},
		Layout:     sessionLayout,
		Heads:      []string{contribution.ContributionID},
		MaterializePreviewOptions: MaterializePreviewOptions{
			SourceCapabilities: map[string]adapter.MaterializeCapability{
				"claude-code": &materializeCapabilityStub{view: adapter.ContextView{
					Version:      adapter.MaterializeViewVersion,
					SourceAgent:  "claude-code",
					SourceFormat: "claude-code-jsonl",
					Items: []adapter.ContextItem{{
						Kind:        adapter.ContextItemUser,
						Text:        "continue",
						SourceIndex: 0,
						Completed:   true,
					}},
				}},
			},
			TargetAgent:      "codex",
			TargetCapability: target,
			Target: adapter.MaterializeTarget{
				PathSpace: adapter.PathSpace{ProjectRoot: `C:\project`, AgentHome: `C:\agent`},
			},
		},
	}
	preview, err := FetchMaterializePreview(context.Background(), request)
	if err != nil {
		t.Fatalf("FetchMaterializePreview: %v", err)
	}
	if len(preview.Coverage.SelectedIDs) != 1 || preview.Coverage.SelectedIDs[0] != contribution.ContributionID {
		t.Fatalf("preview coverage = %+v", preview.Coverage)
	}
	if len(preview.Sources) != 1 || preview.Sources[0].SourceAgent != "claude-code" || preview.Sources[0].RecordCount != 1 {
		t.Fatalf("preview sources = %+v", preview.Sources)
	}
	if preview.TargetAgent != "codex" || preview.ContextItems != 1 || preview.SelectedRecordCount != 1 {
		t.Fatalf("preview target/counts = %q/%d/%d", preview.TargetAgent, preview.ContextItems, preview.SelectedRecordCount)
	}
	objectsAfter, err := store.List(context.Background(), "v2/hubs/hub/projects/project/sessions/session")
	if err != nil {
		t.Fatal(err)
	}
	if len(objectsAfter) != len(objectsBefore) {
		t.Fatalf("preview wrote remote objects: before=%d after=%d", len(objectsBefore), len(objectsAfter))
	}
}

func TestFetchMaterializePreviewRejectsAmbiguousReplicaSource(t *testing.T) {
	layout, err := syncer.NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	replicaA, err := syncer.NewReplicaLayout("hub", "project", "session", "replica", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	replicaB, err := syncer.NewReplicaLayout("hub", "project", "session", "replica", "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	descriptorA := syncerTestReplicaDescriptor(t, replicaA, "claude-code")
	descriptorB := syncerTestReplicaDescriptor(t, replicaB, "codex")
	first := materializeSelectionContribution(t, "a", nil, descriptorA, [][]byte{[]byte(`{"n":1}`)}, 0, 1)
	second := materializeSelectionContribution(t, "b", nil, descriptorB, [][]byte{[]byte(`{"n":2}`)}, 0, 1)
	graph, err := sessionhub.NewContributionGraph("session", []sessionhub.Contribution{first, second})
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := graph.Select(first.ContributionID, second.ContributionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selectedReplicaLayouts(layout, graph, coverage.SelectedIDs); err == nil {
		t.Fatal("selectedReplicaLayouts accepted one Replica ID from two source identities")
	} else if !errors.Is(err, ErrMaterializeRemoteSourceConflict) {
		t.Fatalf("ambiguous Replica error = %v, want ErrMaterializeRemoteSourceConflict", err)
	}
}

func TestResolveMaterializeHeadsSupportsPoliciesDeterministically(t *testing.T) {
	claudeLayout, err := syncer.NewReplicaLayout("hub", "project", "session", "replicaclaude", "deviceclaude")
	if err != nil {
		t.Fatal(err)
	}
	codexLayout, err := syncer.NewReplicaLayout("hub", "project", "session", "replicacodex", "devicecodex")
	if err != nil {
		t.Fatal(err)
	}
	claude := syncerTestReplicaDescriptor(t, claudeLayout, "claude-code")
	codex := syncerTestReplicaDescriptor(t, codexLayout, "codex")
	records := [][]byte{[]byte(`{"n":1}`)}
	root := materializeSelectionContribution(t, "a", nil, claude, records, 0, 1)
	otherRoot := materializeSelectionContribution(t, "b", nil, codex, records, 0, 1)
	claudeChild := materializeSelectionContribution(t, "c", []string{"a"}, claude, records, 0, 1)
	graph, err := sessionhub.NewContributionGraph("session", []sessionhub.Contribution{claudeChild, otherRoot, root})
	if err != nil {
		t.Fatal(err)
	}

	causal, err := ResolveMaterializeHeads(graph, MaterializeContextCausalHead, []string{"c"}, "")
	if err != nil || len(causal) != 1 || causal[0] != "c" {
		t.Fatalf("causal heads = %v, err=%v", causal, err)
	}
	all, err := ResolveMaterializeHeads(graph, MaterializeContextAllHeads, nil, "")
	if err != nil || len(all) != 2 || all[0] != "b" || all[1] != "c" {
		t.Fatalf("all heads = %v, err=%v", all, err)
	}
	agentOnly, err := ResolveMaterializeHeads(graph, MaterializeContextAgentOnly, nil, "claude-code")
	if err != nil || len(agentOnly) != 2 || agentOnly[0] != "a" || agentOnly[1] != "c" {
		t.Fatalf("agent-only heads = %v, err=%v", agentOnly, err)
	}
	if _, err := ResolveMaterializeHeads(graph, MaterializeContextCausalHead, nil, ""); !errors.Is(err, ErrMaterializeHeadSelection) {
		t.Fatalf("causal automatic selection error = %v, want ErrMaterializeHeadSelection", err)
	}
	if _, err := ResolveMaterializeHeads(graph, MaterializeContextAllHeads, []string{"c"}, ""); !errors.Is(err, ErrMaterializeHeadSelection) {
		t.Fatalf("all-heads explicit head error = %v, want ErrMaterializeHeadSelection", err)
	}
}
