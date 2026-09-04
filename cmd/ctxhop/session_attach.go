package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

type sessionAttachReport struct {
	Action     string              `json:"action"`
	Scope      string              `json:"scope"`
	Hub        sessionHubScope     `json:"hub"`
	Project    sessionProjectScope `json:"project"`
	SessionID  string              `json:"sessionId"`
	Agent      string              `json:"agent"`
	NativeID   string              `json:"nativeId"`
	DeviceID   string              `json:"deviceId"`
	ReplicaID  string              `json:"replicaId"`
	Generation uint64              `json:"generation"`
	Parent     string              `json:"parent,omitempty"`
	State      string              `json:"state"`
}

type sessionReconcileReport struct {
	Action        string              `json:"action"`
	Scope         string              `json:"scope"`
	Hub           sessionHubScope     `json:"hub"`
	Project       sessionProjectScope `json:"project"`
	SessionID     string              `json:"sessionId,omitempty"`
	Agent         string              `json:"agent"`
	NativeID      string              `json:"nativeId"`
	DeviceID      string              `json:"deviceId"`
	ReplicaID     string              `json:"replicaId,omitempty"`
	Generation    uint64              `json:"generation,omitempty"`
	State         string              `json:"state"`
	Binding       string              `json:"binding"`
	LocalRecords  uint64              `json:"localRecordCount"`
	RemoteRecords uint64              `json:"remoteRecordCount,omitempty"`
	CommonPrefix  uint64              `json:"commonPrefix,omitempty"`
	RemoteHead    string              `json:"remoteHead,omitempty"`
	Explanation   string              `json:"explanation,omitempty"`
}

