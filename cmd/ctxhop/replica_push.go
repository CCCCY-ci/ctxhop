package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"
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
	_, err := publishNativeReplicaInHubResult(ctx, configDir, deviceID, identifierKey, sessionhub.DefaultHubLogicalID, projectIdentity, layout, installation, store, public, stateRoot, ref, legacySessionID, data, space, identities)
	return err
}

func publishNativeReplicaInHub(ctx context.Context, configDir, deviceID string, identifierKey []byte, hubName, projectIdentity string, layout adapter.SessionLayout, installation adapter.Installation, store remote.Remote, public *ecdh.PublicKey, stateRoot string, ref adapter.SessionRef, legacySessionID string, data adapter.SessionData, space adapter.PathSpace, identities []*ecdh.PrivateKey) error {
	_, err := publishNativeReplicaInHubResult(ctx, configDir, deviceID, identifierKey, hubName, projectIdentity, layout, installation, store, public, stateRoot, ref, legacySessionID, data, space, identities)
	return err
}

// nativeReplicaPublication carries the exact v2 identity and durable cursor
// chosen by a successful Replica write. Keeping this result together prevents
// the later Contribution step from re-deriving a different logical source.
type nativeReplicaPublication struct {
	Layout          syncer.ReplicaLayout
	Descriptor      sessionhub.NativeReplicaDescriptor
	NativeSessionID string
	Stream          syncflow.CanonicalStream
	Push            syncer.ReplicaPushResult
	Binding         *sessionhub.LocalBinding
}

