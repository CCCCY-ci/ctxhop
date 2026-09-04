package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

const (
	maxProjectMoveObjectBytes = (64 << 20) + (1 << 20)
	projectMoveMetaObject     = "meta"
	projectMoveBodyObject     = "000001"
)

type projectMoveReport struct {
	Action             string `json:"action"`
	State              string `json:"state"`
	ProjectID          string `json:"projectId"`
	DestinationProject string `json:"destinationProjectId"`
	SourceHub          string `json:"sourceHub"`
	DestinationHub     string `json:"destinationHub"`
	SourceObjects      int    `json:"sourceObjects"`
	CopiedObjects      int    `json:"copiedObjects"`
	SessionCount       int    `json:"sessions"`
	BindingCount       int    `json:"bindings"`
}

type projectMoveLocation struct {
	HubIndex     int
	ProjectIndex int
	Hub          sessionhub.HubRecord
	Project      sessionhub.ProjectRecord
	Identity     string
}

func collectProjectMove(ctx context.Context, c *config.Config, configDir string, options projectOptions, input io.Reader, prompt io.Writer) (projectMoveReport, error) {
	if c == nil {
		return projectMoveReport{}, errors.New("project move: configuration is unavailable")
	}
	if ctx == nil {
		return projectMoveReport{}, errors.New("project move: context is required")
	}
	if err := ctx.Err(); err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: %w", err)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: load local sync material: %w", err)
	}
	registry, err := loadHubRegistry(configDir, secrets.IdentifierKey)
	if err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: load local Session Hub registry: %w", err)
	}
	location, err := locateProjectForMove(registry, c, secrets.IdentifierKey, options.projectID)
	if err != nil {
		return projectMoveReport{}, err
	}
	if location.Identity == "" {
		return projectMoveReport{}, errors.New("project move: the local canonical project identity is unavailable; bind the project before moving it")
	}
	destinationName, destinationHub, err := resolveLocalHubSelectorForRead(registry, options.hub, c)
	if err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: %w", err)
	}
	if destinationHub.Descriptor.HubID == location.Hub.Descriptor.HubID {
		return projectMoveReport{}, errors.New("project move: destination Hub is the current Hub")
	}
	if destinationHub.Descriptor.Lifecycle != sessionhub.HubActive {
		return projectMoveReport{}, fmt.Errorf("project move: destination Hub %q is archived", destinationName)
	}
	if location.Project.Descriptor.Lifecycle != sessionhub.ProjectActive {
		return projectMoveReport{}, fmt.Errorf("project move: source Project %q is archived", safeListText(location.Project.Descriptor.ProjectID))
	}
	destinationProjectID, err := sessionhub.DeriveProjectKey(secrets.IdentifierKey, destinationHub.Descriptor.HubID, location.Identity)
	if err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: derive destination Project identity: %w", err)
	}
	if err := validateMoveDestination(registry, destinationHub.Descriptor.HubID, destinationProjectID, location.Project); err != nil {
		return projectMoveReport{}, err
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "project move")
	if err != nil {
		return projectMoveReport{}, err
	}
	defer access.close()
	if err := syncer.PutHubDescriptorForDevice(ctx, access.Store, access.Public, destinationHub.Descriptor.HubID, c.Device.ID, destinationHub.Descriptor); err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: publish destination Hub descriptor: %w", err)
	}
	stats, err := moveRemoteProjectObjects(ctx, access, secrets.IdentifierKey, location.Hub.Descriptor.HubID, location.Project.Descriptor.ProjectID, destinationHub.Descriptor.HubID, destinationProjectID, location.Project.Descriptor, c.Device.ID)
	if err != nil {
		return projectMoveReport{}, err
	}

	bindings, err := moveLocalProjectBindings(configDir, location.Hub.Descriptor.HubID, location.Project.Descriptor.ProjectID, destinationHub.Descriptor.HubID, destinationProjectID)
	if err != nil {
		return projectMoveReport{}, err
	}
	if err := moveRegistryProject(&registry, location, destinationHub.Descriptor, destinationProjectID); err != nil {
		return projectMoveReport{}, err
	}
	if err := setConfiguredProjectHub(c, location.Identity, destinationName); err != nil {
		return projectMoveReport{}, err
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: save local Session Hub registry: %w", err)
	}
	if err := c.Save(configDir); err != nil {
		return projectMoveReport{}, fmt.Errorf("project move: save configuration: %w", err)
	}
	return projectMoveReport{
		Action:             projectActionMove,
		State:              "moved",
		ProjectID:          location.Project.Descriptor.ProjectID,
		DestinationProject: destinationProjectID,
		SourceHub:          location.Hub.Descriptor.Name,
		DestinationHub:     destinationName,
		SourceObjects:      stats.SourceObjects,
		CopiedObjects:      stats.CopiedObjects,
		SessionCount:       len(location.Project.Sessions),
		BindingCount:       bindings,
	}, nil
}