func collectSessionAttach(ctx context.Context, c *config.Config, configDir, projectDir string, options sessionOptions, input io.Reader, prompt io.Writer) (sessionAttachReport, error) {
	if c == nil {
		return sessionAttachReport{}, errors.New("session attach: configuration is unavailable")
	}
	if err := devicePullError("session attach", c); err != nil {
		return sessionAttachReport{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: local device identity is invalid: %w", err)
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		return sessionAttachReport{}, errors.New("session attach: current directory has no stable project identity")
	}
	if mode := projectPullMode(c, current.Identity.Value); mode == projectModeExcluded || mode == projectModePushOnly {
		return sessionAttachReport{}, errors.New("session attach: current project does not allow remote Session Hub reads")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: load local sync material: %w", err)
	}
	hubScope, projectScope, projectID, err := sessionHubAndProjectForConfig(secrets.IdentifierKey, current, c)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: prepare Session Hub identity: %w", err)
	}

	installed, err := adapter.FindInstalled(ctx, options.agent)
	if err != nil {
		if errors.Is(err, adapter.ErrNotInstalled) {
			return sessionAttachReport{}, fmt.Errorf("session attach: Agent %s is not installed", resumeAgentLabel(options.agent))
		}
		return sessionAttachReport{}, fmt.Errorf("session attach: inspect Agent %s: %w", resumeAgentLabel(options.agent), err)
	}
	refs, err := installed.Layout.DiscoverSessions(current.Root)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: discover local %s sessions: %w", resumeAgentLabel(options.agent), err)
	}
	localRef, ok := findNativeSessionRef(refs, options.nativeID)
	if !ok {
		return sessionAttachReport{}, fmt.Errorf("session attach: native session %q was not found for the current project", safeListText(options.nativeID))
	}
	if localRef.Agent == "" {
		localRef.Agent = options.agent
	}
	localRef.ProjectPath = current.Root
	data, err := installed.Layout.ReadSession(localRef)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: read local native session: %w", err)
	}
	if data.DroppedTail || data.Skipped != 0 || len(data.Records) == 0 {
		return sessionAttachReport{}, errors.New("session attach: local native session is incomplete and cannot be bound safely")
	}
	stream, err := syncflow.CanonicalizeSession(data, adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installed.Installation.DataDir}, installed.Installation)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: canonicalize local native session: %w", err)
	}

	mutationLock, err := acquireLocalMutationLock(ctx, configDir, "session attach")
	if err != nil {
		return sessionAttachReport{}, err
	}
	defer mutationLock.Close() //nolint:errcheck // operation result is already determined

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "session attach")
	if err != nil {
		return sessionAttachReport{}, err
	}
	defer access.close()
	group, err := fetchSessionHubGroup(ctx, access, hubScope, projectID, options.sessionID)
	if err != nil {
		return sessionAttachReport{}, err
	}

	sessionDescriptor, err := attachSessionDescriptor(group, projectID, options.sessionID, options.agent, c.Device.ID, localRef)
	if err != nil {
		return sessionAttachReport{}, err
	}
	nativeKey, err := sessionhub.DeriveNativeSessionKey(secrets.IdentifierKey, options.agent, options.nativeID)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: derive native identity: %w", err)
	}
	if err := validateAttachParent(ctx, access, group, options.parent, options.asRoot); err != nil {
		return sessionAttachReport{}, err
	}

	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: load local Session Hub registry: %w", err)
	}
	if _, ok := registry.HubByName(hubScope.Name); !ok {
		if _, err := registry.EnsureHub(secrets.IdentifierKey, hubScope.Name, time.Now().UTC()); err != nil {
			return sessionAttachReport{}, fmt.Errorf("session attach: create local Hub scope: %w", err)
		}
	}
	identityKind := sessionhub.ProjectIdentityRemote
	if current.Identity.Kind == project.KindManual {
		identityKind = sessionhub.ProjectIdentityManual
	}
	projectRecord, err := registry.EnsureProjectInHub(secrets.IdentifierKey, hubScope.Name, identityKind, current.Identity.Value, time.Now().UTC())
	if err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: register local Project: %w", err)
	}
	if projectRecord.Descriptor.ProjectID != projectID {
		return sessionAttachReport{}, errors.New("session attach: local Project identity does not match the selected Hub")
	}
	if existing, exists := registry.FindSessionByNative(projectID, options.agent, options.nativeID, ""); exists && existing.Descriptor.SessionID != options.sessionID {
		return sessionAttachReport{}, fmt.Errorf("session attach: native session is already bound to logical Session %q", existing.Descriptor.SessionID)
	}
	if _, err := registry.EnsureSession(projectID, sessionDescriptor); err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: register logical Session: %w", err)
	}

	selected, err := selectAttachReplica(group, options.agent, nativeKey, c.Device.ID)
	if err != nil {
		return sessionAttachReport{}, err
	}
	var binding sessionhub.LocalBinding
	state := "attached"
	if selected != nil {
		binding, state, err = bindingForExistingReplica(ctx, configDir, access, current, installed, localRef, stream, group, *selected, options)
		if err != nil {
			return sessionAttachReport{}, err
		}
	} else {
		generation := nextLocalReplicaGeneration(group, options.agent, nativeKey, c.Device.ID)
		replicaID, err := sessionhub.DeriveReplicaKey(secrets.IdentifierKey, options.sessionID, options.agent, nativeKey, c.Device.ID, generation)
		if err != nil {
			return sessionAttachReport{}, fmt.Errorf("session attach: derive Replica identity: %w", err)
		}
		baseHeads := []string(nil)
		if options.parent != "" {
			baseHeads = []string{options.parent}
		}
		binding = newNativeAttachBinding(hubScope.ID, projectID, options.sessionID, options.agent, options.nativeID, replicaID, generation, baseHeads)
	}

	if group.SessionDescriptor == nil {
		if err := syncer.PutSessionDescriptorForDevice(ctx, access.Store, access.Public, mustSessionHubLayout(hubScope.ID, projectID, options.sessionID), c.Device.ID, sessionDescriptor); err != nil {
			return sessionAttachReport{}, fmt.Errorf("session attach: publish missing Session descriptor: %w", err)
		}
	}
	if err := registry.BindNativeSession(projectID, options.sessionID, sessionhub.NativeSessionBinding{
		Agent:           options.agent,
		NativeSessionID: options.nativeID,
		BoundAt:         time.Now().UTC().Round(0),
	}); err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: bind local native session: %w", err)
	}
	if err := sessionhub.SaveLocalBinding(configDir, binding); err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: save local binding: %w", err)
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return sessionAttachReport{}, fmt.Errorf("session attach: save local Session Hub registry: %w", err)
	}
	return sessionAttachReport{
		Action:     "attach",
		Scope:      "project",
		Hub:        hubScope,
		Project:    projectScope,
		SessionID:  options.sessionID,
		Agent:      options.agent,
		NativeID:   options.nativeID,
		DeviceID:   c.Device.ID,
		ReplicaID:  binding.ReplicaID,
		Generation: binding.Generation,
		Parent:     firstAttachParent(binding.Origin.BaseHeads),
		State:      state,
	}, nil
}

