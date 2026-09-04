package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

// resumeSelection is the common boundary between source selection and the
// existing environment/workspace/apply pipeline. The legacy path fills only
// Candidate, Plan and Agent; the v2 path also records the logical Session,
// selected Replica and explicit omissions.
type resumeSelection struct {
	Candidate         resumeCandidate
	Plan              syncflow.RestorePlan
	Agent             adapter.AgentSessions
	HubID             string
	HubName           string
	ProjectID         string
	SessionDescriptor sessionhub.SessionDescriptor
	ReplicaGeneration uint64
	ReplicaCursor     sessionhub.ReplicaCursor
	LocalState        nativeResumeLocalState
	LogicalSession    string
	AgentName         string
	ReplicaID         string
	OmittedAgents     []string
	OmittedReplicas   []string
	SelectedHeads     []string
	OmittedHeads      []string
	IncludedByAgent   map[string]uint64
	OmittedByAgent    map[string]uint64
	UnreadableHeads   []string
	LegacySessionID   string
}

type nativeResumeCandidate struct {
	Group       syncer.ProjectReplicaMetadataRef
	Replica     syncer.ReplicaMetadata
	NativeID    string
	Summary     syncflow.SessionSummary
	LegacyGroup syncer.ProjectMetadataRef
	HasLegacy   bool
}

type nativeResumeLocalState struct {
	State         string
	RemoteRecords uint64
	LocalRecords  uint64
	AppendRecords uint64
}

type nativeResumeSource struct {
	agent       string
	nativeKey   string
	nativeID    string
	deviceID    string
	summary     syncflow.SessionSummary
	legacyGroup syncer.ProjectMetadataRef
	hasLegacy   bool
}