func locateProjectForMove(registry sessionhub.Registry, c *config.Config, identifierKey []byte, projectID string) (projectMoveLocation, error) {
	for hubIndex, hub := range registry.Hubs {
		for projectIndex, candidate := range hub.Projects {
			if candidate.Descriptor.ProjectID != projectID {
				continue
			}
			identity := strings.TrimSpace(candidate.Identity)
			if identity == "" {
				identity = identityForProjectID(c, identifierKey, projectID, hub.Descriptor.HubID)
			}
			return projectMoveLocation{HubIndex: hubIndex, ProjectIndex: projectIndex, Hub: hub, Project: candidate, Identity: identity}, nil
		}
	}
	// A registry can be absent after an interrupted bootstrap. Recover the
	// source Hub only from the local canonical binding and deterministic key;
	// never infer it from a title, path suffix, or remote listing order.
	for _, binding := range c.Projects.Bindings {
		identity := strings.TrimSpace(binding.Identity)
		if identity == "" {
			continue
		}
		for hubIndex, hub := range registry.Hubs {
			derived, err := sessionhub.DeriveProjectKey(identifierKey, hub.Descriptor.HubID, identity)
			if err != nil || derived != projectID {
				continue
			}
			return projectMoveLocation{
				HubIndex:     hubIndex,
				ProjectIndex: -1,
				Hub:          hub,
				Project: sessionhub.ProjectRecord{Descriptor: sessionhub.ProjectDescriptor{
					Version:             sessionhub.ModelVersion,
					HubID:               hub.Descriptor.HubID,
					ProjectID:           projectID,
					IdentityKind:        identityKindForValue(identity),
					IdentityFingerprint: projectID,
					CreatedAt:           time.Now().UTC().Round(0),
					Lifecycle:           sessionhub.ProjectActive,
				}, Identity: identity},
				Identity: identity,
			}, nil
		}
	}
	return projectMoveLocation{}, fmt.Errorf("project move: Project %q is not registered locally; run session discover or project bind first", safeListText(projectID))
}

func identityForProjectID(c *config.Config, identifierKey []byte, projectID, hubID string) string {
	if c == nil {
		return ""
	}
	for _, binding := range c.Projects.Bindings {
		if derived, err := sessionhub.DeriveProjectKey(identifierKey, hubID, binding.Identity); err == nil && derived == projectID {
			return binding.Identity
		}
	}
	return ""
}

func validateMoveDestination(registry sessionhub.Registry, hubID, projectID string, source sessionhub.ProjectRecord) error {
	for _, hub := range registry.Hubs {
		if hub.Descriptor.HubID != hubID {
			continue
		}
		for _, candidate := range hub.Projects {
			if candidate.Descriptor.ProjectID != projectID {
				continue
			}
			if candidate.Identity != "" && source.Identity != "" && candidate.Identity != source.Identity {
				return errors.New("project move: destination Project identity conflicts with the source")
			}
			if len(candidate.Sessions) != 0 && len(source.Sessions) != 0 {
				return errors.New("project move: destination Project already contains Sessions; refusing an ambiguous merge")
			}
		}
	}
	return nil
}