func collectSessionReconcile(ctx context.Context, c *config.Config, configDir, projectDir string, options sessionOptions, input io.Reader, prompt io.Writer) (sessionReconcileReport, error) {
	if c == nil {
		return sessionReconcileReport{}, errors.New("session reconcile: configuration is unavailable")
	}
	if err := devicePullError("session reconcile", c); err != nil {
		return sessionReconcileReport{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: local device identity is invalid: %w", err)
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		return sessionReconcileReport{}, errors.New("session reconcile: current directory has no stable project identity")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: load local sync material: %w", err)
	}
	hubScope, projectScope, projectID, err := sessionHubAndProjectForConfig(secrets.IdentifierKey, current, c)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: prepare Session Hub identity: %w", err)
	}
	installed, err := adapter.FindInstalled(ctx, options.agent)
	if err != nil {
		if errors.Is(err, adapter.ErrNotInstalled) {
			return sessionReconcileReport{}, fmt.Errorf("session reconcile: Agent %s is not installed", resumeAgentLabel(options.agent))
		}
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: inspect Agent %s: %w", resumeAgentLabel(options.agent), err)
	}
	refs, err := installed.Layout.DiscoverSessions(current.Root)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: discover local %s sessions: %w", resumeAgentLabel(options.agent), err)
	}
	localRef, ok := findNativeSessionRef(refs, options.nativeID)
	if !ok {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: native session %q was not found for the current project", safeListText(options.nativeID))
	}
	localRef.Agent = options.agent
	localRef.ProjectPath = current.Root
	data, err := installed.Layout.ReadSession(localRef)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: read local native session: %w", err)
	}
	if data.DroppedTail || data.Skipped != 0 {
		return sessionReconcileReport{}, errors.New("session reconcile: local native session has an incomplete or damaged record stream")
	}
	stream, err := syncflow.CanonicalizeSession(data, adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installed.Installation.DataDir}, installed.Installation)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: canonicalize local native session: %w", err)
	}
	report := sessionReconcileReport{
		Action:       "reconcile",
		Scope:        "project",
		Hub:          hubScope,
		Project:      projectScope,
		Agent:        options.agent,
		NativeID:     options.nativeID,
		DeviceID:     c.Device.ID,
		State:        "unbound",
		Binding:      "missing",
		LocalRecords: uint64(len(stream.Records)),
	}

	registry, registryErr := loadSessionRegistryForRead(configDir, secrets.IdentifierKey, hubScope.ID)
	if registryErr == nil {
		if record, exists := registry.FindSessionByNative(projectID, options.agent, options.nativeID, ""); exists {
			report.SessionID = record.Descriptor.SessionID
			report.Binding = "registry"
			report.State = "unpublished"
		}
	} else if !errors.Is(registryErr, sessionhub.ErrRegistryNotFound) {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: load local Session Hub registry: %w", registryErr)
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "session reconcile")
	if err != nil {
		return sessionReconcileReport{}, err
	}
	defer access.close()
	group, replica, err := findRemoteNativeReplica(ctx, access, hubScope, projectID, options.agent, secrets.IdentifierKey, options.nativeID, c.Device.ID)
	if errors.Is(err, syncer.ErrNoReplicaMetadata) {
		report.Explanation = "no v2 Replica for this native session has been published"
		return report, nil
	}
	if err != nil {
		return sessionReconcileReport{}, err
	}
	report.SessionID = group.SessionID
	report.ReplicaID = replica.Descriptor.ReplicaID
	report.Generation = replica.Descriptor.Source.Generation
	if report.Binding == "registry" {
		report.Binding = "registry"
	} else {
		report.Binding = "remote-only"
	}
	snapshot, err := syncer.FetchCompleteReplica(ctx, access.Store, replica.Layout, access.Identities)
	if err != nil {
		return sessionReconcileReport{}, fmt.Errorf("session reconcile: read complete remote Replica: %w", err)
	}
	comparison := syncer.CompareRecords(stream.Records, snapshot.Records)
	report.RemoteRecords = uint64(len(snapshot.Records))
	report.CommonPrefix = comparison.CommonPrefix
	report.RemoteHead = snapshot.Tip.HeadDigest
	switch comparison.Relation {
	case syncer.Equal:
		report.State = "exact"
	case syncer.LeftPrefix:
		report.State = "behind"
	case syncer.RightPrefix:
		report.State = "ahead"
	case syncer.Diverged:
		report.State = "diverged"
	}
	return report, nil
}

