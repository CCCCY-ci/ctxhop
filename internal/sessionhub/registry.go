package sessionhub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// RegistryVersion is the version of the local, metadata-only Session Hub
// registry. The registry is a rebuildable index: it never contains a native
// session body or an encrypted remote payload.
const RegistryVersion = 1

// RegistryFileName is deliberately separate from config.json. The existing
// v1 configuration remains the owner of storage, device and project-policy
// settings; this file only holds the v2 logical namespace and local bindings.
const RegistryFileName = "session-hub-registry.json"

var (
	// ErrRegistryNotFound reports that this installation has not written a
	// local v2 registry yet. Callers may build a read-only compatibility view
	// from v1 metadata in this case.
	ErrRegistryNotFound = errors.New("sessionhub: local registry does not exist")

	// ErrNativeSessionAlreadyBound reports an attempt to make one local native
	// session belong to two logical Sessions in the same Project.
	ErrNativeSessionAlreadyBound = errors.New("sessionhub: native session is already bound")
)

// Registry is the local hierarchical index:
//
//	Domain (the config directory)
//	└── Hub
//	    └── Project
//	        └── Session

// It intentionally stores nested records instead of a Domain-wide flat
// session table. That keeps the same scope boundaries in memory, on disk, and
// in the future remote namespace.
type Registry struct {
	Version int         `json:"version"`
	Hubs    []HubRecord `json:"hubs"`
}

// HubRecord is one locally known SessionHub and its Projects.
type HubRecord struct {
	Descriptor HubDescriptor   `json:"descriptor"`
	Projects   []ProjectRecord `json:"projects"`
}

// ProjectRecord is one Project and its logical Sessions.
type ProjectRecord struct {
	Descriptor ProjectDescriptor `json:"descriptor"`
	Sessions   []SessionRecord   `json:"sessions"`
}

// SessionRecord is a logical Session and the local source bindings known for
// it. A source binding is metadata only; it is not a copy of the Agent's
// native conversation.
type SessionRecord struct {
	Descriptor SessionDescriptor      `json:"descriptor"`
	Sources    []NativeSessionBinding `json:"sources,omitempty"`
}

// NativeSessionBinding maps one Agent-native session to a logical Session.
// NativeSessionID and LegacySessionID are local metadata and are never used
// as remote object path segments.
type NativeSessionBinding struct {
	Agent           string    `json:"agent"`
	NativeSessionID string    `json:"nativeSessionId"`
	LegacySessionID string    `json:"legacySessionId,omitempty"`
	BoundAt         time.Time `json:"boundAt"`
}

// MarshalBinary encodes the registry in the same compact, strict format used
// by the other Session Hub descriptors.
func (r Registry) MarshalBinary() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	copyRegistry := cloneRegistry(r)
	sortRegistry(&copyRegistry)
	return marshalCompact(copyRegistry, "local registry", maxDescriptorBytes)
}