func identityKindForValue(identity string) sessionhub.ProjectIdentityKind {
	if strings.HasPrefix(identity, "manual:") {
		return sessionhub.ProjectIdentityManual
	}
	return sessionhub.ProjectIdentityRemote
}

func moveRegistryProject(registry *sessionhub.Registry, location projectMoveLocation, destinationHub sessionhub.HubDescriptor, destinationProjectID string) error {
	if registry == nil {
		return errors.New("project move: local Session Hub registry is unavailable")
	}
	source := location.Project
	source.Descriptor.Lifecycle = sessionhub.ProjectArchived
	if location.ProjectIndex >= 0 {
		registry.Hubs[location.HubIndex].Projects[location.ProjectIndex] = source
	}
	destination := location.Project
	destination.Descriptor.HubID = destinationHub.HubID
	destination.Descriptor.ProjectID = destinationProjectID
	destination.Descriptor.IdentityFingerprint = destinationProjectID
	destination.Descriptor.Lifecycle = sessionhub.ProjectActive
	for index := range destination.Sessions {
		destination.Sessions[index].Descriptor.ProjectID = destinationProjectID
	}
	for index, candidate := range registry.Hubs {
		if candidate.Descriptor.HubID != destinationHub.HubID {
			continue
		}
		for projectIndex := range candidate.Projects {
			if candidate.Projects[projectIndex].Descriptor.ProjectID == destinationProjectID {
				registry.Hubs[index].Projects[projectIndex] = destination
				return registry.Validate()
			}
		}
		registry.Hubs[index].Projects = append(registry.Hubs[index].Projects, destination)
		return registry.Validate()
	}
	return fmt.Errorf("project move: destination Hub %q is missing from the local registry", safeListText(destinationHub.Name))
}

type projectMoveRemoteStats struct {
	SourceObjects int
	CopiedObjects int
}

type projectMoveAttachmentRef struct {
	sessionID     string
	environmentID string
	deviceID      string
}