func findNativeSessionRef(refs []adapter.SessionRef, nativeID string) (adapter.SessionRef, bool) {
	for _, ref := range refs {
		if ref.NativeID == nativeID {
			return ref, true
		}
	}
	return adapter.SessionRef{}, false
}

func fetchSessionHubGroup(ctx context.Context, access *domainAccess, hub sessionHubScope, projectID, sessionID string) (syncer.ProjectReplicaMetadataRef, error) {
	if access == nil {
		return syncer.ProjectReplicaMetadataRef{}, errors.New("session: authenticated Session Hub access is unavailable")
	}
	layout, err := syncer.NewProjectHubLayout(hub.ID, projectID)
	if err != nil {
		return syncer.ProjectReplicaMetadataRef{}, fmt.Errorf("session: prepare Session Hub Project layout: %w", err)
	}
	groups, err := syncer.FetchProjectReplicaMetadataWithDevices(ctx, access.Store, layout, access.Identities, access.allowedDevices())
	if errors.Is(err, syncer.ErrNoReplicaMetadata) {
		return syncer.ProjectReplicaMetadataRef{}, fmt.Errorf("session: logical Session %q is not available in the current Hub", safeListText(sessionID))
	}
	if err != nil {
		return syncer.ProjectReplicaMetadataRef{}, fmt.Errorf("session: read Session Hub metadata: %w", err)
	}
	for _, group := range groups {
		if group.SessionID == sessionID {
			return group, nil
		}
	}
	return syncer.ProjectReplicaMetadataRef{}, fmt.Errorf("session: logical Session %q is not available in the current Hub", safeListText(sessionID))
}

func attachSessionDescriptor(group syncer.ProjectReplicaMetadataRef, projectID, sessionID, agent, deviceID string, ref adapter.SessionRef) (sessionhub.SessionDescriptor, error) {
	if group.SessionDescriptor != nil {
		if group.SessionDescriptor.SessionID != sessionID || group.SessionDescriptor.ProjectID != projectID {
			return sessionhub.SessionDescriptor{}, errors.New("session attach: remote Session descriptor has an invalid hierarchy")
		}
		if err := group.SessionDescriptor.Validate(); err != nil {
			return sessionhub.SessionDescriptor{}, fmt.Errorf("session attach: remote Session descriptor is invalid: %w", err)
		}
		return *group.SessionDescriptor, nil
	}
	createdAt := ref.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	descriptor := sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: sessionID,
		ProjectID: projectID,
		Title:     safeListText(ref.Title),
		CreatedAt: createdAt.UTC().Round(0),
		CreatedBy: sessionhub.SessionCreator{Agent: agent, DeviceID: deviceID},
		Lifecycle: sessionhub.SessionActive,
	}
	if err := descriptor.Validate(); err != nil {
		return sessionhub.SessionDescriptor{}, fmt.Errorf("session attach: build Session descriptor: %w", err)
	}
	return descriptor, nil
}

func validateAttachParent(ctx context.Context, access *domainAccess, group syncer.ProjectReplicaMetadataRef, parent string, asRoot bool) error {
	if asRoot {
		return nil
	}
	if parent == "" {
		return errors.New("session attach: parent is required unless --as-root is set")
	}
	layout, err := sessionLayoutForGroup(group)
	if err != nil {
		return err
	}
	graph, err := syncer.FetchContributionGraph(ctx, access.Store, layout, access.Identities)
	if errors.Is(err, syncer.ErrNoContributions) {
		return errors.New("session attach: cannot use --parent because the Session has no Contributions")
	}
	if err != nil {
		return fmt.Errorf("session attach: read Contribution graph: %w", err)
	}
	if _, ok := graph.Contribution(parent); !ok {
		return fmt.Errorf("session attach: parent Contribution %q is not present in the selected Session", safeListText(parent))
	}
	return nil
}

