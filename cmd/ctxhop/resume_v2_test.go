package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestChooseNativeResumeCandidateRequiresAgentForMultipleSources(t *testing.T) {
	identifierKey := []byte(strings.Repeat("k", 32))
	group, legacy := nativeResumeTestGroup(t, identifierKey)

	_, err := chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"",
		"",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "specify --agent") {
		t.Fatalf("ambiguous Agent error = %v", err)
	}

	candidate, err := chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"claude-code",
		"",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("explicit Agent selection: %v", err)
	}
	if candidate.Replica.Descriptor.Source.Agent != "claude-code" || candidate.NativeID != "claude-native" || !candidate.HasLegacy {
		t.Fatalf("selected candidate = %+v", candidate)
	}
	agents, replicas := nativeResumeOmissions(group, candidate.Replica)
	if strings.Join(agents, ",") != "codex" || len(replicas) != 1 || replicas[0] != "replicacodex" {
		t.Fatalf("omissions = agents %v replicas %v", agents, replicas)
	}
}

func TestChooseNativeResumeCandidateRequiresReplicaWhenOneAgentHasSeveralReplicas(t *testing.T) {
	identifierKey := []byte(strings.Repeat("k", 32))
	group, legacy := nativeResumeTestGroup(t, identifierKey)
	extra := nativeResumeReplica(t, identifierKey, "codex", "codex-native-2", "replicacodex2", "deviceb", true)
	group.Replicas = append(group.Replicas, extra)
	legacy = append(legacy, syncer.ProjectMetadataRef{
		SessionID: "legacy-codex-2",
		Devices:   []syncer.MetadataRef{nativeResumeLegacyMetadata(t, "codex", "codex-native-2", time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC))},
	})

	_, err := chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"codex",
		"",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "specify --replica") {
		t.Fatalf("ambiguous Replica error = %v", err)
	}

	candidate, err := chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"codex",
		"replicacodex2",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("explicit Replica selection: %v", err)
	}
	if candidate.Replica.Descriptor.ReplicaID != "replicacodex2" || candidate.NativeID != "codex-native-2" {
		t.Fatalf("selected Replica = %+v", candidate)
	}

	candidate, err = chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"",
		"replicacodex2",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("Replica-only selection: %v", err)
	}
	if candidate.Replica.Descriptor.Source.Agent != "codex" || candidate.NativeID != "codex-native-2" {
		t.Fatalf("Replica-only candidate = %+v", candidate)
	}
}

func TestChooseNativeResumeCandidateRejectsIncompleteReplica(t *testing.T) {
	identifierKey := []byte(strings.Repeat("k", 32))
	group, legacy := nativeResumeTestGroup(t, identifierKey)
	group.Replicas[0].Tip = nil
	group.Replicas = group.Replicas[:1]

	_, err := chooseNativeResumeCandidate(
		[]syncer.ProjectReplicaMetadataRef{group},
		group.SessionID,
		"claude-code",
		"replicaclaude",
		legacy,
		identifierKey,
		nil,
		nil,
	)
	if err == nil || !errors.Is(err, syncer.ErrReplicaIncomplete) {
		t.Fatalf("incomplete Replica error = %v", err)
	}
}

func nativeResumeTestGroup(t *testing.T, identifierKey []byte) (syncer.ProjectReplicaMetadataRef, []syncer.ProjectMetadataRef) {
	t.Helper()
	claude := nativeResumeReplica(t, identifierKey, "claude-code", "claude-native", "replicaclaude", "devicea", true)
	codex := nativeResumeReplica(t, identifierKey, "codex", "codex-native", "replicacodex", "deviceb", true)
	created := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	return syncer.ProjectReplicaMetadataRef{
			SessionID: "logical-session",
			SessionDescriptor: &sessionhub.SessionDescriptor{
				Version:   sessionhub.ModelVersion,
				SessionID: "logical-session",
				ProjectID: "project",
				Title:     "shared session",
				CreatedAt: created,
				CreatedBy: sessionhub.SessionCreator{Agent: "claude-code", DeviceID: "devicea"},
				Lifecycle: sessionhub.SessionActive,
			},
			Replicas: []syncer.ReplicaMetadata{claude, codex},
		}, []syncer.ProjectMetadataRef{
			{SessionID: "legacy-claude", Devices: []syncer.MetadataRef{nativeResumeLegacyMetadata(t, "claude-code", "claude-native", created)}},
			{SessionID: "legacy-codex", Devices: []syncer.MetadataRef{nativeResumeLegacyMetadata(t, "codex", "codex-native", created)}},
		}
}

func nativeResumeReplica(t *testing.T, identifierKey []byte, agent, nativeID, replicaID, deviceID string, complete bool) syncer.ReplicaMetadata {
	t.Helper()
	nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, agent, nativeID)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := syncer.NewReplicaLayout("hub", "project", "logicalsession", replicaID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 27, 1, 0, 0, 0, time.UTC)
	descriptor := sessionhub.NativeReplicaDescriptor{
		Version:   sessionhub.ModelVersion,
		ReplicaID: replicaID,
		SessionID: layout.SessionKey(),
		Source: sessionhub.NativeSource{
			Agent:            agent,
			NativeSessionKey: nativeKey,
			DeviceID:         deviceID,
			Generation:       1,
			NativeFormat:     agent + "-jsonl",
		},
		Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative, BaseHeads: []string{}},
		CreatedAt: created,
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	metadata := syncer.ReplicaMetadata{Layout: layout, Descriptor: descriptor}
	if complete {
		metadata.Tip = &sessionhub.ReplicaTip{
			Version:     sessionhub.ModelVersion,
			ReplicaID:   replicaID,
			RecordCount: 1,
			ShardCount:  1,
			LastShard:   1,
			HeadDigest:  strings.Repeat("a", 64),
			UpdatedAt:   created,
		}
	}
	return metadata
}

func nativeResumeLegacyMetadata(t *testing.T, agent, nativeID string, created time.Time) syncer.MetadataRef {
	t.Helper()
	payload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		Agent:     agent,
		NativeID:  nativeID,
		Title:     "shared session",
		CreatedAt: created,
		UpdatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	return syncer.MetadataRef{
		DeviceID: "device" + strings.TrimPrefix(agent, "claude-code"),
		Metadata: syncer.Metadata{RecordCount: 1, HeadDigest: [32]byte{1}, Payload: payload},
	}
}
