package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// legacyMigrationPublishResult describes the side effects of one explicit
// legacy branch publication. ReplicaID is stable even when the operation
// stops after a descriptor or an intermediate shard; the local cursor and
// migration ledger make that state retryable.
type legacyMigrationPublishResult struct {
	ReplicaID       string
	WritesV2        bool
	Complete        bool
	PublishedShards int
}

// selectLegacyMigrationPublishSource chooses the branch that this device is
// allowed to re-publish. A v1 device branch is owned by the device that wrote
// it; migration must not impersonate another device merely because the
// encrypted branch is readable.
func selectLegacyMigrationPublishSource(candidate legacyMigrationCandidate, localDeviceID string) (legacyMigrationSource, error) {
	if err := configDeviceID(localDeviceID); err != nil {
		return legacyMigrationSource{}, fmt.Errorf("session migrate: local writer identity is invalid: %w", err)
	}
	var selected *legacyMigrationSource
	for index := range candidate.sources {
		source := &candidate.sources[index]
		if source.deviceID != localDeviceID {
			continue
		}
		if selected != nil {
			return legacyMigrationSource{}, errors.New("session migrate: legacy source is ambiguous for the local device")
		}
		selected = source
	}
	if selected == nil {
		return legacyMigrationSource{}, errors.New("session migrate: selected legacy branch is owned by another device; run publish on its source device")
	}
	if !selected.known {
		return legacyMigrationSource{}, errors.New("session migrate: selected legacy branch has no usable Agent source")
	}
	if strings.TrimSpace(selected.nativeID) == "" {
		return legacyMigrationSource{}, errors.New("session migrate: selected legacy branch has no usable native identity")
	}
	return *selected, nil
}

// selectCompleteLegacyBranch returns the current, authenticated v1 branch
// for the local writer. It deliberately passes a one-device allowlist to the
// compatibility reader, so a full publish of one branch does not download
// unrelated branches from the same logical Session.
func selectCompleteLegacyBranch(ctx context.Context, access *domainAccess, collection listCollection, candidate legacyMigrationCandidate, source legacyMigrationSource) (syncer.LegacyReplica, legacyMigrationSource, error) {
	if ctx == nil {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: context is required")
	}
	if access == nil || access.Store == nil || access.Public == nil {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: authenticated domain access is unavailable")
	}
	if source.deviceID != collection.localDeviceID {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: legacy source is not owned by the local device")
	}
	if active := access.allowedDevices(); active != nil {
		if _, ok := active[source.deviceID]; !ok {
			return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: local device is not active in the sync domain")
		}
	}

	replicas, err := syncer.FetchCompleteLegacyReplicasWithIdentitiesAndDevices(
		ctx,
		access.Store,
		collection.projectID,
		candidate.legacyID,
		access.Identities,
		map[string]struct{}{source.deviceID: {}},
	)
	if err != nil {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, fmt.Errorf("session migrate: read complete legacy branch: %w", err)
	}
	if len(replicas) != 1 || replicas[0].DeviceID != source.deviceID {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: selected legacy branch is not uniquely readable")
	}
	legacy := replicas[0]
	ref, ok := legacyMigrationRefForDevice(candidate.refs, source.deviceID)
	if !ok || ref.RecordCount != legacy.Metadata.RecordCount || ref.BranchHeadDigest != legacyMigrationDigest(legacy.Metadata.HeadDigest) {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: legacy branch changed during migration planning; retry discovery")
	}

	// Metadata can change between the metadata-only discovery and this body
	// read. Re-derive the transient source from the authenticated live payload
	// and refuse to publish under a stale native identity.
	liveSource, warning := legacyMigrationSourceFromMetadata(collection.identifierKey, candidate.legacyID, syncer.MetadataRef{
		DeviceID: source.deviceID,
		Metadata: legacy.Metadata,
	})
	if warning != nil || !liveSource.known {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: live legacy metadata has no usable Agent source")
	}
	if liveSource.agent != source.agent || liveSource.nativeID != source.nativeID {
		return syncer.LegacyReplica{}, legacyMigrationSource{}, errors.New("session migrate: legacy Agent source changed during migration planning; retry discovery")
	}
	return legacy, liveSource, nil
}