func selectAttachReplica(group syncer.ProjectReplicaMetadataRef, agent, nativeKey, localDeviceID string) (*syncer.ReplicaMetadata, error) {
	matches := make([]syncer.ReplicaMetadata, 0)
	for index := range group.Replicas {
		replica := group.Replicas[index]
		if replica.Descriptor.Source.Agent == agent && replica.Descriptor.Source.NativeSessionKey == nativeKey {
			matches = append(matches, replica)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	local := matches[:0]
	for _, replica := range matches {
		if replica.Layout.DeviceID() == localDeviceID {
			local = append(local, replica)
		}
	}
	if len(local) == 0 {
		// A Replica published by another device is not a safe binding target
		// for this local native session. Create a new local Replica instead.
		return nil, nil
	}
	matches = local
	sort.Slice(matches, func(i, j int) bool {
		left, right := matches[i].Descriptor.Source.Generation, matches[j].Descriptor.Source.Generation
		if left != right {
			return left > right
		}
		return matches[i].Descriptor.ReplicaID < matches[j].Descriptor.ReplicaID
	})
	if len(matches) > 1 && matches[0].Layout.DeviceID() == localDeviceID && matches[1].Layout.DeviceID() == localDeviceID && matches[0].Descriptor.Source.Generation == matches[1].Descriptor.Source.Generation {
		return nil, errors.New("session attach: multiple Replicas for this local Agent session have the same generation")
	}
	return &matches[0], nil
}

func nextLocalReplicaGeneration(group syncer.ProjectReplicaMetadataRef, agent, nativeKey, deviceID string) uint64 {
	var generation uint64 = 1
	for _, replica := range group.Replicas {
		if replica.Layout.DeviceID() == deviceID && replica.Descriptor.Source.Agent == agent && replica.Descriptor.Source.NativeSessionKey == nativeKey && replica.Descriptor.Source.Generation >= generation {
			generation = replica.Descriptor.Source.Generation + 1
		}
	}
	return generation
}

func bindingForExistingReplica(ctx context.Context, configDir string, access *domainAccess, current project.Project, installed adapter.AgentSessions, localRef adapter.SessionRef, localStream syncflow.CanonicalStream, group syncer.ProjectReplicaMetadataRef, replica syncer.ReplicaMetadata, options sessionOptions) (sessionhub.LocalBinding, string, error) {
	if replica.Tip == nil {
		return sessionhub.LocalBinding{}, "", errors.New("session attach: selected remote Replica has no complete-prefix tip")
	}
	loaded, err := sessionhub.LoadLocalBinding(configDir, replica.Layout.HubKey(), replica.Layout.ProjectKey(), replica.Layout.SessionKey(), replica.Descriptor.ReplicaID, options.agent)
	if err == nil {
		if loaded.NativeSessionID != options.nativeID {
			return sessionhub.LocalBinding{}, "", errors.New("session attach: existing local binding points to another native session")
		}
		return loaded, "already-attached", nil
	}
	if !errors.Is(err, sessionhub.ErrLocalBindingNotFound) {
		return sessionhub.LocalBinding{}, "", fmt.Errorf("session attach: read existing local binding: %w", err)
	}
	snapshot, err := syncer.FetchCompleteReplica(ctx, access.Store, replica.Layout, access.Identities)
	if err != nil {
		return sessionhub.LocalBinding{}, "", fmt.Errorf("session attach: verify existing remote Replica: %w", err)
	}
	comparison := syncer.CompareRecords(localStream.Records, snapshot.Records)
	if comparison.Relation != syncer.Equal {
		return sessionhub.LocalBinding{}, "", fmt.Errorf("session attach: local native session does not exactly match the existing remote Replica (%s)", comparison.Relation.String())
	}
	sessionLayout, err := replica.Layout.SessionLayout()
	if err != nil {
		return sessionhub.LocalBinding{}, "", err
	}
	contributions, err := syncer.FetchSessionContributions(ctx, access.Store, sessionLayout, access.Identities)
	if errors.Is(err, syncer.ErrNoContributions) {
		contributions = nil
	} else if err != nil {
		return sessionhub.LocalBinding{}, "", fmt.Errorf("session attach: read existing Contribution cursor: %w", err)
	}
	cursor := contributionCursorForReplica(contributions, replica.Descriptor.ReplicaID, replica.Descriptor.Source.Generation, replica.Layout.DeviceID())
	origin := replica.Descriptor.Origin
	if options.asRoot && len(origin.BaseHeads) != 0 {
		return sessionhub.LocalBinding{}, "", errors.New("session attach: existing Replica was not created as a root")
	}
	if options.parent != "" && !sameSessionStringList(origin.BaseHeads, []string{options.parent}) {
		return sessionhub.LocalBinding{}, "", errors.New("session attach: existing Replica parent differs from --parent")
	}
	return sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           replica.Layout.HubKey(),
		ProjectID:       replica.Layout.ProjectKey(),
		SessionID:       replica.Layout.SessionKey(),
		Agent:           options.agent,
		NativeSessionID: options.nativeID,
		ReplicaID:       replica.Descriptor.ReplicaID,
		Generation:      replica.Descriptor.Source.Generation,
		ReplicaCursor: sessionhub.ReplicaCursor{
			NextShard:   snapshot.Tip.LastShard + 1,
			RecordCount: snapshot.Tip.RecordCount,
			HeadDigest:  snapshot.Tip.HeadDigest,
		},
		ContributionCursor: cursor,
		Origin: sessionhub.BindingOrigin{
			Kind:      bindingOriginKind(origin.Kind),
			BaseHeads: append([]string(nil), origin.BaseHeads...),
		},
	}, "attached-existing-replica", nil
}

func newNativeAttachBinding(hubID, projectID, sessionID, agent, nativeID, replicaID string, generation uint64, baseHeads []string) sessionhub.LocalBinding {
	empty := syncer.EmptyDigest()
	originKind := sessionhub.ReplicaOriginNative
	// An explicit --parent attach is a continuation from an authenticated
	// Session Hub head. It is not a materialization, but it is also not an
	// independent native root: the provenance must survive the first push so
	// the resulting Contribution can point at that parent. Reuse the existing
	// same-agent continuation kind instead of creating a second, attach-only
	// provenance format.
	if len(baseHeads) != 0 {
		originKind = sessionhub.ReplicaOriginSameAgentRestore
	}
	return sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           hubID,
		ProjectID:       projectID,
		SessionID:       sessionID,
		Agent:           agent,
		NativeSessionID: nativeID,
		ReplicaID:       replicaID,
		Generation:      generation,
		ReplicaCursor:   sessionhub.ReplicaCursor{NextShard: 1, HeadDigest: fmt.Sprintf("%x", empty[:])},
		Origin:          sessionhub.BindingOrigin{Kind: originKind, BaseHeads: append([]string(nil), baseHeads...)},
	}
}