func moveRemoteProjectObjects(ctx context.Context, access *domainAccess, identifierKey []byte, sourceHubID, sourceProjectID, destinationHubID, destinationProjectID string, sourceDescriptor sessionhub.ProjectDescriptor, deviceID string) (projectMoveRemoteStats, error) {
	if access == nil || access.Store == nil || access.Public == nil {
		return projectMoveRemoteStats{}, errors.New("project move: authenticated remote access is unavailable")
	}
	sourceLayout, err := syncer.NewProjectHubLayout(sourceHubID, sourceProjectID)
	if err != nil {
		return projectMoveRemoteStats{}, fmt.Errorf("project move: source layout: %w", err)
	}
	destinationLayout, err := syncer.NewProjectHubLayout(destinationHubID, destinationProjectID)
	if err != nil {
		return projectMoveRemoteStats{}, fmt.Errorf("project move: destination layout: %w", err)
	}
	sourcePrefix, err := sourceLayout.ProjectPrefix()
	if err != nil {
		return projectMoveRemoteStats{}, err
	}
	destinationPrefix, err := destinationLayout.ProjectPrefix()
	if err != nil {
		return projectMoveRemoteStats{}, err
	}
	objects, err := access.Store.List(ctx, sourcePrefix)
	if err != nil {
		return projectMoveRemoteStats{}, fmt.Errorf("project move: list source Project objects: %w", err)
	}
	stats := projectMoveRemoteStats{SourceObjects: len(objects)}
	if len(objects) == 0 {
		moved := sourceDescriptor
		moved.HubID = destinationHubID
		moved.ProjectID = destinationProjectID
		moved.IdentityFingerprint = destinationProjectID
		moved.Lifecycle = sessionhub.ProjectActive
		if err := putMovedProjectDescriptor(ctx, access.Store, access.Public, access.Identities, destinationLayout, deviceID, moved); err != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: create destination Project descriptor: %w", err)
		}
		return stats, archiveSourceProjectDescriptor(ctx, access.Store, access.Public, sourceLayout, deviceID, sourceDescriptor)
	}

	handled := make(map[string]struct{})
	projectDescriptorDevices := make(map[string]struct{})
	type sessionDescriptorRef struct{ sessionID, deviceID, sourceKey string }
	sessionDescriptors := make([]sessionDescriptorRef, 0)
	attachments := make(map[projectMoveAttachmentRef]struct{})
	for _, object := range objects {
		suffix, ok := moveObjectSuffix(sourcePrefix, object.Key)
		if !ok {
			continue
		}
		if descriptorDevice, ok := parseMoveProjectDescriptorSuffix(suffix); ok {
			projectDescriptorDevices[descriptorDevice] = struct{}{}
			handled[object.Key] = struct{}{}
			continue
		}
		if sessionID, descriptorDevice, ok := parseMoveSessionDescriptorSuffix(suffix); ok {
			sessionDescriptors = append(sessionDescriptors, sessionDescriptorRef{sessionID: sessionID, deviceID: descriptorDevice, sourceKey: object.Key})
			handled[object.Key] = struct{}{}
			continue
		}
		if sessionID, environmentID, attachmentDevice, ok := parseMoveAttachmentSuffix(suffix); ok {
			attachments[projectMoveAttachmentRef{sessionID: sessionID, environmentID: environmentID, deviceID: attachmentDevice}] = struct{}{}
		}
	}

	if len(projectDescriptorDevices) == 0 {
		// A pre-v2 bootstrap may contain Replica objects without the Project
		// descriptor. The caller's authenticated local descriptor is still the
		// only safe identity source; publish it for the current device.
		projectDescriptorDevices[deviceID] = struct{}{}
	}
	devices := make([]string, 0, len(projectDescriptorDevices))
	for value := range projectDescriptorDevices {
		devices = append(devices, value)
	}
	sort.Strings(devices)
	for _, descriptorDevice := range devices {
		// Build each destination descriptor from the source descriptor afresh.
		// A missing descriptor for one device must not accidentally reuse the
		// descriptor fetched for the previous device in this loop.
		movedProject := sourceDescriptor
		if descriptor, descriptorErr := syncer.FetchProjectDescriptorForDevice(ctx, access.Store, sourceLayout, descriptorDevice, access.Identities); descriptorErr == nil {
			movedProject = descriptor
		} else if !errors.Is(descriptorErr, remote.ErrNotFound) && !errors.Is(descriptorErr, syncer.ErrReplicaDescriptorMissing) {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: read source Project descriptor: %w", descriptorErr)
		}
		movedProject.HubID = destinationHubID
		movedProject.ProjectID = destinationProjectID
		movedProject.IdentityFingerprint = destinationProjectID
		movedProject.Lifecycle = sessionhub.ProjectActive
		if err := putMovedProjectDescriptor(ctx, access.Store, access.Public, access.Identities, destinationLayout, descriptorDevice, movedProject); err != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: copy Project descriptor: %w", err)
		}
		stats.CopiedObjects++
	}

	for _, descriptor := range sessionDescriptors {
		sourceSessionLayout, layoutErr := sourceLayout.Session(descriptor.sessionID)
		if layoutErr != nil {
			return projectMoveRemoteStats{}, layoutErr
		}
		sessionDescriptor, fetchErr := syncer.FetchSessionDescriptorForDevice(ctx, access.Store, sourceSessionLayout, descriptor.deviceID, access.Identities)
		if fetchErr != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: read source Session descriptor: %w", fetchErr)
		}
		sessionDescriptor.ProjectID = destinationProjectID
		destinationSessionLayout, layoutErr := destinationLayout.Session(descriptor.sessionID)
		if layoutErr != nil {
			return projectMoveRemoteStats{}, layoutErr
		}
		if err := putMovedSessionDescriptor(ctx, access.Store, access.Public, access.Identities, destinationSessionLayout, descriptor.deviceID, sessionDescriptor); err != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: copy Session descriptor: %w", err)
		}
		stats.CopiedObjects++
	}

	requiredComponents := make(map[string]map[string]struct{})
	attachmentRefs := make([]projectMoveAttachmentRef, 0, len(attachments))
	for ref := range attachments {
		attachmentRefs = append(attachmentRefs, ref)
	}
	sort.Slice(attachmentRefs, func(i, j int) bool {
		left, right := attachmentRefs[i], attachmentRefs[j]
		if left.sessionID != right.sessionID {
			return left.sessionID < right.sessionID
		}
		if left.environmentID != right.environmentID {
			return left.environmentID < right.environmentID
		}
		return left.deviceID < right.deviceID
	})
	for _, ref := range attachmentRefs {
		sourceSessionLayout, layoutErr := sourceLayout.Session(ref.sessionID)
		if layoutErr != nil {
			return projectMoveRemoteStats{}, layoutErr
		}
		attachment, fetchErr := syncer.FetchEnvironmentAttachment(ctx, access.Store, sourceSessionLayout, ref.environmentID, ref.deviceID, access.Identities)
		if fetchErr != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: read source environment attachment: %w", fetchErr)
		}
		for index := range attachment.Components {
			component := &attachment.Components[index]
			if component.Scope == "project" {
				component.ProjectID = destinationProjectID
			}
			if requiredComponents[component.Fingerprint] == nil {
				requiredComponents[component.Fingerprint] = make(map[string]struct{})
			}
			requiredComponents[component.Fingerprint][ref.deviceID] = struct{}{}
		}
		destinationSessionLayout, layoutErr := destinationLayout.Session(ref.sessionID)
		if layoutErr != nil {
			return projectMoveRemoteStats{}, layoutErr
		}
		if err := putMovedEnvironmentAttachment(ctx, access.Store, access.Public, access.Identities, destinationSessionLayout, ref.environmentID, ref.deviceID, attachment); err != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: copy environment attachment: %w", err)
		}
		handled[moveAttachmentKey(sourcePrefix, ref, projectMoveMetaObject)] = struct{}{}
		handled[moveAttachmentKey(sourcePrefix, ref, projectMoveBodyObject)] = struct{}{}
		stats.CopiedObjects += 2
	}

	if err := copyMovedEnvironmentComponents(ctx, access, identifierKey, sourceHubID, destinationHubID, destinationProjectID, requiredComponents); err != nil {
		return projectMoveRemoteStats{}, err
	}

	for _, object := range objects {
		if _, ok := handled[object.Key]; ok {
			continue
		}
		suffix, ok := moveObjectSuffix(sourcePrefix, object.Key)
		if !ok || suffix == "" {
			continue
		}
		destinationKey, keyErr := checkedMoveKey(destinationPrefix + "/" + suffix)
		if keyErr != nil {
			return projectMoveRemoteStats{}, keyErr
		}
		if err := copyReencryptedMoveObject(ctx, access.Store, access.Public, access.Identities, object.Key, destinationKey, object.Size); err != nil {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: copy Project object: %w", err)
		}
		stats.CopiedObjects++
	}
	for _, descriptorDevice := range devices {
		descriptor := sourceDescriptor
		if fetched, fetchErr := syncer.FetchProjectDescriptorForDevice(ctx, access.Store, sourceLayout, descriptorDevice, access.Identities); fetchErr == nil {
			descriptor = fetched
		} else if !errors.Is(fetchErr, remote.ErrNotFound) && !errors.Is(fetchErr, syncer.ErrReplicaDescriptorMissing) {
			return projectMoveRemoteStats{}, fmt.Errorf("project move: read source Project descriptor for archive: %w", fetchErr)
		}
		if err := archiveSourceProjectDescriptor(ctx, access.Store, access.Public, sourceLayout, descriptorDevice, descriptor); err != nil {
			return projectMoveRemoteStats{}, err
		}
	}
	return stats, nil
}