// selectLegacyMigrationReader opens the current, authenticated v1 branch for
// the local writer without assembling its records. Metadata and shard
// structure are checked before any v2 object is written; shard bodies are
// consumed later by publishLegacyMigrationReplicaStream.
func selectLegacyMigrationReader(ctx context.Context, access *domainAccess, collection listCollection, candidate legacyMigrationCandidate, source legacyMigrationSource) (*syncer.LegacyReplicaReader, syncer.Metadata, legacyMigrationSource, error) {
	if ctx == nil {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: context is required")
	}
	if access == nil || access.Store == nil || access.Public == nil {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: authenticated domain access is unavailable")
	}
	if source.deviceID != collection.localDeviceID {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: legacy source is not owned by the local device")
	}
	if active := access.allowedDevices(); active != nil {
		if _, ok := active[source.deviceID]; !ok {
			return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: local device is not active in the sync domain")
		}
	}

	reader, err := syncer.OpenLegacyReplicaReader(ctx, access.Store, collection.projectID, candidate.legacyID, source.deviceID, access.Identities)
	if err != nil {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, fmt.Errorf("session migrate: open legacy branch reader: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = reader.Close()
		}
	}()
	metadata := reader.Metadata()
	ref, ok := legacyMigrationRefForDevice(candidate.refs, source.deviceID)
	if !ok || ref.RecordCount != metadata.RecordCount || ref.BranchHeadDigest != legacyMigrationDigest(metadata.HeadDigest) {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: legacy branch changed during migration planning; retry discovery")
	}

	// Metadata can change between metadata-only discovery and this reader open.
	// Re-derive the transient source from the authenticated live payload and
	// refuse to publish under a stale native identity.
	liveSource, warning := legacyMigrationSourceFromMetadata(collection.identifierKey, candidate.legacyID, syncer.MetadataRef{
		DeviceID: source.deviceID,
		Metadata: metadata,
	})
	if warning != nil || !liveSource.known {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: live legacy metadata has no usable Agent source")
	}
	if liveSource.agent != source.agent || liveSource.nativeID != source.nativeID {
		return nil, syncer.Metadata{}, legacyMigrationSource{}, errors.New("session migrate: legacy Agent source changed during migration planning; retry discovery")
	}
	closeOnError = false
	return reader, metadata, liveSource, nil
}

// publishLegacyMigrationReplica creates a new v2 source-native Replica from
// one materialized v1 branch. It remains as a compatibility wrapper for
// callers/tests that already have a complete LegacyReplica; the production
// migration path uses the reader-based function below.
func publishLegacyMigrationReplica(ctx context.Context, configDir string, access *domainAccess, collection listCollection, hubScope sessionHubScope, projectScope sessionProjectScope, candidate legacyMigrationCandidate, source legacyMigrationSource, legacy syncer.LegacyReplica) (legacyMigrationPublishResult, error) {
	result := legacyMigrationPublishResult{}
	if legacy.DeviceID != source.deviceID || legacy.LegacySessionID != candidate.legacyID {
		return result, errors.New("session migrate: legacy branch ownership does not match the local writer")
	}
	if len(legacy.Branch.Records) == 0 {
		return result, errors.New("session migrate: legacy branch is empty")
	}
	if err := legacy.Metadata.Validate(); err != nil {
		return result, fmt.Errorf("session migrate: legacy metadata is invalid: %w", err)
	}
	ref, ok := legacyMigrationRefForDevice(candidate.refs, source.deviceID)
	if !ok || ref.RecordCount != legacy.Metadata.RecordCount || ref.BranchHeadDigest != legacyMigrationDigest(legacy.Metadata.HeadDigest) {
		return result, errors.New("session migrate: legacy branch does not match the migration plan")
	}
	digest, err := syncer.DigestRecords(legacy.Branch.Records)
	if err != nil {
		return result, fmt.Errorf("session migrate: validate legacy branch records: %w", err)
	}
	if uint64(len(legacy.Branch.Records)) != legacy.Metadata.RecordCount || digest != legacy.Metadata.HeadDigest || digest != legacy.Branch.HeadDigest {
		return result, errors.New("session migrate: legacy branch body does not match its authenticated metadata")
	}
	reader, err := syncer.NewSliceRecordReader(legacy.Branch.Records)
	if err != nil {
		return result, fmt.Errorf("session migrate: prepare legacy branch reader: %w", err)
	}
	return publishLegacyMigrationReplicaStream(ctx, configDir, access, collection, hubScope, projectScope, candidate, source, legacy.Metadata, reader)
}