func contributionCursorForReplica(contributions []sessionhub.Contribution, replicaID string, generation uint64, deviceID string) sessionhub.ContributionCursor {
	var cursor sessionhub.ContributionCursor
	for _, contribution := range contributions {
		if contribution.Source.ReplicaID != replicaID || contribution.Source.Generation != generation || contribution.Source.DeviceID != deviceID || len(contribution.Ranges) == 0 {
			continue
		}
		end := contribution.Ranges[len(contribution.Ranges)-1].EndRecord
		if end > cursor.EndRecord || end == cursor.EndRecord && contribution.ContributionID > cursor.LastContributionID {
			cursor = sessionhub.ContributionCursor{EndRecord: end, LastContributionID: contribution.ContributionID}
		}
	}
	return cursor
}

func findRemoteNativeReplica(ctx context.Context, access *domainAccess, hub sessionHubScope, projectID, agent string, identifierKey []byte, nativeID, localDeviceID string) (syncer.ProjectReplicaMetadataRef, syncer.ReplicaMetadata, error) {
	layout, err := syncer.NewProjectHubLayout(hub.ID, projectID)
	if err != nil {
		return syncer.ProjectReplicaMetadataRef{}, syncer.ReplicaMetadata{}, fmt.Errorf("session reconcile: prepare Project layout: %w", err)
	}
	groups, err := syncer.FetchProjectReplicaMetadataWithDevices(ctx, access.Store, layout, access.Identities, access.allowedDevices())
	if err != nil {
		return syncer.ProjectReplicaMetadataRef{}, syncer.ReplicaMetadata{}, err
	}
	nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, agent, nativeID)
	if err != nil {
		return syncer.ProjectReplicaMetadataRef{}, syncer.ReplicaMetadata{}, fmt.Errorf("session reconcile: derive native identity: %w", err)
	}
	type match struct {
		group   syncer.ProjectReplicaMetadataRef
		replica syncer.ReplicaMetadata
	}
	matches := make([]match, 0)
	for _, group := range groups {
		for _, replica := range group.Replicas {
			if replica.Descriptor.Source.Agent == agent && replica.Descriptor.Source.NativeSessionKey == nativeKey {
				matches = append(matches, match{group: group, replica: replica})
			}
		}
	}
	if len(matches) == 0 {
		return syncer.ProjectReplicaMetadataRef{}, syncer.ReplicaMetadata{}, syncer.ErrNoReplicaMetadata
	}
	sort.Slice(matches, func(i, j int) bool {
		left, right := matches[i].replica, matches[j].replica
		leftLocal := left.Layout.DeviceID() == localDeviceID
		rightLocal := right.Layout.DeviceID() == localDeviceID
		if leftLocal != rightLocal {
			return leftLocal
		}
		if left.Descriptor.Source.Generation != right.Descriptor.Source.Generation {
			return left.Descriptor.Source.Generation > right.Descriptor.Source.Generation
		}
		return left.Descriptor.ReplicaID < right.Descriptor.ReplicaID
	})
	return matches[0].group, matches[0].replica, nil
}

