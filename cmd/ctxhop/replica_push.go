package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

// publishNativeReplica appends the adapter's canonical stream to the v2
// source-native Replica namespace. It is intentionally called after the v1
// compatibility push: a v2 failure can be retried without changing or
// rolling back the already-published v1 session.
//
// The projectIdentity argument is empty in a few legacy unit-level helpers
// that exercise only v1 storage. The command path always supplies a stable
// identity; retaining the no-op keeps those lower-level compatibility helpers
// independent of the Session Hub registry.
func publishNativeReplica(ctx context.Context, configDir, deviceID string, identifierKey []byte, projectIdentity string, layout adapter.SessionLayout, installation adapter.Installation, store remote.Remote, public *ecdh.PublicKey, stateRoot string, ref adapter.SessionRef, legacySessionID string, data adapter.SessionData, space adapter.PathSpace, identities []*ecdh.PrivateKey) error {
	if strings.TrimSpace(projectIdentity) == "" {
		return nil
	}
	if ctx == nil {
		return errors.New("session hub: context is required")
	}
	if store == nil {
		return errors.New("session hub: remote store is required")
	}
	if public == nil {
		return errors.New("session hub: recipient key is required")
	}
	if strings.TrimSpace(configDir) == "" {
		return errors.New("session hub: configuration directory is required")
	}
	if strings.TrimSpace(stateRoot) == "" {
		return errors.New("session hub: state root is required")
	}
	if err := configDeviceID(deviceID); err != nil {
		return err
	}

	stream, err := syncflow.CanonicalizeSession(data, space, installation)
	if err != nil {
		return fmt.Errorf("canonicalize native Replica: %w", err)
	}
	agent := sessionAgentLabel(ref.Agent)
	if ref.Agent == "" {
		agent = sessionAgentLabel(layout.Name())
	}
	if agent == "unknown" {
		return errors.New("session hub: native Replica has no Agent identity")
	}

	hubKey, projectKey, sessionKey, err := resolveReplicaSessionIdentity(configDir, identifierKey, projectIdentity, agent, ref.NativeID, legacySessionID)
	if err != nil {
		return err
	}
	nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, agent, ref.NativeID)
	if err != nil {
		return fmt.Errorf("derive native Replica identity: %w", err)
	}
	const generation uint64 = 1
	replicaKey, err := sessionhub.DeriveReplicaKey(identifierKey, sessionKey, agent, nativeKey, deviceID, generation)
	if err != nil {
		return fmt.Errorf("derive Replica identity: %w", err)
	}
	layoutV2, err := syncer.NewReplicaLayout(hubKey, projectKey, sessionKey, replicaKey, deviceID)
	if err != nil {
		return fmt.Errorf("prepare Replica layout: %w", err)
	}
	materializedBinding, err := loadMaterializedReplicaBinding(configDir, hubKey, projectKey, sessionKey, replicaKey, agent, ref.NativeID, generation)
	if err != nil {
		return err
	}
	if materializedBinding != nil {
		if err := syncflow.ValidateMaterializedPushPreflight(*materializedBinding, stream.Records); err != nil {
			return fmt.Errorf("validate materialized target before Replica publication: %w", err)
		}
		if err := ensureMaterializedReplicaOrigin(ctx, store, layoutV2, identities, *materializedBinding); err != nil {
			return err
		}
	}

	createdAt := ref.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	now := time.Now().UTC()
	sessionDescriptor := sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: sessionKey,
		ProjectID: projectKey,
		Title:     safeListText(ref.Title),
		CreatedAt: createdAt.UTC().Round(0),
		CreatedBy: sessionhub.SessionCreator{Agent: agent, DeviceID: deviceID},
		Lifecycle: sessionhub.SessionActive,
	}
	replicaOrigin := sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative}
	if materializedBinding != nil {
		replicaOrigin = sessionhub.ReplicaOrigin{
			Kind:      sessionhub.ReplicaOriginLocalMaterialize,
			BaseHeads: append([]string(nil), materializedBinding.Origin.BaseHeads...),
		}
	}
	replicaDescriptor := sessionhub.NativeReplicaDescriptor{
		Version:   sessionhub.ModelVersion,
		ReplicaID: replicaKey,
		SessionID: sessionKey,
		Source: sessionhub.NativeSource{
			Agent:            agent,
			NativeSessionKey: nativeKey,
			DeviceID:         deviceID,
			Generation:       generation,
			NativeFormat:     agent + "-jsonl",
			AgentVersion:     installation.Version,
		},
		Origin:    replicaOrigin,
		CreatedAt: createdAt.UTC().Round(0),
	}

	// The Hub and Project descriptors are published once per device by the
	// outer project push. Keeping them out of this per-session path avoids
	// concurrent writers replacing the same scope metadata for every session.
	if err := syncer.PutSessionDescriptor(ctx, store, public, layoutV2, sessionDescriptor); err != nil {
		return fmt.Errorf("publish logical Session descriptor: %w", err)
	}

	cursorStore, err := syncer.NewReplicaCursorStore(stateRoot, layoutV2)
	if err != nil {
		return fmt.Errorf("prepare Replica cursor: %w", err)
	}
	if _, err := syncer.PushReplicaWithCursorStore(ctx, store, public, layoutV2, replicaDescriptor, cursorStore, stream.Records, syncer.ReplicaPushOptions{
		Plan:       syncer.DefaultPlanOptions(),
		Identities: identities,
		Now:        now,
	}); err != nil {
		return fmt.Errorf("publish native Replica: %w", err)
	}
	if materializedBinding != nil {
		if err := publishMaterializedReplicaSuffix(ctx, configDir, store, public, identities, identifierKey, layoutV2, *materializedBinding); err != nil {
			return fmt.Errorf("publish materialized target suffix: %w", err)
		}
	}
	return nil
}