// publishLegacyMigrationReplicaStream creates a new v2 source-native Replica
// from one canonical v1 record reader. It never calls a v1 write API. The
// source reader and publisher each retain only one shard at a time, while the
// authenticated metadata count/digest prevents a stale or truncated listing
// from being promoted into a complete v2 Replica.
func publishLegacyMigrationReplicaStream(ctx context.Context, configDir string, access *domainAccess, collection listCollection, hubScope sessionHubScope, projectScope sessionProjectScope, candidate legacyMigrationCandidate, source legacyMigrationSource, metadata syncer.Metadata, reader syncer.RecordReader) (result legacyMigrationPublishResult, err error) {
	if reader == nil {
		return result, errors.New("session migrate: legacy branch reader is required")
	}
	handedOff := false
	defer func() {
		if handedOff {
			return
		}
		if closeErr := reader.Close(); closeErr != nil {
			if err == nil {
				err = fmt.Errorf("session migrate: close legacy branch reader: %w", closeErr)
			} else {
				err = errors.Join(err, fmt.Errorf("session migrate: close legacy branch reader: %w", closeErr))
			}
		}
	}()

	if ctx == nil {
		return result, errors.New("session migrate: context is required")
	}
	if err := ctx.Err(); err != nil {
		return result, fmt.Errorf("session migrate: %w", err)
	}
	if strings.TrimSpace(configDir) == "" {
		return result, errors.New("session migrate: configuration directory is required")
	}
	if access == nil || access.Store == nil || access.Public == nil {
		return result, errors.New("session migrate: authenticated domain access is unavailable")
	}
	if !source.known || strings.TrimSpace(source.agent) == "" || strings.TrimSpace(source.nativeID) == "" {
		return result, errors.New("session migrate: legacy source is not publishable")
	}
	if source.deviceID != collection.localDeviceID {
		return result, errors.New("session migrate: legacy branch ownership does not match the local writer")
	}
	if metadata.RecordCount == 0 {
		return result, errors.New("session migrate: legacy branch is empty")
	}
	if err := configDeviceID(collection.localDeviceID); err != nil {
		return result, err
	}
	if err := metadata.Validate(); err != nil {
		return result, fmt.Errorf("session migrate: legacy metadata is invalid: %w", err)
	}
	ref, ok := legacyMigrationRefForDevice(candidate.refs, source.deviceID)
	if !ok || ref.RecordCount != metadata.RecordCount || ref.BranchHeadDigest != legacyMigrationDigest(metadata.HeadDigest) {
		return result, errors.New("session migrate: legacy branch does not match the migration plan")
	}

	nativeKey, err := sessionhub.DeriveNativeSessionKey(collection.identifierKey, source.agent, source.nativeID)
	if err != nil {
		return result, fmt.Errorf("session migrate: derive legacy NativeSession identity: %w", err)
	}
	const generation uint64 = 1
	replicaID, err := sessionhub.DeriveReplicaKey(collection.identifierKey, candidate.sessionID, source.agent, nativeKey, collection.localDeviceID, generation)
	if err != nil {
		return result, fmt.Errorf("session migrate: derive v2 Replica identity: %w", err)
	}
	result.ReplicaID = replicaID

	layout, err := syncer.NewReplicaLayout(hubScope.ID, projectScope.ID, candidate.sessionID, replicaID, collection.localDeviceID)
	if err != nil {
		return result, fmt.Errorf("session migrate: prepare v2 Replica layout: %w", err)
	}

	// Scope descriptors are device-owned and may be created if this project has
	// never had a v2 writer. Existing scope metadata is reused rather than
	// rewritten, and none of these operations touch the v1 namespace.
	if wrote, err := ensureLegacyMigrationScope(ctx, access, layout, hubScope.Name, collection); err != nil {
		return result, fmt.Errorf("session migrate: publish v2 scope descriptors: %w", err)
	} else if wrote {
		result.WritesV2 = true
	}

	createdAt := candidate.createdAt
	if createdAt.IsZero() {
		createdAt = legacyUnknownTime
	}
	sessionDescriptor := sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: candidate.sessionID,
		ProjectID: projectScope.ID,
		Title:     safeListText(candidate.title),
		CreatedAt: createdAt.UTC().Round(0),
		CreatedBy: sessionhub.SessionCreator{Agent: source.agent, DeviceID: collection.localDeviceID},
		Lifecycle: sessionhub.SessionActive,
	}
	sessionLayout, err := syncer.NewSessionHubLayout(hubScope.ID, projectScope.ID, candidate.sessionID)
	if err != nil {
		return result, fmt.Errorf("session migrate: prepare logical Session layout: %w", err)
	}
	if wrote, err := ensureLegacySessionDescriptor(ctx, access, sessionLayout, collection.localDeviceID, sessionDescriptor); err != nil {
		return result, fmt.Errorf("session migrate: publish logical Session descriptor: %w", err)
	} else if wrote {
		result.WritesV2 = true
	}

	// A migrated v1 branch remains source-native content. The local migration
	// ledger carries the legacy provenance; using the existing native origin
	// keeps v1-compatible v2 clients able to read the Replica without a schema
	// extension they would reject.
	replicaDescriptor := sessionhub.NativeReplicaDescriptor{
		Version:   sessionhub.ModelVersion,
		ReplicaID: replicaID,
		SessionID: candidate.sessionID,
		Source: sessionhub.NativeSource{
			Agent:            source.agent,
			NativeSessionKey: nativeKey,
			NativeSessionID:  source.nativeID,
			DeviceID:         collection.localDeviceID,
			Generation:       generation,
			NativeFormat:     "legacy-v1",
		},
		Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative},
		CreatedAt: createdAt.UTC().Round(0),
	}
	cursorStore, err := syncer.NewReplicaCursorStore(configDir, layout)
	if err != nil {
		return result, fmt.Errorf("session migrate: prepare v2 Replica cursor: %w", err)
	}
	pushResult, pushErr := syncer.PushReplicaStreamWithCursorStore(ctx, access.Store, access.Public, layout, replicaDescriptor, cursorStore, reader, syncer.ReplicaStreamOptions{
		ReplicaPushOptions: syncer.ReplicaPushOptions{
			Plan:       syncer.DefaultPlanOptions(),
			Identities: access.Identities,
			Now:        time.Now().UTC(),
		},
		VerifyExpected:      true,
		ExpectedRecordCount: metadata.RecordCount,
		ExpectedHeadDigest:  metadata.HeadDigest,
	})
	handedOff = true
	result.PublishedShards = pushResult.PublishedShards
	if pushErr != nil {
		result.WritesV2 = true
		return result, fmt.Errorf("session migrate: publish v2 Replica: %w", pushErr)
	}
	result.WritesV2 = true
	result.Complete = true
	return result, nil
}