// selectNativeResume reads v2 metadata, chooses exactly one logical
// Session/Agent/Replica tuple, and only then downloads the selected Replica
// body. It is deliberately separate from the v1 resolver: a v2 native resume
// must never combine branches from different Agents into one plan.
func selectNativeResume(ctx context.Context, configDir string, current project.Project, hubName string, identifierKey []byte, legacyGroups []syncer.ProjectMetadataRef, options resumeOptions, prompter *resumePrompter, access *domainAccess) (resumeSelection, error) {
	if access == nil {
		return resumeSelection{}, errors.New("resume: remote access is unavailable")
	}
	if options.version > 0 {
		return resumeSelection{}, errors.New("resume: Session Hub Replica has one append-only version; --version must be zero or omitted")
	}
	hubScope, _, v2ProjectID, err := sessionHubAndProjectInHub(identifierKey, current, hubName)
	if err != nil {
		return resumeSelection{}, fmt.Errorf("resume: prepare Session Hub identity: %w", err)
	}
	projectLayout, err := syncer.NewProjectHubLayout(hubScope.ID, v2ProjectID)
	if err != nil {
		return resumeSelection{}, fmt.Errorf("resume: prepare Session Hub metadata: %w", err)
	}
	v2Groups, err := syncer.FetchProjectReplicaMetadataWithDevices(ctx, access.Store, projectLayout, access.Identities, access.allowedDevices())
	if errors.Is(err, syncer.ErrNoReplicaMetadata) {
		return resumeSelection{}, errors.New("resume: no Session Hub Replica is available for this project")
	}
	if err != nil {
		return resumeSelection{}, fmt.Errorf("resume: read Session Hub Replica metadata: %w", err)
	}

	localRefs, err := discoverListSessionsWithContext(ctx, current.Root)
	if err != nil {
		return resumeSelection{}, fmt.Errorf("resume: discover local Agent sessions: %w", err)
	}
	candidate, err := chooseNativeResumeCandidate(v2Groups, options.session, options.agent, options.replica, legacyGroups, identifierKey, localRefs, prompter)
	if err != nil {
		return resumeSelection{}, err
	}
	if candidate.HasLegacy && candidate.LegacyGroup.SessionID != "" {
		readMode, err := loadLegacyMigrationReadMode(configDir, hubScope.ID, v2ProjectID, candidate.LegacyGroup.SessionID)
		if err != nil {
			return resumeSelection{}, err
		}
		if readMode == sessionhub.MigrationReadModeLegacy {
			return resumeSelection{}, errors.New("resume: v2 mapping is rolled back locally; use the legacy resume path or publish the Session again")
		}
	}
	// A v2 descriptor carries the native ID as encrypted recovery metadata, so
	// same-Agent resume does not depend on the legacy v1 summary being present
	// on this device. The legacy summary is still used when available for the
	// optional workspace/environment compatibility path.
	if candidate.NativeID == "" {
		candidate.NativeID = candidate.Replica.Descriptor.Source.NativeSessionID
	}
	if candidate.NativeID == "" {
		return resumeSelection{}, errors.New("resume: selected Replica has no recoverable native session identity; refresh its compatibility metadata or establish a local binding")
	}
	if candidate.Summary.Agent == "" {
		candidate.Summary.Agent = candidate.Replica.Descriptor.Source.Agent
	}
	if candidate.Summary.NativeID == "" {
		candidate.Summary.NativeID = candidate.NativeID
	}
	if candidate.Summary.Title == "" && candidate.Group.SessionDescriptor != nil {
		candidate.Summary.Title = candidate.Group.SessionDescriptor.Title
	}
	if candidate.Summary.CreatedAt.IsZero() {
		candidate.Summary.CreatedAt = candidate.Replica.Descriptor.CreatedAt
	}
	if candidate.Summary.UpdatedAt.IsZero() && candidate.Replica.Tip != nil {
		candidate.Summary.UpdatedAt = candidate.Replica.Tip.UpdatedAt
	}

	selectedAgent, err := selectResumeAgent(ctx, current.Root, syncflow.SessionSummary{
		Agent:    candidate.Replica.Descriptor.Source.Agent,
		NativeID: candidate.NativeID,
	})
	if err != nil {
		return resumeSelection{}, err
	}
	space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: selectedAgent.Installation.DataDir}
	snapshot, err := syncer.FetchCompleteReplica(ctx, access.Store, candidate.Replica.Layout, access.Identities)
	if err != nil {
		return resumeSelection{}, safeResumePlanError(err)
	}
	plan, err := syncflow.PlanNativeReplicaRestore(snapshot, space, selectedAgent.Installation, syncflow.RestoreOptions{
		AllowLimited: options.allowLimited,
	})
	if err != nil {
		return resumeSelection{}, safeResumePlanError(err)
	}
	sessionDescriptor := sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: candidate.Group.SessionID,
		ProjectID: v2ProjectID,
		Title:     candidate.Summary.Title,
		CreatedAt: candidate.Summary.CreatedAt,
		CreatedBy: sessionhub.SessionCreator{
			Agent:    candidate.Replica.Descriptor.Source.Agent,
			DeviceID: candidate.Replica.Layout.DeviceID(),
		},
		Lifecycle: sessionhub.SessionActive,
	}
	if candidate.Group.SessionDescriptor != nil {
		sessionDescriptor = *candidate.Group.SessionDescriptor
	}
	if err := sessionDescriptor.Validate(); err != nil {
		return resumeSelection{}, fmt.Errorf("resume: selected Session descriptor is invalid: %w", err)
	}
	if snapshot.Tip.LastShard == ^uint64(0) {
		return resumeSelection{}, errors.New("resume: selected Replica cursor exceeds the supported local range")
	}
	omittedAgents, omittedReplicas := nativeResumeOmissions(candidate.Group, candidate.Replica)
	coverage, err := collectNativeResumeCoverage(ctx, access.Store, candidate.Replica.Layout, access.Identities, candidate.Replica)
	if err != nil {
		return resumeSelection{}, err
	}
	compatibilityGroup := candidate.LegacyGroup
	if compatibilityGroup.SessionID == "" {
		compatibilityGroup = syncer.ProjectMetadataRef{SessionID: candidate.Group.SessionID}
	}
	return resumeSelection{
		Candidate:         resumeCandidate{Group: compatibilityGroup, Summary: candidate.Summary},
		Plan:              plan,
		Agent:             selectedAgent,
		HubID:             hubScope.ID,
		HubName:           hubName,
		ProjectID:         v2ProjectID,
		SessionDescriptor: sessionDescriptor,
		ReplicaGeneration: candidate.Replica.Descriptor.Source.Generation,
		ReplicaCursor: sessionhub.ReplicaCursor{
			NextShard:   snapshot.Tip.LastShard + 1,
			RecordCount: snapshot.Tip.RecordCount,
			HeadDigest:  snapshot.Tip.HeadDigest,
		},
		LogicalSession:  candidate.Group.SessionID,
		AgentName:       candidate.Replica.Descriptor.Source.Agent,
		ReplicaID:       candidate.Replica.Descriptor.ReplicaID,
		OmittedAgents:   omittedAgents,
		OmittedReplicas: omittedReplicas,
		SelectedHeads:   coverage.SelectedHeads,
		OmittedHeads:    coverage.OmittedHeads,
		IncludedByAgent: coverage.IncludedByAgent,
		OmittedByAgent:  coverage.OmittedByAgent,
		UnreadableHeads: coverage.UnreadableHeads,
		LegacySessionID: func() string {
			if candidate.HasLegacy {
				return candidate.LegacyGroup.SessionID
			}
			return ""
		}(),
	}, nil
}