func archiveSourceProjectDescriptor(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, layout syncer.ProjectHubLayout, deviceID string, descriptor sessionhub.ProjectDescriptor) error {
	descriptor.Lifecycle = sessionhub.ProjectArchived
	if err := syncer.PutProjectDescriptorForDevice(ctx, store, public, layout, deviceID, descriptor); err != nil {
		return fmt.Errorf("project move: archive source Project descriptor: %w", err)
	}
	return nil
}

func putMovedProjectDescriptor(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, layout syncer.ProjectHubLayout, deviceID string, descriptor sessionhub.ProjectDescriptor) error {
	if existing, err := syncer.FetchProjectDescriptorForDevice(ctx, store, layout, deviceID, identities); err == nil {
		left, leftErr := existing.MarshalBinary()
		right, rightErr := descriptor.MarshalBinary()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return errors.New("destination Project descriptor conflicts with the source")
		}
		return nil
	} else if !errors.Is(err, remote.ErrNotFound) {
		return err
	}
	return syncer.PutProjectDescriptorForDevice(ctx, store, public, layout, deviceID, descriptor)
}

func putMovedSessionDescriptor(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, layout syncer.SessionHubLayout, deviceID string, descriptor sessionhub.SessionDescriptor) error {
	if existing, err := syncer.FetchSessionDescriptorForDevice(ctx, store, layout, deviceID, identities); err == nil {
		left, leftErr := existing.MarshalBinary()
		right, rightErr := descriptor.MarshalBinary()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return errors.New("destination Session descriptor conflicts with the source")
		}
		return nil
	} else if !errors.Is(err, remote.ErrNotFound) {
		return err
	}
	return syncer.PutSessionDescriptorForDevice(ctx, store, public, layout, deviceID, descriptor)
}