// ensureLegacyMigrationScope creates missing device-owned Hub and Project
// descriptors while preserving any metadata already published by this local
// device. This avoids making a migration retry look like a project metadata
// update merely because it happened at a later wall-clock time.
func ensureLegacyMigrationScope(ctx context.Context, access *domainAccess, layout syncer.ReplicaLayout, hubName string, collection listCollection) (bool, error) {
	if access == nil || access.Store == nil || access.Public == nil {
		return false, errors.New("authenticated domain access is unavailable")
	}
	now := time.Now().UTC().Round(0)
	hubName = strings.TrimSpace(hubName)
	if hubName == "" {
		hubName = sessionhub.DefaultHubLogicalID
	}
	hub := sessionhub.HubDescriptor{
		Version:   sessionhub.ModelVersion,
		HubID:     layout.HubKey(),
		Name:      hubName,
		CreatedAt: now,
		Lifecycle: sessionhub.HubActive,
	}
	project := sessionhub.ProjectDescriptor{
		Version:             sessionhub.ModelVersion,
		HubID:               layout.HubKey(),
		ProjectID:           layout.ProjectKey(),
		IdentityKind:        replicaProjectIdentityKind(collection.current.Identity.Kind, collection.current.Identity.Value),
		IdentityFingerprint: layout.ProjectKey(),
		CreatedAt:           now,
		Lifecycle:           sessionhub.ProjectActive,
	}
	wrote := false
	if _, err := syncer.FetchHubDescriptor(ctx, access.Store, layout, access.Identities); err != nil {
		if !errors.Is(err, remote.ErrNotFound) {
			return false, err
		}
		if err := syncer.PutHubDescriptor(ctx, access.Store, access.Public, layout, hub); err != nil {
			return false, err
		}
		wrote = true
	}
	if _, err := syncer.FetchProjectDescriptor(ctx, access.Store, layout, access.Identities); err != nil {
		if !errors.Is(err, remote.ErrNotFound) {
			return false, err
		}
		if err := syncer.PutProjectDescriptor(ctx, access.Store, access.Public, layout, project); err != nil {
			return false, err
		}
		wrote = true
	}
	return wrote, nil
}