func publishNativeReplicaInHubResult(ctx context.Context, configDir, deviceID string, identifierKey []byte, hubName, projectIdentity string, layout adapter.SessionLayout, installation adapter.Installation, store remote.Remote, public *ecdh.PublicKey, stateRoot string, ref adapter.SessionRef, legacySessionID string, data adapter.SessionData, space adapter.PathSpace, identities []*ecdh.PrivateKey) (nativeReplicaPublication, error) {
	if strings.TrimSpace(projectIdentity) == "" {
		return nativeReplicaPublication{}, nil
	}
	if ctx == nil {
		return nativeReplicaPublication{}, errors.New("session hub: context is required")
	}
	if store == nil {
		return nativeReplicaPublication{}, errors.New("session hub: remote store is required")
	}
	if public == nil {
		return nativeReplicaPublication{}, errors.New("session hub: recipient key is required")
	}
	if strings.TrimSpace(configDir) == "" {
		return nativeReplicaPublication{}, errors.New("session hub: configuration directory is required")
	}
	if strings.TrimSpace(stateRoot) == "" {
		return nativeReplicaPublication{}, errors.New("session hub: state root is required")
	}
	if err := configDeviceID(deviceID); err != nil {
		return nativeReplicaPublication{}, err
	}

	stream, err := syncflow.CanonicalizeSession(data, space, installation)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("canonicalize native Replica: %w", err)
	}
	agent := sessionAgentLabel(ref.Agent)
	if ref.Agent == "" {
		agent = sessionAgentLabel(layout.Name())
	}
	if agent == "unknown" {
		return nativeReplicaPublication{}, errors.New("session hub: native Replica has no Agent identity")
	}

	hubKey, projectKey, sessionKey, err := resolveReplicaSessionIdentityInHub(configDir, identifierKey, hubName, projectIdentity, agent, ref.NativeID, legacySessionID)
	if err != nil {
		return nativeReplicaPublication{}, err
	}
	nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, agent, ref.NativeID)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("derive native Replica identity: %w", err)
	}
	sessionLayout, err := syncer.NewSessionHubLayout(hubKey, projectKey, sessionKey)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("prepare Session layout: %w", err)
	}
	generation, err := chooseNativeReplicaGeneration(ctx, configDir, store, sessionLayout, agent, nativeKey, ref.NativeID, deviceID, stream.Records, identities)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("choose Replica generation: %w", err)
	}
	replicaKey, err := sessionhub.DeriveReplicaKey(identifierKey, sessionKey, agent, nativeKey, deviceID, generation)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("derive Replica identity: %w", err)
	}
	layoutV2, err := syncer.NewReplicaLayout(hubKey, projectKey, sessionKey, replicaKey, deviceID)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("prepare Replica layout: %w", err)
	}
	var localBinding *sessionhub.LocalBinding
	loadedBinding, bindingErr := sessionhub.LoadLocalBinding(configDir, hubKey, projectKey, sessionKey, replicaKey, agent)
	if bindingErr == nil {
		localBinding = &loadedBinding
	} else if !errors.Is(bindingErr, sessionhub.ErrLocalBindingNotFound) {
		return nativeReplicaPublication{}, fmt.Errorf("load local Replica binding: %w", bindingErr)
	}
	if localBinding == nil {
		// A same-Agent resume performed on another device first records the
		// selected source Replica as a local restore marker. The next push owns
		// a new device Replica, so re-home that marker onto the target Replica
		// selected above instead of treating the restored prefix as a new root.
		marker, markerErr := sessionhub.FindLocalBindingByNativeSession(configDir, hubKey, projectKey, sessionKey, agent, ref.NativeID)
		if markerErr != nil && !errors.Is(markerErr, sessionhub.ErrLocalBindingNotFound) {
			return nativeReplicaPublication{}, fmt.Errorf("find same-Agent restore binding: %w", markerErr)
		}
		if markerErr == nil && marker.Origin.Kind == sessionhub.ReplicaOriginSameAgentRestore {
			marker.ReplicaID = replicaKey
			marker.Generation = generation
			localBinding = &marker
		}
	}
	materializedBinding, err := loadMaterializedReplicaBinding(configDir, hubKey, projectKey, sessionKey, replicaKey, agent, ref.NativeID, generation)
	if err != nil {
		return nativeReplicaPublication{}, err
	}
	if materializedBinding != nil {
		localBinding = materializedBinding
	}
	if materializedBinding != nil {
		if err := syncflow.ValidateMaterializedPushPreflight(*materializedBinding, stream.Records); err != nil {
			return nativeReplicaPublication{}, fmt.Errorf("validate materialized target before Replica publication: %w", err)
		}
		if err := ensureMaterializedReplicaOrigin(ctx, store, layoutV2, identities, *materializedBinding); err != nil {
			return nativeReplicaPublication{}, err
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
	if localBinding != nil && localBinding.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize {
		replicaOrigin = sessionhub.ReplicaOrigin{
			Kind:      localBinding.Origin.Kind,
			BaseHeads: append([]string(nil), localBinding.Origin.BaseHeads...),
		}
	}
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
			NativeSessionID:  ref.NativeID,
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
		return nativeReplicaPublication{}, fmt.Errorf("publish logical Session descriptor: %w", err)
	}

	cursorStore, err := syncer.NewReplicaCursorStore(stateRoot, layoutV2)
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("prepare Replica cursor: %w", err)
	}
	pushResult, err := syncer.PushReplicaWithCursorStore(ctx, store, public, layoutV2, replicaDescriptor, cursorStore, stream.Records, syncer.ReplicaPushOptions{
		Plan:       syncer.DefaultPlanOptions(),
		Identities: identities,
		Now:        now,
	})
	if err != nil {
		return nativeReplicaPublication{}, fmt.Errorf("publish native Replica: %w", err)
	}
	if materializedBinding != nil {
		if err := publishMaterializedReplicaSuffix(ctx, configDir, store, public, identities, identifierKey, layoutV2, *materializedBinding); err != nil {
			return nativeReplicaPublication{}, fmt.Errorf("publish materialized target suffix: %w", err)
		}
	}
	return nativeReplicaPublication{
		Layout:          layoutV2,
		Descriptor:      replicaDescriptor,
		NativeSessionID: ref.NativeID,
		Stream:          stream,
		Push:            pushResult,
		Binding:         localBinding,
	}, nil
}