// publishReplicaProjectScope publishes the one-per-device scope descriptors
// before session workers start. The paths are outside any Session, so the same
// Project metadata is not duplicated for every NativeReplica.
func publishReplicaProjectScope(ctx context.Context, deviceID string, identifierKey []byte, projectIdentity string, identityKind project.IdentityKind, store remote.Remote, public *ecdh.PublicKey) error {
	hubKey, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		return fmt.Errorf("derive Session Hub identity: %w", err)
	}
	projectKey, err := sessionhub.DeriveProjectKey(identifierKey, hubKey, projectIdentity)
	if err != nil {
		return fmt.Errorf("derive Session Hub Project identity: %w", err)
	}
	projectLayout, err := syncer.NewProjectHubLayout(hubKey, projectKey)
	if err != nil {
		return fmt.Errorf("prepare Project layout: %w", err)
	}
	now := time.Now().UTC().Round(0)
	if err := syncer.PutHubDescriptorForDevice(ctx, store, public, hubKey, deviceID, sessionhub.HubDescriptor{
		Version:   sessionhub.ModelVersion,
		HubID:     hubKey,
		Name:      sessionhub.DefaultHubLogicalID,
		CreatedAt: now,
		Lifecycle: sessionhub.HubActive,
	}); err != nil {
		return fmt.Errorf("publish Hub descriptor: %w", err)
	}
	if err := syncer.PutProjectDescriptorForDevice(ctx, store, public, projectLayout, deviceID, sessionhub.ProjectDescriptor{
		Version:             sessionhub.ModelVersion,
		HubID:               hubKey,
		ProjectID:           projectKey,
		IdentityKind:        replicaProjectIdentityKind(identityKind, projectIdentity),
		IdentityFingerprint: projectKey,
		CreatedAt:           now,
		Lifecycle:           sessionhub.ProjectActive,
	}); err != nil {
		return fmt.Errorf("publish Project descriptor: %w", err)
	}
	return nil
}

func resolveReplicaSessionIdentity(configDir string, identifierKey []byte, projectIdentity, agent, nativeID, legacySessionID string) (string, string, string, error) {
	hubKey, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		return "", "", "", fmt.Errorf("derive Session Hub identity: %w", err)
	}
	projectKey, err := sessionhub.DeriveProjectKey(identifierKey, hubKey, projectIdentity)
	if err != nil {
		return "", "", "", fmt.Errorf("derive Session Hub Project identity: %w", err)
	}
	registry, err := loadSessionRegistryForRead(configDir, identifierKey, hubKey)
	if err != nil {
		return "", "", "", fmt.Errorf("load local Session Hub registry: %w", err)
	}
	if record, ok := registry.FindSessionByNative(projectKey, agent, nativeID, legacySessionID); ok {
		return hubKey, projectKey, record.Descriptor.SessionID, nil
	}
	sessionKey, err := sessionhub.DeriveNativeLogicalSessionKey(identifierKey, projectKey, agent, nativeID)
	if err != nil {
		return "", "", "", fmt.Errorf("derive logical Session identity: %w", err)
	}
	return hubKey, projectKey, sessionKey, nil
}

func replicaProjectIdentityKind(kind project.IdentityKind, value string) sessionhub.ProjectIdentityKind {
	if kind == project.KindManual || strings.HasPrefix(value, "manual:") {
		return sessionhub.ProjectIdentityManual
	}
	return sessionhub.ProjectIdentityRemote
}

func configDeviceID(deviceID string) error {
	if err := config.ValidateDeviceID(deviceID); err != nil {
		return fmt.Errorf("session hub: invalid device identity: %w", err)
	}
	return nil
}
