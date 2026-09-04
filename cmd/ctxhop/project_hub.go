package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// prepareProjectHubBinding resolves the requested Hub, rejects a local
// project that is already active in another Hub, and returns a registry with
// the target Project ensured. It deliberately does not save either config or
// registry; the caller commits both after the local root binding succeeds.
func prepareProjectHubBinding(c *config.Config, configDir, identity, requestedHub string) (string, sessionhub.Registry, error) {
	if c == nil {
		return "", sessionhub.Registry{}, errors.New("project bind: configuration is unavailable")
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", sessionhub.Registry{}, errors.New("project bind: project identity is required")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return "", sessionhub.Registry{}, fmt.Errorf("project bind: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return "", sessionhub.Registry{}, fmt.Errorf("project bind: load local Session Hub registry: %w", err)
	}
	hubName, hub, err := resolveLocalHubSelector(registry, secrets.IdentifierKey, requestedHub)
	if err != nil {
		return "", sessionhub.Registry{}, fmt.Errorf("project bind: %w", err)
	}
	if hub.Descriptor.Lifecycle != sessionhub.HubActive {
		return "", sessionhub.Registry{}, fmt.Errorf("project bind: Hub %q is archived", hubName)
	}

	for _, candidateHub := range registry.Hubs {
		if candidateHub.Descriptor.Name == hubName {
			continue
		}
		candidateProjectID, deriveErr := sessionhub.DeriveProjectKey(secrets.IdentifierKey, candidateHub.Descriptor.HubID, identity)
		if deriveErr != nil {
			return "", sessionhub.Registry{}, deriveErr
		}
		for _, candidateProject := range candidateHub.Projects {
			identityMatches := candidateProject.Identity == identity || candidateProject.Descriptor.ProjectID == candidateProjectID
			if !identityMatches || candidateProject.Descriptor.Lifecycle != sessionhub.ProjectActive {
				continue
			}
			return "", sessionhub.Registry{}, fmt.Errorf("project bind: project is already active in Hub %q; run project move %s --to %s", candidateHub.Descriptor.Name, safeListText(candidateProject.Descriptor.ProjectID), safeListText(hubName))
		}
	}
	identityKind := sessionhub.ProjectIdentityRemote
	if strings.HasPrefix(identity, "manual:") {
		identityKind = sessionhub.ProjectIdentityManual
	}
	if _, err := registry.EnsureProjectInHub(secrets.IdentifierKey, hubName, identityKind, identity, time.Now().UTC()); err != nil {
		return "", sessionhub.Registry{}, fmt.Errorf("project bind: register Project in Hub: %w", err)
	}
	return hubName, registry, nil
}

func savePreparedProjectHubRegistry(configDir string, registry sessionhub.Registry) error {
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return fmt.Errorf("project bind: save local Session Hub registry: %w", err)
	}
	return nil
}

func resolveLocalHubSelector(registry sessionhub.Registry, identifierKey []byte, selector string) (string, sessionhub.HubRecord, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		selector = sessionhub.DefaultHubLogicalID
	}
	if hub, ok := registry.HubByName(selector); ok {
		derived, err := sessionhub.DeriveHubKey(identifierKey, hub.Descriptor.Name)
		if err != nil {
			return "", sessionhub.HubRecord{}, err
		}
		if derived != hub.Descriptor.HubID {
			return "", sessionhub.HubRecord{}, errors.New("local Hub registry contains an invalid Hub identity")
		}
		return hub.Descriptor.Name, hub, nil
	}
	for _, hub := range registry.Hubs {
		if hub.Descriptor.HubID == selector {
			return hub.Descriptor.Name, hub, nil
		}
	}
	if selector == sessionhub.DefaultHubLogicalID {
		return "", sessionhub.HubRecord{}, errors.New("default Hub is missing from the local registry")
	}
	// A bind can create a new named Hub as part of the same local operation.
	// A selector that looks like a keyed ID is rejected instead of accidentally
	// creating a Hub whose display name is an opaque key.
	if looksLikeOpaqueHubKey(selector) {
		return "", sessionhub.HubRecord{}, fmt.Errorf("Hub ID %q is not known locally; create or discover that Hub first", safeListText(selector))
	}
	hub, err := registry.EnsureHub(identifierKey, selector, time.Now().UTC())
	if err != nil {
		return "", sessionhub.HubRecord{}, err
	}
	return hub.Descriptor.Name, hub, nil
}

func looksLikeOpaqueHubKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}