// chooseNativeReplicaGeneration reuses the newest local-device generation
// only when its complete remote body is an exact prefix of the current native
// stream. A divergent or orphaned branch gets a fresh generation, preserving
// the immutable Replica already published under the old identity.
func chooseNativeReplicaGeneration(ctx context.Context, configDir string, store remote.Remote, sessionLayout syncer.SessionHubLayout, agent, nativeKey, nativeID, deviceID string, records [][]byte, identities []*ecdh.PrivateKey) (uint64, error) {
	if len(identities) == 0 {
		return 1, nil
	}
	metadata, err := syncer.FetchSessionReplicaMetadata(ctx, store, sessionLayout, identities)
	if errors.Is(err, syncer.ErrNoReplicaMetadata) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	type candidate struct {
		generation uint64
		metadata   syncer.ReplicaMetadata
	}
	candidates := make([]candidate, 0)
	for _, item := range metadata {
		if item.Descriptor.Source.Agent != agent || item.Descriptor.Source.NativeSessionKey != nativeKey || item.Layout.DeviceID() != deviceID {
			continue
		}
		if item.Descriptor.Source.NativeSessionID != "" && item.Descriptor.Source.NativeSessionID != nativeID {
			continue
		}
		candidates = append(candidates, candidate{generation: item.Descriptor.Source.Generation, metadata: item})
	}
	if len(candidates) == 0 {
		return 1, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].generation > candidates[j].generation
	})
	maxGeneration := candidates[0].generation
	for _, item := range candidates {
		binding, bindingErr := sessionhub.LoadLocalBinding(configDir, item.metadata.Layout.HubKey(), item.metadata.Layout.ProjectKey(), item.metadata.Layout.SessionKey(), item.metadata.Descriptor.ReplicaID, agent)
		if bindingErr != nil && !errors.Is(bindingErr, sessionhub.ErrLocalBindingNotFound) {
			return 0, fmt.Errorf("load generation %d binding: %w", item.generation, bindingErr)
		}
		if item.metadata.Tip == nil {
			// A descriptor-only bootstrap has no authenticated complete prefix.
			// Do not guess which shards belong to it; leave it immutable and
			// allocate a clean generation below.
			continue
		}
		snapshot, snapshotErr := syncer.FetchCompleteReplica(ctx, store, item.metadata.Layout, identities)
		if snapshotErr != nil {
			return 0, fmt.Errorf("verify generation %d: %w", item.generation, snapshotErr)
		}
		if replicaRecordsPrefix(snapshot.Records, records) {
			return item.generation, nil
		}
		if bindingErr == nil && binding.NativeSessionID == nativeID && binding.Generation == item.generation {
			// The local binding confirms ownership, but the remote body still
			// diverged. The caller must not append into that branch.
			continue
		}
	}
	if maxGeneration == ^uint64(0) {
		return 0, errors.New("Replica generation limit reached")
	}
	return maxGeneration + 1, nil
}

func replicaRecordsPrefix(prefix, records [][]byte) bool {
	if len(prefix) > len(records) {
		return false
	}
	for index := range prefix {
		if !bytes.Equal(prefix[index], records[index]) {
			return false
		}
	}
	return true
}