// ParseRegistry strictly decodes a local registry. Unknown fields and
// trailing JSON are rejected so a newer writer cannot be mistaken for a
// complete older registry.
func ParseRegistry(data []byte) (Registry, error) {
	var registry Registry
	if err := decodeCompact(data, &registry, "local registry", maxDescriptorBytes); err != nil {
		return Registry{}, err
	}
	if err := registry.Validate(); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

// NewDefaultRegistry creates the initial local registry for a Domain. The
// default Hub logical identity is stable, while the descriptor timestamp is
// only display metadata and is persisted by SaveRegistry.
func NewDefaultRegistry(identifierKey []byte, now time.Time) (Registry, error) {
	hubID, err := DeriveHubKey(identifierKey, DefaultHubLogicalID)
	if err != nil {
		return Registry{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	hub := HubRecord{Descriptor: HubDescriptor{
		Version:   ModelVersion,
		HubID:     hubID,
		Name:      DefaultHubLogicalID,
		CreatedAt: now.UTC().Round(0),
		Lifecycle: HubActive,
	}}
	registry := Registry{Version: RegistryVersion, Hubs: []HubRecord{hub}}
	if err := registry.Validate(); err != nil {
		return Registry{}, err
	}
	return registry, nil
}

// LoadRegistry reads the local metadata registry without creating it.
func LoadRegistry(dir string) (Registry, error) {
	if strings.TrimSpace(dir) == "" {
		return Registry{}, errors.New("sessionhub: registry directory is required")
	}
	data, err := os.ReadFile(filepath.Join(dir, RegistryFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Registry{}, ErrRegistryNotFound
	}
	if err != nil {
		return Registry{}, fmt.Errorf("sessionhub: read local registry: %w", err)
	}
	registry, err := ParseRegistry(data)
	if err != nil {
		return Registry{}, err
	}
	return registry, nil
}

// SaveRegistry atomically writes the local metadata registry.
func SaveRegistry(dir string, registry Registry) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("sessionhub: registry directory is required")
	}
	data, err := registry.MarshalBinary()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sessionhub: create registry directory: %w", err)
	}
	if err := atomicfile.WriteBytes(filepath.Join(dir, RegistryFileName), data); err != nil {
		return fmt.Errorf("sessionhub: write local registry: %w", err)
	}
	return nil
}

// Validate checks the complete local hierarchy and rejects duplicate or
// conflicting bindings. It does not contact a Remote.
func (r Registry) Validate() error {
	if r.Version != RegistryVersion {
		if r.Version > RegistryVersion {
			return fmt.Errorf("%w: registry version %d", ErrUnsupportedVersion, r.Version)
		}
		return fmt.Errorf("%w: registry version %d", ErrInvalidModel, r.Version)
	}
	if len(r.Hubs) == 0 {
		return fmt.Errorf("%w: registry has no hubs", ErrInvalidModel)
	}
	hubIDs := make(map[string]struct{}, len(r.Hubs))
	for _, hub := range r.Hubs {
		if err := hub.Descriptor.Validate(); err != nil {
			return fmt.Errorf("%w: hub: %v", ErrInvalidModel, err)
		}
		if _, exists := hubIDs[hub.Descriptor.HubID]; exists {
			return fmt.Errorf("%w: duplicate hub %q", ErrInvalidModel, hub.Descriptor.HubID)
		}
		hubIDs[hub.Descriptor.HubID] = struct{}{}
		projectIDs := make(map[string]struct{}, len(hub.Projects))
		for _, project := range hub.Projects {
			if err := project.Descriptor.Validate(); err != nil {
				return fmt.Errorf("%w: project: %v", ErrInvalidModel, err)
			}
			if project.Descriptor.HubID != hub.Descriptor.HubID {
				return fmt.Errorf("%w: project %q has the wrong hub", ErrInvalidModel, project.Descriptor.ProjectID)
			}
			if _, exists := projectIDs[project.Descriptor.ProjectID]; exists {
				return fmt.Errorf("%w: duplicate project %q", ErrInvalidModel, project.Descriptor.ProjectID)
			}
			projectIDs[project.Descriptor.ProjectID] = struct{}{}
			if err := validateSessionRecords(project); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSessionRecords(project ProjectRecord) error {
	sessionIDs := make(map[string]struct{}, len(project.Sessions))
	boundSources := make(map[string]string)
	for _, session := range project.Sessions {
		if err := session.Descriptor.Validate(); err != nil {
			return fmt.Errorf("%w: session: %v", ErrInvalidModel, err)
		}
		if session.Descriptor.ProjectID != project.Descriptor.ProjectID {
			return fmt.Errorf("%w: session %q has the wrong project", ErrInvalidModel, session.Descriptor.SessionID)
		}
		if _, exists := sessionIDs[session.Descriptor.SessionID]; exists {
			return fmt.Errorf("%w: duplicate session %q", ErrInvalidModel, session.Descriptor.SessionID)
		}
		sessionIDs[session.Descriptor.SessionID] = struct{}{}
		for _, source := range session.Sources {
			if err := source.Validate(); err != nil {
				return fmt.Errorf("%w: source in session %q: %v", ErrInvalidModel, session.Descriptor.SessionID, err)
			}
			key := source.Agent + "\x00" + source.NativeSessionID
			if previous, exists := boundSources[key]; exists && previous != session.Descriptor.SessionID {
				return fmt.Errorf("%w: source belongs to sessions %q and %q", ErrInvalidModel, previous, session.Descriptor.SessionID)
			}
			boundSources[key] = session.Descriptor.SessionID
		}
	}
	return nil
}

// Validate checks a local source binding.
func (b NativeSessionBinding) Validate() error {
	if err := validateAgent(b.Agent); err != nil {
		return fmt.Errorf("%w: source agent", err)
	}
	if err := validateNativeSessionID(b.NativeSessionID); err != nil {
		return fmt.Errorf("%w: native session id", err)
	}
	if b.LegacySessionID != "" {
		if err := validateOpaqueID(b.LegacySessionID); err != nil {
			return fmt.Errorf("%w: legacy session id", err)
		}
	}
	if b.BoundAt.IsZero() {
		return fmt.Errorf("%w: binding timestamp", ErrInvalidModel)
	}
	return nil
}

// EnsureProject returns the default-Hub Project for a stable project
// identity, creating it in memory when it is new. The caller decides when to
// persist the returned registry.
func (r *Registry) EnsureProject(identifierKey []byte, identityKind ProjectIdentityKind, identity string, now time.Time) (ProjectRecord, error) {
	if r == nil {
		return ProjectRecord{}, errors.New("sessionhub: registry is required")
	}
	if err := r.Validate(); err != nil {
		return ProjectRecord{}, err
	}
	if strings.TrimSpace(identity) == "" || !utf8.ValidString(identity) || strings.ContainsRune(identity, 0) {
		return ProjectRecord{}, fmt.Errorf("%w: project identity", ErrInvalidIdentity)
	}
	if identityKind != ProjectIdentityRemote && identityKind != ProjectIdentityManual {
		return ProjectRecord{}, fmt.Errorf("%w: project identity kind", ErrInvalidIdentity)
	}
	hubIndex := r.defaultHubIndex()
	if hubIndex < 0 {
		return ProjectRecord{}, fmt.Errorf("%w: default hub is unavailable", ErrInvalidModel)
	}
	hubID := r.Hubs[hubIndex].Descriptor.HubID
	projectID, err := DeriveProjectKey(identifierKey, hubID, identity)
	if err != nil {
		return ProjectRecord{}, err
	}
	if index := projectIndex(r.Hubs[hubIndex], projectID); index >= 0 {
		return cloneProjectRecord(r.Hubs[hubIndex].Projects[index]), nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	project := ProjectRecord{Descriptor: ProjectDescriptor{
		Version:             ModelVersion,
		HubID:               hubID,
		ProjectID:           projectID,
		IdentityKind:        identityKind,
		IdentityFingerprint: projectID,
		CreatedAt:           now.UTC().Round(0),
		Lifecycle:           ProjectActive,
	}}
	r.Hubs[hubIndex].Projects = append(r.Hubs[hubIndex].Projects, project)
	sortProjects(r.Hubs[hubIndex].Projects)
	return cloneProjectRecord(project), nil
}

// EnsureLegacySession creates or updates the deterministic read-only mapping
// from a v1 remote session group to a v2 logical Session. This mapping is a
// compatibility projection, not a content merge: it only treats the existing
// v1 group identity as authoritative.
func (r *Registry) EnsureLegacySession(identifierKey []byte, projectID, legacySessionID, title string, createdAt time.Time, creator SessionCreator) (SessionRecord, error) {
	if r == nil {
		return SessionRecord{}, errors.New("sessionhub: registry is required")
	}
	if err := validateOpaqueID(projectID); err != nil {
		return SessionRecord{}, fmt.Errorf("%w: project id", ErrInvalidIdentity)
	}
	logicalID, err := DeriveLegacySessionKey(identifierKey, projectID, legacySessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := validateSessionCreator(creator); err != nil {
		return SessionRecord{}, err
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	projectIndex, hubIndex := r.findProject(projectID)
	if hubIndex < 0 || projectIndex < 0 {
		return SessionRecord{}, fmt.Errorf("%w: project %q is not registered", ErrInvalidModel, projectID)
	}
	project := &r.Hubs[hubIndex].Projects[projectIndex]
	if index := sessionIndex(*project, logicalID); index >= 0 {
		record := &project.Sessions[index]
		if strings.TrimSpace(title) != "" {
			record.Descriptor.Title = title
		}
		if createdAt.Before(record.Descriptor.CreatedAt) {
			record.Descriptor.CreatedAt = createdAt.UTC().Round(0)
		}
		return cloneSessionRecord(*record), nil
	}
	session := SessionRecord{Descriptor: SessionDescriptor{
		Version:   ModelVersion,
		SessionID: logicalID,
		ProjectID: projectID,
		Title:     title,
		CreatedAt: createdAt.UTC().Round(0),
		CreatedBy: creator,
		Lifecycle: SessionActive,
	}}
	project.Sessions = append(project.Sessions, session)
	sortSessions(project.Sessions)
	return cloneSessionRecord(session), nil
}

// BindNativeSession attaches a local Agent session to an existing logical
// Session. It never reads or changes the Agent's native file.
func (r *Registry) BindNativeSession(projectID, sessionID string, binding NativeSessionBinding) error {
	if r == nil {
		return errors.New("sessionhub: registry is required")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	projectIndex, hubIndex := r.findProject(projectID)
	if hubIndex < 0 || projectIndex < 0 {
		return fmt.Errorf("%w: project %q is not registered", ErrInvalidModel, projectID)
	}
	project := &r.Hubs[hubIndex].Projects[projectIndex]
	sessionIndex := sessionIndex(*project, sessionID)
	if sessionIndex < 0 {
		return fmt.Errorf("%w: session %q is not registered", ErrInvalidModel, sessionID)
	}
	for projectSessionIndex := range project.Sessions {
		for sourceIndex := range project.Sessions[projectSessionIndex].Sources {
			existing := project.Sessions[projectSessionIndex].Sources[sourceIndex]
			if existing.Agent != binding.Agent || existing.NativeSessionID != binding.NativeSessionID {
				continue
			}
			if projectSessionIndex != sessionIndex {
				return fmt.Errorf("%w: %s/%s", ErrNativeSessionAlreadyBound, binding.Agent, binding.NativeSessionID)
			}
			project.Sessions[projectSessionIndex].Sources[sourceIndex] = binding
			return nil
		}
	}
	project.Sessions[sessionIndex].Sources = append(project.Sessions[sessionIndex].Sources, binding)
	sortNativeBindings(project.Sessions[sessionIndex].Sources)
	return nil
}

// FindSessionByNative locates an explicit local binding in one Project. The
// legacy session ID is an optional fallback used by compatibility views when
// a source has not yet been fully described.
func (r Registry) FindSessionByNative(projectID, agent, nativeSessionID, legacySessionID string) (SessionRecord, bool) {
	projectIndex, hubIndex := r.findProject(projectID)
	if hubIndex < 0 || projectIndex < 0 {
		return SessionRecord{}, false
	}
	project := r.Hubs[hubIndex].Projects[projectIndex]
	for _, session := range project.Sessions {
		for _, source := range session.Sources {
			if source.Agent == agent && source.NativeSessionID == nativeSessionID {
				return cloneSessionRecord(session), true
			}
		}
	}
	if legacySessionID != "" && (agent == "" || agent == "unknown") && nativeSessionID == "" {
		for _, session := range project.Sessions {
			for _, source := range session.Sources {
				if source.LegacySessionID == legacySessionID {
					return cloneSessionRecord(session), true
				}
			}
		}
	}
	return SessionRecord{}, false
}

// Project returns a detached ProjectRecord by keyed Project ID.
func (r Registry) Project(projectID string) (ProjectRecord, bool) {
	projectIndex, hubIndex := r.findProject(projectID)
	if hubIndex < 0 || projectIndex < 0 {
		return ProjectRecord{}, false
	}
	return cloneProjectRecord(r.Hubs[hubIndex].Projects[projectIndex]), true
}

// DefaultHub returns the detached default Hub. Older local registries may
// contain additional hubs later, but Phase 1 only creates the reserved one.
func (r Registry) DefaultHub() (HubRecord, bool) {
	index := r.defaultHubIndex()
	if index < 0 {
		return HubRecord{}, false
	}
	return cloneHubRecord(r.Hubs[index]), true
}

func (r Registry) defaultHubIndex() int {
	for index, hub := range r.Hubs {
		if hub.Descriptor.Name == DefaultHubLogicalID {
			return index
		}
	}
	if len(r.Hubs) == 1 {
		return 0
	}
	return -1
}

func (r Registry) findProject(projectID string) (foundProjectIndex, foundHubIndex int) {
	for hubIndex, hub := range r.Hubs {
		if index := projectIndex(hub, projectID); index >= 0 {
			return index, hubIndex
		}
	}
	return -1, -1
}

func projectIndex(hub HubRecord, projectID string) int {
	for index, project := range hub.Projects {
		if project.Descriptor.ProjectID == projectID {
			return index
		}
	}
	return -1
}

func sessionIndex(project ProjectRecord, sessionID string) int {
	for index, session := range project.Sessions {
		if session.Descriptor.SessionID == sessionID {
			return index
		}
	}
	return -1
}

func sortProjects(projects []ProjectRecord) {
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Descriptor.ProjectID < projects[j].Descriptor.ProjectID
	})
}

func sortSessions(sessions []SessionRecord) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Descriptor.SessionID < sessions[j].Descriptor.SessionID
	})
}

func sortNativeBindings(bindings []NativeSessionBinding) {
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Agent != bindings[j].Agent {
			return bindings[i].Agent < bindings[j].Agent
		}
		return bindings[i].NativeSessionID < bindings[j].NativeSessionID
	})
}

func cloneHubRecord(value HubRecord) HubRecord {
	projects := value.Projects
	value.Projects = make([]ProjectRecord, len(projects))
	for index, project := range projects {
		value.Projects[index] = cloneProjectRecord(project)
	}
	return value
}

func cloneProjectRecord(value ProjectRecord) ProjectRecord {
	sessions := value.Sessions
	value.Sessions = make([]SessionRecord, len(sessions))
	for index, session := range sessions {
		value.Sessions[index] = cloneSessionRecord(session)
	}
	return value
}

func cloneSessionRecord(value SessionRecord) SessionRecord {
	value.Sources = append([]NativeSessionBinding(nil), value.Sources...)
	return value
}

func cloneRegistry(value Registry) Registry {
	hubs := value.Hubs
	value.Hubs = make([]HubRecord, len(hubs))
	for index, hub := range hubs {
		value.Hubs[index] = cloneHubRecord(hub)
	}
	return value
}

func sortRegistry(registry *Registry) {
	if registry == nil {
		return
	}
	sort.Slice(registry.Hubs, func(i, j int) bool {
		return registry.Hubs[i].Descriptor.HubID < registry.Hubs[j].Descriptor.HubID
	})
	for hubIndex := range registry.Hubs {
		sortProjects(registry.Hubs[hubIndex].Projects)
		for projectIndex := range registry.Hubs[hubIndex].Projects {
			sortSessions(registry.Hubs[hubIndex].Projects[projectIndex].Sessions)
			for sessionIndex := range registry.Hubs[hubIndex].Projects[projectIndex].Sessions {
				sortNativeBindings(registry.Hubs[hubIndex].Projects[projectIndex].Sessions[sessionIndex].Sources)
			}
		}
	}
}