func sessionLayoutForGroup(group syncer.ProjectReplicaMetadataRef) (syncer.SessionHubLayout, error) {
	if len(group.Replicas) != 0 {
		return group.Replicas[0].Layout.SessionLayout()
	}
	return syncer.SessionHubLayout{}, errors.New("session attach: Session has no Replica layout")
}

func mustSessionHubLayout(hubID, projectID, sessionID string) syncer.SessionHubLayout {
	layout, _ := syncer.NewSessionHubLayout(hubID, projectID, sessionID)
	return layout
}

func firstAttachParent(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func sameSessionStringList(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func writeSessionAttachJSON(w io.Writer, report sessionAttachReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeSessionAttachText(w io.Writer, report sessionAttachReport) error {
	_, err := fmt.Fprintf(w, "attached: %s\nsession: %s\nagent: %s\nnative: %s\nreplica: %s\ngeneration: %d\nstate: %s\n", report.Action, safeListText(report.SessionID), safeListText(report.Agent), safeListText(report.NativeID), safeListText(report.ReplicaID), report.Generation, safeListText(report.State))
	if err != nil {
		return err
	}
	if report.Parent != "" {
		_, err = fmt.Fprintf(w, "parent: %s\n", safeListText(report.Parent))
	}
	return err
}

func writeSessionReconcileJSON(w io.Writer, report sessionReconcileReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeSessionReconcileText(w io.Writer, report sessionReconcileReport) error {
	_, err := fmt.Fprintf(w, "state: %s\nsession: %s\nagent: %s\nnative: %s\nlocal-records: %d\n", safeListText(report.State), safeListText(report.SessionID), safeListText(report.Agent), safeListText(report.NativeID), report.LocalRecords)
	if err != nil {
		return err
	}
	if report.ReplicaID != "" {
		_, err = fmt.Fprintf(w, "replica: %s\ngeneration: %d\nremote-records: %d\ncommon-prefix: %d\n", safeListText(report.ReplicaID), report.Generation, report.RemoteRecords, report.CommonPrefix)
	}
	if err == nil && report.Explanation != "" {
		_, err = fmt.Fprintf(w, "note: %s\n", safeListText(report.Explanation))
	}
	return err
}