// publishNativeContribution appends the logical Contribution for a successful
// native Replica publication. It runs after the environment manifest so the
// immutable event can reference the exact filtered environment snapshot that
// was observed with the same push.
func publishNativeContribution(ctx context.Context, configDir string, identifierKey []byte, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, publication nativeReplicaPublication, deviceID string, environmentRefs []string) (*sessionhub.Contribution, error) {
	if len(identities) == 0 {
		// Low-level compatibility tests and callers that intentionally perform a
		// write-only bootstrap cannot decrypt an existing graph to prove an
		// idempotent Contribution. The command path always supplies retained
		// identities through domainAccess, so it never takes this branch.
		return nil, nil
	}
	if ctx == nil {
		return nil, errors.New("session hub: context is required")
	}
	if strings.TrimSpace(configDir) == "" {
		return nil, errors.New("session hub: configuration directory is required")
	}
	if strings.TrimSpace(deviceID) == "" {
		return nil, errors.New("session hub: device identity is required")
	}
	if publication.Descriptor.SessionID == "" || publication.Descriptor.ReplicaID == "" {
		return nil, errors.New("session hub: native Replica publication is incomplete")
	}
	sessionLayout, err := publication.Layout.SessionLayout()
	if err != nil {
		return nil, fmt.Errorf("prepare Session Contribution layout: %w", err)
	}
	existing, err := syncer.FetchSessionContributions(ctx, store, sessionLayout, identities)
	if errors.Is(err, syncer.ErrNoContributions) {
		existing = nil
	} else if err != nil {
		return nil, fmt.Errorf("read Session Contributions: %w", err)
	}

	var binding sessionhub.LocalBinding
	if publication.Binding != nil {
		binding = *publication.Binding
	}
	if binding.Version == 0 {
		binding = sessionhub.LocalBinding{
			Version:         sessionhub.ModelVersion,
			HubID:           publication.Layout.HubKey(),
			ProjectID:       publication.Layout.ProjectKey(),
			SessionID:       publication.Layout.SessionKey(),
			Agent:           publication.Descriptor.Source.Agent,
			NativeSessionID: publication.NativeSessionID,
			ReplicaID:       publication.Descriptor.ReplicaID,
			Generation:      publication.Descriptor.Source.Generation,
			Origin: sessionhub.BindingOrigin{
				Kind:      bindingOriginKind(publication.Descriptor.Origin.Kind),
				BaseHeads: append([]string(nil), publication.Descriptor.Origin.BaseHeads...),
			},
		}
	}
	if binding.NativeSessionID == "" {
		return nil, errors.New("session hub: native Contribution binding has no local NativeSession identity")
	}
	if binding.HubID == "" {
		binding.HubID = publication.Layout.HubKey()
	}
	if binding.ProjectID == "" {
		binding.ProjectID = publication.Layout.ProjectKey()
	}
	if binding.SessionID == "" {
		binding.SessionID = publication.Layout.SessionKey()
	}
	binding.ReplicaID = publication.Descriptor.ReplicaID
	binding.Generation = publication.Descriptor.Source.Generation
	binding.ReplicaCursor = sessionhub.ReplicaCursor{
		NextShard:   publication.Push.Cursor.NextShard,
		RecordCount: publication.Push.Cursor.RecordCount,
		HeadDigest:  fmt.Sprintf("%x", publication.Push.Cursor.HeadDigest),
	}

	plan, err := syncflow.PlanNativeContribution(syncflow.NativeContributionRequest{
		Binding:               binding,
		DeviceID:              deviceID,
		Records:               publication.Stream.Records,
		ExistingContributions: existing,
		IdentifierKey:         identifierKey,
		EnvironmentRefs:       environmentRefs,
		Cursor:                binding.ReplicaCursor,
	})
	if err != nil {
		return nil, err
	}
	var result *sessionhub.Contribution
	if plan.Contribution != nil {
		if err := syncer.PutContributionWithIdentities(ctx, store, public, sessionLayout, *plan.Contribution, identities); err != nil {
			return nil, fmt.Errorf("publish native Contribution: %w", err)
		}
		copyValue := *plan.Contribution
		result = &copyValue
	} else if plan.Binding.ContributionCursor.LastContributionID != "" {
		for index := range existing {
			if existing[index].ContributionID == plan.Binding.ContributionCursor.LastContributionID {
				copyValue := existing[index]
				result = &copyValue
				break
			}
		}
	}
	if err := sessionhub.SaveLocalBinding(configDir, plan.Binding); err != nil {
		return nil, fmt.Errorf("commit native Contribution cursor: %w", err)
	}
	return result, nil
}

func bindingOriginKind(kind sessionhub.ReplicaOriginKind) sessionhub.ReplicaOriginKind {
	switch kind {
	case sessionhub.ReplicaOriginSameAgentRestore:
		return sessionhub.ReplicaOriginSameAgentRestore
	default:
		return sessionhub.ReplicaOriginNative
	}
}

// publishReplicaProjectScope publishes the one-per-device scope descriptors
// before session workers start. The paths are outside any Session, so the same
// Project metadata is not duplicated for every NativeReplica.
func publishReplicaProjectScope(ctx context.Context, deviceID string, identifierKey []byte, projectIdentity string, identityKind project.IdentityKind, store remote.Remote, public *ecdh.PublicKey) error {
	return publishReplicaProjectScopeInHub(ctx, deviceID, identifierKey, sessionhub.DefaultHubLogicalID, projectIdentity, identityKind, store, public)
}

func publishReplicaProjectScopeInHub(ctx context.Context, deviceID string, identifierKey []byte, hubName, projectIdentity string, identityKind project.IdentityKind, store remote.Remote, public *ecdh.PublicKey) error {
	hubKey, err := sessionhub.DeriveHubKey(identifierKey, hubName)
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
		Name:      hubName,
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
	return resolveReplicaSessionIdentityInHub(configDir, identifierKey, sessionhub.DefaultHubLogicalID, projectIdentity, agent, nativeID, legacySessionID)
}

func resolveReplicaSessionIdentityInHub(configDir string, identifierKey []byte, hubName, projectIdentity, agent, nativeID, legacySessionID string) (string, string, string, error) {
	hubKey, err := sessionhub.DeriveHubKey(identifierKey, hubName)
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