type nativeResumeCoverage struct {
	SelectedHeads   []string
	OmittedHeads    []string
	IncludedByAgent map[string]uint64
	OmittedByAgent  map[string]uint64
	UnreadableHeads []string
}

// collectNativeResumeCoverage turns the complete logical Contribution graph
// into the omission facts shown by resume. A selected Replica is represented
// by its last Contribution range; every other current graph head remains
// visible as an omission instead of being silently folded into the restore.
func collectNativeResumeCoverage(ctx context.Context, store remote.Remote, replicaLayout syncer.ReplicaLayout, identities []*ecdh.PrivateKey, selected syncer.ReplicaMetadata) (nativeResumeCoverage, error) {
	sessionLayout, err := replicaLayout.SessionLayout()
	if err != nil {
		return nativeResumeCoverage{}, err
	}
	contributions, err := syncer.FetchSessionContributions(ctx, store, sessionLayout, identities)
	if errors.Is(err, syncer.ErrNoContributions) {
		return nativeResumeCoverage{IncludedByAgent: map[string]uint64{}, OmittedByAgent: map[string]uint64{}}, nil
	}
	if err != nil {
		return nativeResumeCoverage{}, fmt.Errorf("resume: read Contribution graph: %w", err)
	}
	graph, err := sessionhub.NewContributionGraph(selected.Descriptor.SessionID, contributions)
	if err != nil {
		return nativeResumeCoverage{}, fmt.Errorf("resume: build Contribution graph: %w", err)
	}
	selectedEnd := uint64(0)
	selectedHeads := make([]string, 0)
	for _, contribution := range contributions {
		if contribution.Source.ReplicaID != selected.Descriptor.ReplicaID || contribution.Source.DeviceID != selected.Layout.DeviceID() || contribution.Source.Generation != selected.Descriptor.Source.Generation || len(contribution.Ranges) == 0 {
			continue
		}
		end := contribution.Ranges[len(contribution.Ranges)-1].EndRecord
		if selected.Tip != nil && end != selected.Tip.RecordCount {
			continue
		}
		if end > selectedEnd {
			selectedEnd = end
			selectedHeads = selectedHeads[:0]
		}
		if end == selectedEnd {
			selectedHeads = append(selectedHeads, contribution.ContributionID)
		}
	}
	selectedHeads = uniqueSortedResumeStrings(selectedHeads)
	coverage := nativeResumeCoverage{
		SelectedHeads:   selectedHeads,
		IncludedByAgent: map[string]uint64{},
		OmittedByAgent:  map[string]uint64{},
	}
	if len(selectedHeads) == 0 {
		for _, contribution := range contributions {
			coverage.OmittedByAgent[contribution.Source.Agent]++
		}
		coverage.OmittedHeads = graph.Heads()
		return coverage, nil
	}
	selectedGraph, err := graph.Select(selectedHeads...)
	if err != nil {
		return nativeResumeCoverage{}, fmt.Errorf("resume: select Contribution ancestry: %w", err)
	}
	for _, id := range selectedGraph.SelectedIDs {
		contribution, ok := graph.Contribution(id)
		if ok {
			coverage.IncludedByAgent[contribution.Source.Agent]++
		}
	}
	for _, id := range selectedGraph.OmittedIDs {
		contribution, ok := graph.Contribution(id)
		if ok {
			coverage.OmittedByAgent[contribution.Source.Agent]++
		}
	}
	for _, head := range graph.Heads() {
		if !containsResumeString(selectedGraph.SelectedIDs, head) {
			coverage.OmittedHeads = append(coverage.OmittedHeads, head)
		}
	}
	return coverage, nil
}

func uniqueSortedResumeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsResumeString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// saveNativeResumeState updates the local metadata index after a successful
// restore. It records both the simple compatibility source binding and the
// richer v2 LocalBinding/cursor; neither contains session body records and
// neither is uploaded to the Remote.
func saveNativeResumeState(configDir string, identifierKey []byte, identity project.Project, localDeviceID string, selection resumeSelection) error {
	if selection.LogicalSession == "" || selection.HubID == "" || selection.ProjectID == "" || selection.ReplicaID == "" {
		return errors.New("resume: selected Session Hub binding is incomplete")
	}
	if selection.ReplicaGeneration == 0 {
		return errors.New("resume: selected Replica generation is missing")
	}
	if err := configDeviceID(localDeviceID); err != nil {
		return err
	}
	registry, err := loadSessionRegistryForRead(configDir, identifierKey, selection.HubID)
	if err != nil {
		return err
	}
	identityKind := sessionhub.ProjectIdentityRemote
	if identity.Identity.Kind == project.KindManual {
		identityKind = sessionhub.ProjectIdentityManual
	}
	createdAt := selection.SessionDescriptor.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	hubName := selection.HubName
	if hubName == "" {
		hubName = sessionhub.DefaultHubLogicalID
	}
	projectRecord, err := registry.EnsureProjectInHub(identifierKey, hubName, identityKind, identity.Identity.Value, createdAt)
	if err != nil {
		return err
	}
	if projectRecord.Descriptor.ProjectID != selection.ProjectID {
		return errors.New("resume: local Session Hub Project does not match the selected Replica")
	}
	if _, err := registry.EnsureSession(selection.ProjectID, selection.SessionDescriptor); err != nil {
		return err
	}
	if err := registry.BindNativeSession(selection.ProjectID, selection.LogicalSession, sessionhub.NativeSessionBinding{
		Agent:           selection.AgentName,
		NativeSessionID: selection.Candidate.Summary.NativeID,
		LegacySessionID: selection.LegacySessionID,
		BoundAt:         time.Now().UTC().Round(0),
	}); err != nil {
		return err
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return err
	}

	binding := sessionhub.LocalBinding{
		Version:         sessionhub.ModelVersion,
		HubID:           selection.HubID,
		ProjectID:       selection.ProjectID,
		SessionID:       selection.LogicalSession,
		Agent:           selection.AgentName,
		NativeSessionID: selection.Candidate.Summary.NativeID,
		ReplicaID:       selection.ReplicaID,
		Generation:      selection.ReplicaGeneration,
		ReplicaCursor:   selection.ReplicaCursor,
		// The restored records are already covered by the selected source
		// Replica. Keep that prefix out of the new device's Contribution stream;
		// publishNativeContribution will begin at this boundary when the local
		// Agent later appends new records.
		ContributionCursor: sessionhub.ContributionCursor{EndRecord: selection.ReplicaCursor.RecordCount},
		Origin: sessionhub.BindingOrigin{
			Kind:      sessionhub.ReplicaOriginSameAgentRestore,
			BaseHeads: append([]string(nil), selection.SelectedHeads...),
		},
	}
	if err := sessionhub.SaveLocalBinding(configDir, binding); err != nil {
		return err
	}
	return nil
}