// ensureLegacySessionDescriptor is intentionally create-if-absent. A local
// device may already have a logical descriptor created by another v2 path;
// migration must not overwrite that user-visible metadata just to publish a
// legacy body.
func ensureLegacySessionDescriptor(ctx context.Context, access *domainAccess, layout syncer.SessionHubLayout, deviceID string, descriptor sessionhub.SessionDescriptor) (bool, error) {
	if access == nil || access.Store == nil || access.Public == nil {
		return false, errors.New("authenticated domain access is unavailable")
	}
	existing, err := syncer.FetchSessionDescriptorForDevice(ctx, access.Store, layout, deviceID, access.Identities)
	if err == nil {
		if existing.SessionID != descriptor.SessionID || existing.ProjectID != descriptor.ProjectID {
			return false, errors.New("existing logical Session descriptor conflicts with the selected project")
		}
		return false, nil
	}
	if !errors.Is(err, remote.ErrNotFound) {
		return false, err
	}
	if err := syncer.PutSessionDescriptorForDevice(ctx, access.Store, access.Public, layout, deviceID, descriptor); err != nil {
		return false, err
	}
	return true, nil
}

// recordLegacyMigrationPublishProgress merges the stable Replica identity
// into the local metadata-only ledger. The timestamp is advanced only when a
// new Replica or a higher status is observed, keeping an idempotent retry
// byte-for-byte stable once it is already complete.
func recordLegacyMigrationPublishProgress(configDir string, hubScope sessionHubScope, projectScope sessionProjectScope, candidate legacyMigrationCandidate, ledgers map[string]sessionhub.MigrationLedger, replicaID string, status sessionhub.MigrationStatus) (map[string]sessionhub.MigrationLedger, bool, error) {
	if strings.TrimSpace(configDir) == "" {
		return ledgers, false, errors.New("session migrate: configuration directory is required")
	}
	if strings.TrimSpace(replicaID) == "" {
		return ledgers, false, errors.New("session migrate: published Replica identity is empty")
	}
	if status != sessionhub.MigrationStatusPartial && status != sessionhub.MigrationStatusPublished {
		return ledgers, false, errors.New("session migrate: invalid published migration status")
	}
	updated := cloneMigrationLedgerMap(ledgers)
	current, ok := updated[candidate.legacyID]
	if !ok {
		current = sessionhub.MigrationLedger{
			Version:         sessionhub.MigrationLedgerVersion,
			HubID:           hubScope.ID,
			ProjectID:       projectScope.ID,
			LegacySessionID: candidate.legacyID,
			SessionID:       candidate.sessionID,
			LegacyRefs:      append([]sessionhub.LegacyMigrationRef(nil), candidate.refs...),
			Status:          sessionhub.MigrationStatusLazy,
			ReadMode:        sessionhub.MigrationReadModeV2,
			UpdatedAt:       legacyUnknownTime,
		}
	}
	if current.HubID != hubScope.ID || current.ProjectID != projectScope.ID || current.LegacySessionID != candidate.legacyID || current.SessionID != candidate.sessionID {
		return ledgers, false, errors.New("session migrate: local migration ledger conflicts with the selected Session")
	}
	if current.Status == sessionhub.MigrationStatusBlocked {
		return ledgers, false, errors.New("session migrate: local migration ledger is blocked; read-only discovery is required")
	}
	before, err := current.MarshalBinary()
	if err != nil {
		return ledgers, false, fmt.Errorf("session migrate: encode publish progress: %w", err)
	}
	replicaAdded := true
	for _, existing := range current.PublishedReplicas {
		if existing == replicaID {
			replicaAdded = false
			break
		}
	}
	current.PublishedReplicas = append(current.PublishedReplicas, replicaID)
	statusChanged := false
	if migrationStatusRankForPublish(status) > migrationStatusRankForPublish(current.Status) {
		current.Status = status
		statusChanged = true
	}
	if replicaAdded || statusChanged {
		current.UpdatedAt = time.Now().UTC().Round(0)
	}
	after, err := current.MarshalBinary()
	if err != nil {
		return ledgers, false, fmt.Errorf("session migrate: encode publish progress: %w", err)
	}
	changed := !bytes.Equal(before, after)
	if !changed {
		updated[candidate.legacyID] = current
		return updated, false, nil
	}
	if err := sessionhub.SaveMigrationLedger(configDir, current); err != nil {
		return ledgers, false, fmt.Errorf("session migrate: save publish progress: %w", err)
	}
	effective, err := sessionhub.LoadMigrationLedger(configDir, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		return ledgers, false, fmt.Errorf("session migrate: verify publish progress: %w", err)
	}
	updated[candidate.legacyID] = effective
	return updated, true, nil
}

