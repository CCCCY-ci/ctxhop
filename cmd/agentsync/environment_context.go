package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/environment"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

type environmentContext struct {
	List            listReport
	CurrentRoot     string
	ProjectID       string
	ProjectIdentity string
	ConfigDir       string
	RemoteSessions  []syncer.ProjectMetadataRef
	Access          *domainAccess
}

type environmentComponentChange struct {
	Component environment.Component `json:"component"`
	Path      string                `json:"path,omitempty"`
	State     string                `json:"state"`
	Backup    string                `json:"backup,omitempty"`
	Reason    string                `json:"reason,omitempty"`
}

func collectEnvironmentContext(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, prompt io.Writer) (environmentContext, error) {
	if c == nil {
		return environmentContext{}, errors.New("env: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return environmentContext{}, fmt.Errorf("env: %w", err)
	}
	if err := devicePullError("env", c); err != nil {
		return environmentContext{}, err
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return environmentContext{}, fmt.Errorf("env: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return environmentContext{}, fmt.Errorf("env: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return environmentContext{}, errors.New("env: project is excluded from synchronization")
	case projectModePushOnly:
		return environmentContext{}, errors.New("env: project is configured as push-only; remote sessions are unavailable")
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return environmentContext{}, fmt.Errorf("env: local device identity is invalid: %w", err)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return environmentContext{}, fmt.Errorf("env: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return environmentContext{}, fmt.Errorf("env: derive project identity: %w", err)
	}
	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "env")
	if err != nil {
		return environmentContext{}, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			access.close()
		}
	}()

	remoteSessions, err := syncer.FetchProjectMetadataWithIdentitiesAndDevices(ctx, access.Store, projectID, access.Identities, access.allowedDevices())
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		remoteSessions = nil
	} else if err != nil {
		return environmentContext{}, fmt.Errorf("env: read encrypted session metadata: %w", err)
	}
	localSessions, err := discoverListSessions(current.Root)
	if err != nil {
		return environmentContext{}, err
	}
	closeOnError = false
	return environmentContext{
		List:            mergeListSessions(c.Device.ID, secrets.IdentifierKey, projectID, localSessions, remoteSessions),
		CurrentRoot:     current.Root,
		ProjectID:       projectID,
		ProjectIdentity: current.Identity.Value,
		ConfigDir:       configDir,
		RemoteSessions:  remoteSessions,
		Access:          access,
	}, nil
}

func buildEnvironmentPreviewReport(ctx context.Context, state environmentContext, session *listSession) environmentPreviewReport {
	report := environmentPreviewReport{
		Scope:        state.List.Scope,
		Session:      session.RemoteID,
		Agent:        session.Agent,
		NativeID:     session.NativeID,
		Dependencies: append([]environment.Reference(nil), session.Dependencies...),
		Components:   append([]environment.Component(nil), session.Components...),
		Status:       "observed-only",
		Notes: []string{
			"only structured dependencies and filtered component summaries recorded in the encrypted env manifest are shown",
			"component bodies are not applied; no local files or commands were changed",
		},
	}
	agentHome := ""
	if session.Agent != "" {
		installed, err := adapter.FindInstalled(ctx, session.Agent)
		if err == nil {
			agentHome = installed.Installation.DataDir
		} else if !errors.Is(err, adapter.ErrNotInstalled) {
			report.Notes = append(report.Notes, "the local agent installation could not be inspected")
		}
	}
	for _, component := range report.Components {
		local := environment.InspectComponent(component, session.Agent, agentHome, state.CurrentRoot)
		report.Changes = append(report.Changes, environmentComponentChange{
			Component: component,
			Path:      local.Path,
			State:     local.State,
			Backup:    local.Backup,
			Reason:    local.Reason,
		})
	}
	if len(report.Dependencies) == 0 {
		report.Notes = append(report.Notes, "no structured dependency was observed in this session")
	}
	if len(report.Changes) != 0 {
		report.Notes = append(report.Notes, "local component state is compared by fingerprint; only Codex Skill files have an apply target")
	}
	return report
}

func readEnvironmentComponentContents(ctx context.Context, state environmentContext, session *listSession) ([]environment.ComponentContent, error) {
	group, ok := findEnvironmentGroup(state.RemoteSessions, session)
	if !ok {
		return nil, errors.New("env apply: remote session metadata was not found")
	}
	source, ok := selectEnvironmentSource(group)
	if !ok {
		return nil, errors.New("env apply: no remote environment component body is available")
	}
	layout, err := syncer.NewObjectLayout(state.ProjectID, group.SessionID, source.DeviceID)
	if err != nil {
		return nil, err
	}
	manifest, err := syncer.ReadEnvironmentManifest(ctx, state.Access.Store, layout, state.Access.Identities)
	if err != nil {
		return nil, fmt.Errorf("env apply: read encrypted environment components: %w", err)
	}
	return manifest.Components, nil
}