// inspectNativeResumeLocalState compares the selected remote canonical stream
// with the target Agent's existing native session. This is a read-only
// preflight used by preview and is repeated by ApplyRestore before any write.
func inspectNativeResumeLocalState(configDir, projectRoot string, selection resumeSelection) (nativeResumeLocalState, error) {
	state := nativeResumeLocalState{
		State:         "missing",
		RemoteRecords: uint64(len(selection.Plan.CanonicalRecords)),
	}
	if selection.Agent.Layout == nil || selection.Candidate.Summary.NativeID == "" {
		return nativeResumeLocalState{}, errors.New("resume: selected NativeSession target is unavailable")
	}
	data, err := selection.Agent.Layout.ReadSession(adapter.SessionRef{
		Agent:       selection.AgentName,
		NativeID:    selection.Candidate.Summary.NativeID,
		ProjectPath: projectRoot,
	})
	if errors.Is(err, os.ErrNotExist) {
		state.AppendRecords = state.RemoteRecords
		return state, nil
	}
	if err != nil {
		state.State = "incompatible"
		return state, nil
	}
	if data.DroppedTail {
		state.State = "incompatible"
		return state, nil
	}
	stream, err := syncflow.CanonicalizeSession(data, adapter.PathSpace{
		ProjectRoot: projectRoot,
		AgentHome:   selection.Agent.Installation.DataDir,
	}, selection.Agent.Installation)
	if err != nil {
		state.State = "incompatible"
		return state, nil
	}
	state.LocalRecords = uint64(len(stream.Records))
	comparison := syncer.CompareRecords(stream.Records, selection.Plan.CanonicalRecords)
	switch comparison.Relation {
	case syncer.Equal:
		state.State = "exact"
	case syncer.LeftPrefix:
		state.State = "behind"
		state.AppendRecords = comparison.RightCount - comparison.LeftCount
	case syncer.RightPrefix:
		state.State = "ahead"
	case syncer.Diverged:
		state.State = "diverged"
	}
	if state.State == "" {
		state.State = "incompatible"
	}
	if _, bindingErr := sessionhub.LoadLocalBinding(configDir, selection.HubID, selection.ProjectID, selection.LogicalSession, selection.ReplicaID, selection.AgentName); bindingErr != nil && !errors.Is(bindingErr, sessionhub.ErrLocalBindingNotFound) {
		return nativeResumeLocalState{}, fmt.Errorf("resume: inspect local Session Hub binding: %w", bindingErr)
	}
	return state, nil
}