func migrationStatusRankForPublish(status sessionhub.MigrationStatus) int {
	switch status {
	case sessionhub.MigrationStatusLazy:
		return 1
	case sessionhub.MigrationStatusPartial:
		return 2
	case sessionhub.MigrationStatusPublished:
		return 3
	default:
		return 0
	}
}

func legacyMigrationRefForDevice(refs []sessionhub.LegacyMigrationRef, deviceID string) (sessionhub.LegacyMigrationRef, bool) {
	for _, ref := range refs {
		if ref.DeviceID == deviceID {
			return ref, true
		}
	}
	return sessionhub.LegacyMigrationRef{}, false
}

func legacyMigrationDigest(digest [32]byte) string {
	return fmt.Sprintf("sha256:%x", digest[:])
}

func loadLegacyMigrationReadMode(configDir, hubID, projectID, legacySessionID string) (sessionhub.MigrationReadMode, error) {
	ledger, err := sessionhub.LoadMigrationLedger(configDir, hubID, projectID, legacySessionID)
	if errors.Is(err, sessionhub.ErrMigrationLedgerNotFound) {
		return sessionhub.MigrationReadModeV2, nil
	}
	if err != nil {
		return "", fmt.Errorf("resume: read migration preference: %w", err)
	}
	if ledger.ReadMode == "" {
		return sessionhub.MigrationReadModeV2, nil
	}
	return ledger.ReadMode, nil
}

// setLegacyMigrationReadMode updates the local preference without changing
// migration progress. Keeping the preference in the ledger avoids a second
// rollback sidecar and makes the policy travel with the existing local
// migration record. The Remote remains untouched.
func setLegacyMigrationReadMode(configDir string, hubScope sessionHubScope, projectScope sessionProjectScope, candidate legacyMigrationCandidate, ledgers map[string]sessionhub.MigrationLedger, mode sessionhub.MigrationReadMode) (map[string]sessionhub.MigrationLedger, bool, error) {
	if strings.TrimSpace(configDir) == "" {
		return ledgers, false, errors.New("session migrate: configuration directory is required")
	}
	if mode != sessionhub.MigrationReadModeV2 && mode != sessionhub.MigrationReadModeLegacy {
		return ledgers, false, errors.New("session migrate: invalid migration read mode")
	}
	updated := cloneMigrationLedgerMap(ledgers)
	current, ok := updated[candidate.legacyID]
	if !ok {
		current = sessionhub.MigrationLedger{
			Version:         sessionhub.MigrationLedgerVersion,
			HubID:           hubScope.ID,
			ProjectID:       projectScope.ID,
			LegacySessionID: candidate.legacyID,
			SessionID:       candidate.sessionID,
			LegacyRefs:      append([]sessionhub.LegacyMigrationRef(nil), candidate.refs...),
			Status:          sessionhub.MigrationStatusLazy,
			ReadMode:        mode,
			UpdatedAt:       legacyUnknownTime,
		}
	} else if current.HubID != hubScope.ID || current.ProjectID != projectScope.ID || current.LegacySessionID != candidate.legacyID || current.SessionID != candidate.sessionID {
		return ledgers, false, errors.New("session migrate: local migration ledger conflicts with the selected Session")
	}
	if current.ReadMode == "" {
		current.ReadMode = sessionhub.MigrationReadModeV2
	}
	if ok && current.ReadMode == mode {
		updated[candidate.legacyID] = current
		return updated, false, nil
	}
	current.ReadMode = mode
	now := time.Now().UTC().Round(0)
	if !now.After(current.UpdatedAt) {
		now = current.UpdatedAt.Add(time.Nanosecond)
	}
	current.UpdatedAt = now
	if err := sessionhub.SaveMigrationLedger(configDir, current); err != nil {
		return ledgers, false, fmt.Errorf("session migrate: save migration read mode: %w", err)
	}
	effective, err := sessionhub.LoadMigrationLedger(configDir, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		return ledgers, false, fmt.Errorf("session migrate: verify migration read mode: %w", err)
	}
	updated[candidate.legacyID] = effective
	return updated, true, nil
}