func collectProjectHubList(ctx context.Context, c *config.Config, configDir, selector string, input io.Reader, prompt io.Writer) (projectListReport, error) {
	if c == nil {
		return projectListReport{}, errors.New("project list: configuration is unavailable")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return projectListReport{}, fmt.Errorf("project list: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return projectListReport{}, fmt.Errorf("project list: load local Session Hub registry: %w", err)
	}
	hubName, hub, err := resolveLocalHubSelectorForRead(registry, selector, c)
	if err != nil {
		return projectListReport{}, fmt.Errorf("project list: %w", err)
	}
	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "project list")
	if err != nil {
		return projectListReport{}, err
	}
	defer access.close()
	refs, err := syncer.FetchHubProjectMetadata(ctx, access.Store, hub.Descriptor.HubID, access.Identities, access.allowedDevices())
	if err != nil && !errors.Is(err, syncer.ErrNoProjectMetadata) {
		return projectListReport{}, fmt.Errorf("project list: read Hub Projects: %w", err)
	}

	entries := make(map[string]*projectListEntry)
	for _, record := range hub.Projects {
		entry := projectHubListEntry(entries, record.Descriptor.ProjectID)
		entry.Hub = hubName
		entry.Identity = record.Identity
		entry.Mode = projectModeForIdentity(c, record.Identity)
		entry.Lifecycle = string(record.Descriptor.Lifecycle)
		entry.Roots = projectRootsForIdentity(c, record.Identity)
	}
	deviceCounts := make(map[string]map[string]struct{})
	for _, ref := range refs {
		entry := projectHubListEntry(entries, ref.Descriptor.ProjectID)
		if entry.Hub == "" {
			entry.Hub = hubName
		}
		if entry.Lifecycle != "" && entry.Lifecycle != string(ref.Descriptor.Lifecycle) {
			return projectListReport{}, fmt.Errorf("project list: conflicting lifecycle for Project %q", safeListText(ref.Descriptor.ProjectID))
		}
		entry.ProjectID = ref.Descriptor.ProjectID
		entry.Lifecycle = string(ref.Descriptor.Lifecycle)
		entry.Remote = true
		if deviceCounts[ref.Descriptor.ProjectID] == nil {
			deviceCounts[ref.Descriptor.ProjectID] = make(map[string]struct{})
		}
		deviceCounts[ref.Descriptor.ProjectID][ref.DeviceID] = struct{}{}
	}
	for projectID, devices := range deviceCounts {
		entries[projectID].Devices = len(devices)
	}
	// Session counts are metadata-only. A missing Session Replica set is a
	// valid empty Project and should not make Project listing fail.
	for projectID, entry := range entries {
		layout, layoutErr := syncer.NewProjectHubLayout(hub.Descriptor.HubID, projectID)
		if layoutErr != nil {
			return projectListReport{}, layoutErr
		}
		groups, groupsErr := syncer.FetchProjectReplicaMetadataWithDevices(ctx, access.Store, layout, access.Identities, access.allowedDevices())
		if groupsErr == nil {
			entry.Sessions = len(groups)
		} else if !errors.Is(groupsErr, syncer.ErrNoReplicaMetadata) {
			return projectListReport{}, fmt.Errorf("project list: read Project %q sessions: %w", safeListText(projectID), groupsErr)
		}
	}

	result := projectListReport{Scope: "hub", Hub: sessionHubScope{ID: hub.Descriptor.HubID, Name: hubName}, Projects: make([]projectListEntry, 0, len(entries))}
	for _, entry := range entries {
		if entry.Mode == "" {
			entry.Mode = projectModeNormal
		}
		sort.Strings(entry.Roots)
		result.Projects = append(result.Projects, *entry)
	}
	sort.Slice(result.Projects, func(i, j int) bool {
		if result.Projects[i].ProjectID != result.Projects[j].ProjectID {
			return result.Projects[i].ProjectID < result.Projects[j].ProjectID
		}
		return result.Projects[i].Identity < result.Projects[j].Identity
	})
	return result, nil
}

func resolveLocalHubSelectorForRead(registry sessionhub.Registry, selector string, c *config.Config) (string, sessionhub.HubRecord, error) {
	if strings.TrimSpace(selector) == "" {
		selector = configuredSessionHub(c)
	}
	if hub, ok := registry.HubByName(strings.TrimSpace(selector)); ok {
		return hub.Descriptor.Name, hub, nil
	}
	for _, hub := range registry.Hubs {
		if hub.Descriptor.HubID == strings.TrimSpace(selector) {
			return hub.Descriptor.Name, hub, nil
		}
	}
	return "", sessionhub.HubRecord{}, fmt.Errorf("Hub %q is not known locally; run hub create or hub list first", safeListText(selector))
}

func projectHubListEntry(entries map[string]*projectListEntry, projectID string) *projectListEntry {
	entry := entries[projectID]
	if entry == nil {
		entry = &projectListEntry{ProjectID: projectID, Mode: projectModeNormal}
		entries[projectID] = entry
	}
	return entry
}

func projectModeForIdentity(c *config.Config, identity string) string {
	if c == nil {
		return projectModeNormal
	}
	for _, value := range c.Projects.Excluded {
		if value == identity {
			return projectModeExcluded
		}
	}
	for _, value := range c.Projects.PushOnly {
		if value == identity {
			return projectModePushOnly
		}
	}
	return projectModeNormal
}

func projectRootsForIdentity(c *config.Config, identity string) []string {
	if c == nil || identity == "" {
		return nil
	}
	roots := make([]string, 0)
	for _, binding := range c.Projects.Bindings {
		if binding.Identity == identity {
			root := normalizedProjectRoot(binding.LocalRoot)
			if root != "" && !containsProjectRoot(roots, root) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

func reorderProjectMoveArgs(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		if (arg == "--to" || arg == "-to") && !strings.ContainsRune(arg, '=') && index+1 < len(args) {
			index++
			flags = append(flags, args[index])
		}
	}
	return append(flags, positionals...)
}