func putMovedEnvironmentAttachment(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, layout syncer.SessionHubLayout, environmentID, deviceID string, attachment sessionhub.EnvironmentAttachment) error {
	if existing, err := syncer.FetchEnvironmentAttachment(ctx, store, layout, environmentID, deviceID, identities); err == nil {
		left, leftErr := existing.MarshalBinary()
		right, rightErr := attachment.MarshalBinary()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return errors.New("destination environment attachment conflicts with the source")
		}
		return nil
	} else if !errors.Is(err, remote.ErrNotFound) && !errors.Is(err, syncer.ErrEnvironmentObjectIncomplete) {
		return err
	}
	return syncer.PutEnvironmentAttachmentWithIdentities(ctx, store, public, layout, environmentID, deviceID, attachment, identities)
}

func copyMovedEnvironmentComponents(ctx context.Context, access *domainAccess, identifierKey []byte, sourceHubID, destinationHubID, destinationProjectID string, required map[string]map[string]struct{}) error {
	sourceLayout, err := syncer.NewEnvironmentHubLayout(sourceHubID)
	if err != nil {
		return fmt.Errorf("project move: source environment layout: %w", err)
	}
	destinationLayout, err := syncer.NewEnvironmentHubLayout(destinationHubID)
	if err != nil {
		return fmt.Errorf("project move: destination environment layout: %w", err)
	}
	fingerprints := make([]string, 0, len(required))
	for fingerprint := range required {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)
	for _, fingerprint := range fingerprints {
		devices := make([]string, 0, len(required[fingerprint]))
		for deviceID := range required[fingerprint] {
			devices = append(devices, deviceID)
		}
		sort.Strings(devices)
		for _, deviceID := range devices {
			sourceKey, keyErr := sessionhub.DeriveEnvironmentKey(identifierKey, sourceHubID, fingerprint)
			if keyErr != nil {
				return fmt.Errorf("project move: derive source environment component: %w", keyErr)
			}
			content, fetchErr := syncer.FetchEnvironmentComponent(ctx, access.Store, sourceLayout, sourceKey, deviceID, access.Identities)
			if fetchErr != nil {
				return fmt.Errorf("project move: read environment component: %w", fetchErr)
			}
			if content.Component.Scope == "project" {
				content.Component.ProjectID = destinationProjectID
			}
			destinationKey, keyErr := sessionhub.DeriveEnvironmentKey(identifierKey, destinationHubID, fingerprint)
			if keyErr != nil {
				return fmt.Errorf("project move: derive destination environment component: %w", keyErr)
			}
			if err := syncer.PutEnvironmentComponentWithIdentities(ctx, access.Store, access.Public, destinationLayout, destinationKey, deviceID, content, access.Identities); err != nil {
				return fmt.Errorf("project move: copy environment component: %w", err)
			}
		}
	}
	return nil
}