// chooseNativeResumeCandidate performs metadata-only filtering. A Replica
// with no tip is retained long enough to produce an explicit incomplete error,
// but is never selected as a restore source.
func chooseNativeResumeCandidate(groups []syncer.ProjectReplicaMetadataRef, requestedSession, requestedAgent, requestedReplica string, legacyGroups []syncer.ProjectMetadataRef, identifierKey []byte, localRefs []adapter.SessionRef, prompter *resumePrompter) (nativeResumeCandidate, error) {
	index := indexNativeResumeSources(legacyGroups, localRefs, identifierKey)
	matched := make([]nativeResumeCandidate, 0)
	for _, group := range groups {
		for _, replica := range group.Replicas {
			candidate := nativeResumeCandidate{Group: group, Replica: replica}
			source := lookupNativeResumeSource(index, replica.Descriptor.Source.Agent, replica.Descriptor.Source.NativeSessionKey, replica.Layout.DeviceID())
			if source != nil {
				candidate.NativeID = source.nativeID
				candidate.Summary = source.summary
				candidate.LegacyGroup = source.legacyGroup
				candidate.HasLegacy = source.hasLegacy
			}
			if requestedAgent != "" && replica.Descriptor.Source.Agent != requestedAgent {
				continue
			}
			if requestedReplica != "" && replica.Descriptor.ReplicaID != requestedReplica {
				continue
			}
			if requestedSession != "" && !nativeResumeSessionMatches(candidate, requestedSession) {
				continue
			}
			matched = append(matched, candidate)
		}
	}
	if len(matched) == 0 {
		return nativeResumeCandidate{}, errors.New("resume: requested Session/Agent/Replica is not available for this project")
	}

	complete := matched[:0]
	for _, candidate := range matched {
		if candidate.Replica.Tip != nil {
			complete = append(complete, candidate)
		}
	}
	if len(complete) == 0 {
		return nativeResumeCandidate{}, fmt.Errorf("resume: %w: selected Replica has no authenticated complete-prefix tip", syncer.ErrReplicaIncomplete)
	}

	if requestedSession == "" && requestedReplica == "" {
		logicalIDs := uniqueNativeResumeSessionIDs(complete)
		if len(logicalIDs) > 1 {
			choice, err := promptNativeResumeSession(complete, logicalIDs, prompter)
			if err != nil {
				return nativeResumeCandidate{}, err
			}
			complete = filterNativeResumeSession(complete, choice)
		}
	}
	if len(complete) == 0 {
		return nativeResumeCandidate{}, errors.New("resume: no complete Replica remains after Session selection")
	}

	agents := uniqueNativeResumeAgents(complete)
	if requestedAgent == "" && requestedReplica == "" && len(agents) > 1 {
		return nativeResumeCandidate{}, fmt.Errorf("resume: Session has multiple Agent sources (%s); specify --agent", strings.Join(agents, ", "))
	}
	if requestedReplica == "" && len(complete) > 1 {
		return nativeResumeCandidate{}, errors.New("resume: multiple complete Replicas are available; specify --replica")
	}
	if len(complete) != 1 {
		return nativeResumeCandidate{}, errors.New("resume: selected Replica is ambiguous")
	}
	return complete[0], nil
}

func indexNativeResumeSources(legacyGroups []syncer.ProjectMetadataRef, localRefs []adapter.SessionRef, identifierKey []byte) map[string][]nativeResumeSource {
	index := make(map[string][]nativeResumeSource)
	for _, group := range legacyGroups {
		for _, device := range group.Devices {
			summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
			if err != nil || summary.Agent == "" {
				continue
			}
			nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, summary.Agent, summary.NativeID)
			if err != nil {
				continue
			}
			addNativeResumeSource(index, nativeResumeSource{
				agent:       summary.Agent,
				nativeKey:   nativeKey,
				nativeID:    summary.NativeID,
				deviceID:    device.DeviceID,
				summary:     summary,
				legacyGroup: group,
				hasLegacy:   true,
			})
		}
	}
	for _, ref := range localRefs {
		if ref.Agent == "" || ref.NativeID == "" {
			continue
		}
		nativeKey, err := sessionhub.DeriveNativeSessionKey(identifierKey, ref.Agent, ref.NativeID)
		if err != nil {
			continue
		}
		addNativeResumeSource(index, nativeResumeSource{
			agent:     ref.Agent,
			nativeKey: nativeKey,
			nativeID:  ref.NativeID,
			deviceID:  "",
			summary: syncflow.SessionSummary{
				Agent:     ref.Agent,
				NativeID:  ref.NativeID,
				Title:     ref.Title,
				CreatedAt: ref.CreatedAt,
				UpdatedAt: ref.UpdatedAt,
			},
		})
	}
	return index
}