func applyEnvironmentComponents(ctx context.Context, state environmentContext, session *listSession, report *environmentPreviewReport) error {
	if report == nil {
		return errors.New("env apply: report is required")
	}
	needsBody := false
	for _, change := range report.Changes {
		if change.Component.Kind == "skill" && (change.State == environment.ComponentStateMissing || change.State == environment.ComponentStateChanged) {
			needsBody = true
			break
		}
	}
	if !needsBody {
		report.Status = "no-changes"
		report.Notes = append(report.Notes, "no Skill file needs to be written; MCP and settings components remain preview-only")
		return nil
	}
	contents, err := readEnvironmentComponentContents(ctx, state, session)
	if err != nil {
		return err
	}
	agentHome := ""
	if session.Agent != "" {
		installed, installErr := adapter.FindInstalled(ctx, session.Agent)
		if installErr == nil {
			agentHome = installed.Installation.DataDir
		} else if !errors.Is(installErr, adapter.ErrNotInstalled) {
			return fmt.Errorf("env apply: inspect %s installation: %w", session.Agent, installErr)
		}
	}
	backupRoot := filepath.Join(
		state.ConfigDir,
		"state",
		"environment-backups",
		state.ProjectID,
		session.RemoteID,
		time.Now().UTC().Format("20060102T150405.000000000Z"),
	)
	var applyErrors []error
	applied := 0
	for index := range report.Changes {
		change := &report.Changes[index]
		if change.Component.Kind != "skill" || (change.State != environment.ComponentStateMissing && change.State != environment.ComponentStateChanged) {
			continue
		}
		content, found := findEnvironmentComponentContent(contents, change.Component)
		if !found {
			change.State = environment.ComponentStateFailed
			change.Reason = "the encrypted environment object has no matching component body"
			applyErrors = append(applyErrors, fmt.Errorf("%s: component body is unavailable", change.Component.Name))
			continue
		}
		local, applyErr := environment.ApplyComponent(content, session.Agent, agentHome, state.CurrentRoot, backupRoot)
		change.Path = local.Path
		change.State = local.State
		change.Backup = local.Backup
		change.Reason = local.Reason
		if applyErr != nil {
			applyErrors = append(applyErrors, fmt.Errorf("%s: %w", change.Component.Name, applyErr))
			continue
		}
		if local.State == environment.ComponentStateApplied {
			applied++
		}
	}
	switch {
	case len(applyErrors) != 0 && applied != 0:
		report.Status = "partial"
	case len(applyErrors) != 0:
		report.Status = "failed"
	case applied != 0:
		report.Status = "applied"
	default:
		report.Status = "no-changes"
	}
	if applied != 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("applied %d filtered Codex Skill component(s); backups were created before replacements", applied))
	}
	if len(applyErrors) != 0 {
		return errors.Join(applyErrors...)
	}
	return nil
}

func findEnvironmentGroup(groups []syncer.ProjectMetadataRef, session *listSession) (*syncer.ProjectMetadataRef, bool) {
	if session == nil {
		return nil, false
	}
	for index := range groups {
		if groups[index].SessionID == session.RemoteID {
			return &groups[index], true
		}
	}
	for index := range groups {
		for _, device := range groups[index].Devices {
			summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
			if err == nil && summary.NativeID == session.NativeID {
				return &groups[index], true
			}
		}
	}
	return nil, false
}

func selectEnvironmentSource(group *syncer.ProjectMetadataRef) (*syncer.MetadataRef, bool) {
	if group == nil {
		return nil, false
	}
	best := -1
	var bestUpdated time.Time
	for index := range group.Devices {
		device := &group.Devices[index]
		if len(device.EnvironmentComponents) == 0 {
			continue
		}
		updated := time.Time{}
		if summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload); err == nil {
			updated = summary.UpdatedAt
		}
		if best < 0 || updated.After(bestUpdated) || (updated.Equal(bestUpdated) && device.DeviceID < group.Devices[best].DeviceID) {
			best = index
			bestUpdated = updated
		}
	}
	if best < 0 {
		return nil, false
	}
	return &group.Devices[best], true
}

func findEnvironmentComponentContent(contents []environment.ComponentContent, descriptor environment.Component) (environment.ComponentContent, bool) {
	for _, content := range contents {
		if content.Component == descriptor {
			return content, true
		}
	}
	return environment.ComponentContent{}, false
}