func copyReencryptedMoveObject(ctx context.Context, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, sourceKey, destinationKey string, sourceSize int64) error {
	if sourceSize > maxProjectMoveObjectBytes {
		return errors.New("source object exceeds the bounded project-move transfer limit")
	}
	sealed, err := readProjectMoveObject(ctx, store, sourceKey, sourceSize)
	if err != nil {
		return err
	}
	plaintext, err := decryptProjectMoveObject(identities, sourceKey, sealed)
	if err != nil {
		return fmt.Errorf("decrypt source object: %w", err)
	}
	if existing, statErr := store.Stat(ctx, destinationKey); statErr == nil {
		if existing.Size > maxProjectMoveObjectBytes {
			return errors.New("destination object exceeds the bounded project-move transfer limit")
		}
		destinationSealed, readErr := readProjectMoveObject(ctx, store, destinationKey, existing.Size)
		if readErr != nil {
			return readErr
		}
		destinationPlaintext, decryptErr := decryptProjectMoveObject(identities, destinationKey, destinationSealed)
		if decryptErr != nil || !bytes.Equal(destinationPlaintext, plaintext) {
			return errors.New("destination object conflicts with the source")
		}
		return nil
	} else if !errors.Is(statErr, remote.ErrNotFound) {
		return fmt.Errorf("inspect destination object: %w", statErr)
	}
	destinationSealed, err := crypto.Encrypt(public, destinationKey, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt destination object: %w", err)
	}
	if err := store.Put(ctx, destinationKey, bytes.NewReader(destinationSealed), int64(len(destinationSealed))); err != nil {
		return fmt.Errorf("write destination object: %w", err)
	}
	return nil
}

func readProjectMoveObject(ctx context.Context, store remote.Remote, key string, expectedSize int64) ([]byte, error) {
	if expectedSize > maxProjectMoveObjectBytes {
		return nil, errors.New("remote object exceeds the bounded project-move transfer limit")
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read remote object: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxProjectMoveObjectBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read remote object: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close remote object: %w", closeErr)
	}
	if len(data) > maxProjectMoveObjectBytes {
		return nil, errors.New("remote object exceeds the bounded project-move transfer limit")
	}
	return data, nil
}

func decryptProjectMoveObject(identities []*ecdh.PrivateKey, key string, sealed []byte) ([]byte, error) {
	var last error
	for _, identity := range identities {
		plaintext, err := crypto.Decrypt(identity, key, sealed)
		if err == nil {
			return plaintext, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no retained identity can open the remote object")
	}
	return nil, last
}

func moveObjectSuffix(prefix, key string) (string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return "", false
	}
	suffix := strings.TrimPrefix(key, prefix+"/")
	if suffix == "" || strings.ContainsRune(suffix, 0) {
		return "", false
	}
	return suffix, true
}

func checkedMoveKey(key string) (string, error) {
	if err := remote.ValidateKey(key); err != nil {
		return "", fmt.Errorf("project move: destination object key is invalid: %w", err)
	}
	return key, nil
}