func addNativeResumeSource(index map[string][]nativeResumeSource, source nativeResumeSource) {
	key := nativeResumeSourceKey(source.agent, source.nativeKey, source.deviceID)
	for _, existing := range index[key] {
		if existing.nativeID == source.nativeID && existing.hasLegacy == source.hasLegacy {
			return
		}
	}
	index[key] = append(index[key], source)
	if source.deviceID != "" {
		general := nativeResumeSourceKey(source.agent, source.nativeKey, "")
		for _, existing := range index[general] {
			if existing.nativeID == source.nativeID && existing.hasLegacy == source.hasLegacy && existing.deviceID == source.deviceID {
				return
			}
		}
		index[general] = append(index[general], source)
	}
}

func lookupNativeResumeSource(index map[string][]nativeResumeSource, agent, nativeKey, deviceID string) *nativeResumeSource {
	keys := []string{
		nativeResumeSourceKey(agent, nativeKey, deviceID),
		nativeResumeSourceKey(agent, nativeKey, ""),
	}
	var fallback *nativeResumeSource
	for _, key := range keys {
		for _, source := range index[key] {
			if source.hasLegacy {
				value := source
				return &value
			}
			if fallback == nil {
				value := source
				fallback = &value
			}
		}
	}
	return fallback
}

func nativeResumeSourceKey(agent, nativeKey, deviceID string) string {
	return agent + "\x00" + nativeKey + "\x00" + deviceID
}

func nativeResumeSessionMatches(candidate nativeResumeCandidate, requested string) bool {
	return candidate.Group.SessionID == requested || (candidate.HasLegacy && candidate.LegacyGroup.SessionID == requested) || candidate.NativeID == requested
}

func uniqueNativeResumeSessionIDs(candidates []nativeResumeCandidate) []string {
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		seen[candidate.Group.SessionID] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func filterNativeResumeSession(candidates []nativeResumeCandidate, sessionID string) []nativeResumeCandidate {
	filtered := make([]nativeResumeCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Group.SessionID == sessionID {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func uniqueNativeResumeAgents(candidates []nativeResumeCandidate) []string {
	seen := make(map[string]struct{})
	for _, candidate := range candidates {
		seen[candidate.Replica.Descriptor.Source.Agent] = struct{}{}
	}
	agents := make([]string, 0, len(seen))
	for agent := range seen {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	return agents
}

func promptNativeResumeSession(candidates []nativeResumeCandidate, sessionIDs []string, prompter *resumePrompter) (string, error) {
	selectedID, err := prompter.pickSession(nativeResumePickerItems(candidates, sessionIDs))
	if err != nil {
		return "", err
	}
	for _, sessionID := range sessionIDs {
		if sessionID == selectedID {
			return selectedID, nil
		}
	}
	return "", errors.New("resume: selected Session is no longer available")
}

func nativeResumeOmissions(group syncer.ProjectReplicaMetadataRef, selected syncer.ReplicaMetadata) ([]string, []string) {
	agents := make([]string, 0)
	replicas := make([]string, 0)
	for _, replica := range group.Replicas {
		if replica.Descriptor.ReplicaID == selected.Descriptor.ReplicaID && replica.Layout.DeviceID() == selected.Layout.DeviceID() {
			continue
		}
		agents = appendUnique(agents, replica.Descriptor.Source.Agent)
		if replica.Descriptor.ReplicaID != "" {
			replicas = appendUnique(replicas, replica.Descriptor.ReplicaID)
		}
	}
	sort.Strings(agents)
	sort.Strings(replicas)
	return agents, replicas
}