func parseMoveProjectDescriptorSuffix(suffix string) (string, bool) {
	parts := strings.Split(suffix, "/")
	if len(parts) != 3 || parts[0] != "descriptors" || parts[2] != "meta" {
		return "", false
	}
	return parts[1], true
}

func parseMoveSessionDescriptorSuffix(suffix string) (string, string, bool) {
	parts := strings.Split(suffix, "/")
	if len(parts) != 5 || parts[0] != "sessions" || parts[2] != "descriptors" || parts[4] != "meta" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

func parseMoveAttachmentSuffix(suffix string) (string, string, string, bool) {
	parts := strings.Split(suffix, "/")
	if len(parts) != 6 || parts[0] != "sessions" || parts[2] != "environments" || parts[5] != "meta" && parts[5] != "000001" {
		return "", "", "", false
	}
	return parts[1], parts[3], parts[4], true
}

func moveAttachmentKey(prefix string, ref projectMoveAttachmentRef, objectName string) string {
	return prefix + "/sessions/" + ref.sessionID + "/environments/" + ref.environmentID + "/" + ref.deviceID + "/" + objectName
}

func moveLocalProjectBindings(configDir, sourceHubID, sourceProjectID, destinationHubID, destinationProjectID string) (int, error) {
	root := filepath.Join(configDir, "state", "v2", "hubs", sourceHubID, "projects", sourceProjectID)
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != sessionhub.LocalBindingFileName {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("project move: scan local bindings: %w", err)
	}
	count := 0
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return 0, fmt.Errorf("project move: read local binding: %w", readErr)
		}
		binding, parseErr := sessionhub.ParseLocalBinding(data)
		if parseErr != nil {
			return 0, fmt.Errorf("project move: local binding is invalid: %w", parseErr)
		}
		if binding.HubID != sourceHubID || binding.ProjectID != sourceProjectID {
			continue
		}
		binding.HubID = destinationHubID
		binding.ProjectID = destinationProjectID
		destinationPath, pathErr := sessionhub.LocalBindingPath(configDir, binding)
		if pathErr != nil {
			return 0, pathErr
		}
		if existing, existingErr := os.ReadFile(destinationPath); existingErr == nil {
			existingBinding, parseExistingErr := sessionhub.ParseLocalBinding(existing)
			if parseExistingErr != nil {
				return 0, fmt.Errorf("project move: destination local binding is invalid: %w", parseExistingErr)
			}
			existingBytes, marshalErr := existingBinding.MarshalBinary()
			if marshalErr != nil {
				return 0, fmt.Errorf("project move: encode destination local binding: %w", marshalErr)
			}
			newBytes, marshalErr := binding.MarshalBinary()
			if marshalErr != nil {
				return 0, fmt.Errorf("project move: encode source local binding: %w", marshalErr)
			}
			if !bytes.Equal(existingBytes, newBytes) {
				return 0, errors.New("project move: destination local binding conflicts with the source")
			}
		} else if !errors.Is(existingErr, os.ErrNotExist) {
			return 0, fmt.Errorf("project move: inspect destination local binding: %w", existingErr)
		} else if saveErr := sessionhub.SaveLocalBinding(configDir, binding); saveErr != nil {
			return 0, fmt.Errorf("project move: write destination local binding: %w", saveErr)
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return 0, fmt.Errorf("project move: remove old local binding: %w", removeErr)
		}
		count++
	}
	return count, nil
}

func writeProjectMoveJSON(w io.Writer, report projectMoveReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeProjectMoveText(w io.Writer, report projectMoveReport) error {
	_, err := fmt.Fprintf(w, "project move: %s\nproject: %s\ndestination: %s\nsource-hub: %s\ndestination-hub: %s\nsessions: %d\nobjects: %d copied=%d\nbindings: %d\n", safeListText(report.State), safeListText(report.ProjectID), safeListText(report.DestinationProject), safeListText(report.SourceHub), safeListText(report.DestinationHub), report.SessionCount, report.SourceObjects, report.CopiedObjects, report.BindingCount)
	return err
}
