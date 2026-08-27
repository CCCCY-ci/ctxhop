package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

type sessionListReport struct {
	Scope    string              `json:"scope"`
	Hub      sessionHubScope     `json:"hub"`
	Project  sessionProjectScope `json:"project"`
	Sessions []sessionListEntry  `json:"sessions"`
}

type sessionHubScope struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type sessionProjectScope struct {
	ID           string `json:"id"`
	IdentityKind string `json:"identityKind"`
}

type sessionListEntry struct {
	SessionID   string               `json:"sessionId"`
	Title       string               `json:"title"`
	CreatedAt   time.Time            `json:"createdAt,omitempty"`
	UpdatedAt   time.Time            `json:"updatedAt,omitempty"`
	Local       bool                 `json:"local"`
	RecordCount uint64               `json:"recordCount,omitempty"`
	Sources     []sessionSourceEntry `json:"sources"`
}

type sessionSourceEntry struct {
	Agent       string    `json:"agent"`
	NativeID    string    `json:"nativeId,omitempty"`
	DeviceID    string    `json:"deviceId,omitempty"`
	Local       bool      `json:"local"`
	RecordCount uint64    `json:"recordCount,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

type sessionDiscoverReport struct {
	Scope          string                 `json:"scope"`
	Hub            sessionHubScope        `json:"hub"`
	Project        sessionProjectScope    `json:"project"`
	NativeSessions []sessionDiscoverEntry `json:"nativeSessions"`
}

type sessionDiscoverEntry struct {
	SessionID string    `json:"sessionId,omitempty"`
	State     string    `json:"state"`
	Agent     string    `json:"agent"`
	NativeID  string    `json:"nativeId"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type sessionProjectionSource struct {
	agent       string
	nativeID    string
	deviceID    string
	title       string
	createdAt   time.Time
	updatedAt   time.Time
	recordCount uint64
	local       bool
}

// loadSessionRegistryForRead returns a detached local registry. A missing
// registry is normal during the v1-to-v2 compatibility period, so the caller
// receives an in-memory default Hub and no file is created by a read command.
func loadSessionRegistryForRead(configDir string, identifierKey []byte, hubID string) (sessionhub.Registry, error) {
	registry, err := sessionhub.LoadRegistry(configDir)
	if errors.Is(err, sessionhub.ErrRegistryNotFound) {
		return sessionhub.NewDefaultRegistry(identifierKey, time.Now().UTC())
	}
	if err != nil {
		return sessionhub.Registry{}, err
	}
	hub, ok := registry.DefaultHub()
	if !ok || hub.Descriptor.HubID != hubID {
		return sessionhub.Registry{}, errors.New("session: local Session Hub registry belongs to another sync domain")
	}
	return registry, nil
}

// ensureDefaultSessionRegistry creates the local default Hub exactly once.
// It is called by init, while read-only list/discover commands deliberately
// use loadSessionRegistryForRead and never create local state.
func ensureDefaultSessionRegistry(configDir string, identifierKey []byte) error {
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		return err
	}
	registry, err := sessionhub.LoadRegistry(configDir)
	if errors.Is(err, sessionhub.ErrRegistryNotFound) {
		registry, err = sessionhub.NewDefaultRegistry(identifierKey, time.Now().UTC())
		if err != nil {
			return err
		}
		return sessionhub.SaveRegistry(configDir, registry)
	}
	if err != nil {
		return err
	}
	hub, ok := registry.DefaultHub()
	if !ok || hub.Descriptor.HubID != hubID {
		return errors.New("session: local Session Hub registry belongs to another sync domain")
	}
	return nil
}

// registerPushedSessions records only the successful v1 push results in the
// local logical namespace. The v1 remote objects remain the source of truth
// for this compatibility phase; this sidecar makes the Project → Session
// relationship explicit without copying any native records.
func registerPushedSessions(configDir string, identifierKey []byte, deviceID string, identity project.Identity, pushed []pushedNativeSession) error {
	if len(pushed) == 0 {
		return nil
	}
	if err := config.ValidateDeviceID(deviceID); err != nil {
		return fmt.Errorf("session: local device identity is invalid: %w", err)
	}
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		return err
	}
	registry, err := sessionhub.LoadRegistry(configDir)
	if errors.Is(err, sessionhub.ErrRegistryNotFound) {
		registry, err = sessionhub.NewDefaultRegistry(identifierKey, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	hub, ok := registry.DefaultHub()
	if !ok || hub.Descriptor.HubID != hubID {
		return errors.New("session: local Session Hub registry belongs to another sync domain")
	}
	identityKind := sessionhub.ProjectIdentityRemote
	if identity.Kind == project.KindManual {
		identityKind = sessionhub.ProjectIdentityManual
	}
	projectRecord, err := registry.EnsureProject(identifierKey, identityKind, identity.Value, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, source := range pushed {
		if strings.TrimSpace(source.LegacySessionID) == "" {
			return errors.New("session: pushed native session has no legacy identity")
		}
		agent := sessionAgentLabel(source.Agent)
		creator := sessionhub.SessionCreator{Agent: agent, DeviceID: deviceID}
		record, err := registry.EnsureLegacySession(identifierKey, projectRecord.Descriptor.ProjectID, source.LegacySessionID, safeListText(source.Title), source.CreatedAt, creator)
		if err != nil {
			return err
		}
		if err := registry.BindNativeSession(projectRecord.Descriptor.ProjectID, record.Descriptor.SessionID, sessionhub.NativeSessionBinding{
			Agent:           agent,
			NativeSessionID: source.NativeID,
			LegacySessionID: source.LegacySessionID,
			BoundAt:         time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
		return err
	}
	return nil
}

func sessionHubAndProject(identifierKey []byte, current project.Project) (sessionHubScope, sessionProjectScope, string, error) {
	if !current.Identity.Stable() {
		return sessionHubScope{}, sessionProjectScope{}, "", errors.New("session: project identity is unstable")
	}
	hubID, err := sessionhub.DeriveHubKey(identifierKey, sessionhub.DefaultHubLogicalID)
	if err != nil {
		return sessionHubScope{}, sessionProjectScope{}, "", err
	}
	projectID, err := sessionhub.DeriveProjectKey(identifierKey, hubID, current.Identity.Value)
	if err != nil {
		return sessionHubScope{}, sessionProjectScope{}, "", err
	}
	identityKind := string(sessionhub.ProjectIdentityRemote)
	if current.Identity.Kind == project.KindManual {
		identityKind = string(sessionhub.ProjectIdentityManual)
	}
	return sessionHubScope{ID: hubID, Name: sessionhub.DefaultHubLogicalID}, sessionProjectScope{
		ID:           projectID,
		IdentityKind: identityKind,
	}, projectID, nil
}

func buildSessionList(collection listCollection, registry sessionhub.Registry) (sessionListReport, error) {
	hubScope, projectScope, v2ProjectID, err := sessionHubAndProject(collection.identifierKey, collection.current)
	if err != nil {
		return sessionListReport{}, err
	}
	if hub, ok := registry.DefaultHub(); ok {
		hubScope = sessionHubScope{ID: hub.Descriptor.HubID, Name: hub.Descriptor.Name}
	}
	builder := sessionProjectionBuilder{
		identifierKey: collection.identifierKey,
		v2ProjectID:   v2ProjectID,
		v1ProjectID:   collection.projectID,
		localDevice:   collection.localDeviceID,
		registry:      registry,
		entries:       make(map[string]*sessionListEntry),
	}
	builder.addRegisteredSessions()
	for _, group := range collection.remoteSessions {
		for _, device := range group.Devices {
			builder.addRemote(group.SessionID, device)
		}
	}
	for _, ref := range collection.localSessions {
		builder.addLocal(ref)
	}

	entries := make([]sessionListEntry, 0, len(builder.entries))
	for _, entry := range builder.entries {
		sortSessionSources(entry.Sources)
		entries = append(entries, *entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UpdatedAt.Equal(entries[j].UpdatedAt) {
			return entries[i].SessionID < entries[j].SessionID
		}
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})
	return sessionListReport{Scope: "project", Hub: hubScope, Project: projectScope, Sessions: entries}, nil
}

type sessionProjectionBuilder struct {
	identifierKey []byte
	v2ProjectID   string
	v1ProjectID   string
	localDevice   string
	registry      sessionhub.Registry
	entries       map[string]*sessionListEntry
}

func (b *sessionProjectionBuilder) addRegisteredSessions() {
	projectRecord, ok := b.registry.Project(b.v2ProjectID)
	if !ok {
		return
	}
	for _, record := range projectRecord.Sessions {
		entry := b.entry(record.Descriptor.SessionID)
		entry.Title = safeListText(record.Descriptor.Title)
		entry.CreatedAt = record.Descriptor.CreatedAt.UTC()
		for _, source := range record.Sources {
			b.addSource(record.Descriptor.SessionID, sessionProjectionSource{
				agent:    source.Agent,
				nativeID: source.NativeSessionID,
				deviceID: b.localDevice,
			})
		}
	}
}

func (b *sessionProjectionBuilder) addRemote(legacyID string, device syncer.MetadataRef) {
	source := sessionProjectionSource{
		agent:       "unknown",
		deviceID:    device.DeviceID,
		recordCount: device.Metadata.RecordCount,
	}
	if summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload); err == nil {
		source.agent = sessionAgentLabel(summary.Agent)
		source.nativeID = safeListText(summary.NativeID)
		source.title = safeListText(summary.Title)
		source.createdAt = summary.CreatedAt
		source.updatedAt = summary.UpdatedAt
	}
	b.addSource(b.sessionID(legacyID, source.agent, source.nativeID), source)
}

func (b *sessionProjectionBuilder) addLocal(ref adapter.SessionRef) {
	legacyID, err := crypto.SessionID(b.identifierKey, b.v1ProjectID, ref.NativeID)
	if err != nil {
		return
	}
	agent := sessionAgentLabel(ref.Agent)
	nativeID := safeListText(ref.NativeID)
	sessionID := b.sessionID(legacyID, agent, nativeID)
	source := sessionProjectionSource{
		agent:     agent,
		nativeID:  nativeID,
		deviceID:  b.localDevice,
		title:     safeListText(ref.Title),
		createdAt: ref.CreatedAt,
		updatedAt: ref.UpdatedAt,
		local:     true,
	}
	b.addSource(sessionID, source)
}

func (b *sessionProjectionBuilder) sessionID(legacyID, agent, nativeID string) string {
	if record, ok := b.registry.FindSessionByNative(b.v2ProjectID, agent, nativeID, legacyID); ok {
		return record.Descriptor.SessionID
	}
	logicalID, err := sessionhub.DeriveLegacySessionKey(b.identifierKey, b.v2ProjectID, legacyID)
	if err != nil {
		return ""
	}
	return logicalID
}

func (b *sessionProjectionBuilder) entry(sessionID string) *sessionListEntry {
	if sessionID == "" {
		return &sessionListEntry{}
	}
	entry := b.entries[sessionID]
	if entry == nil {
		entry = &sessionListEntry{SessionID: sessionID, Sources: []sessionSourceEntry{}}
		b.entries[sessionID] = entry
	}
	return entry
}

func (b *sessionProjectionBuilder) addSource(sessionID string, source sessionProjectionSource) {
	if sessionID == "" {
		return
	}
	entry := b.entry(sessionID)
	if source.title != "" && (entry.Title == "" || entry.Title == "encrypted session metadata" || source.updatedAt.After(entry.UpdatedAt)) {
		entry.Title = source.title
	}
	if entry.Title == "" {
		entry.Title = "encrypted session metadata"
	}
	if !source.createdAt.IsZero() && (entry.CreatedAt.IsZero() || source.createdAt.Before(entry.CreatedAt)) {
		entry.CreatedAt = source.createdAt.UTC()
	}
	if source.updatedAt.After(entry.UpdatedAt) {
		entry.UpdatedAt = source.updatedAt.UTC()
	}
	entry.Local = entry.Local || source.local
	if source.recordCount > entry.RecordCount {
		entry.RecordCount = source.recordCount
	}

	key := source.agent + "\x00" + source.nativeID + "\x00" + source.deviceID
	for index := range entry.Sources {
		existing := &entry.Sources[index]
		if existing.Agent+"\x00"+existing.NativeID+"\x00"+existing.DeviceID != key {
			continue
		}
		existing.Local = existing.Local || source.local
		if source.recordCount > existing.RecordCount {
			existing.RecordCount = source.recordCount
		}
		if source.updatedAt.After(existing.UpdatedAt) {
			if !source.createdAt.IsZero() {
				existing.CreatedAt = source.createdAt.UTC()
			}
			existing.UpdatedAt = source.updatedAt.UTC()
		}
		return
	}
	entry.Sources = append(entry.Sources, sessionSourceEntry{
		Agent:       source.agent,
		NativeID:    source.nativeID,
		DeviceID:    source.deviceID,
		Local:       source.local,
		RecordCount: source.recordCount,
		CreatedAt:   source.createdAt.UTC(),
		UpdatedAt:   source.updatedAt.UTC(),
	})
}

func sessionAgentLabel(agent string) string {
	if strings.TrimSpace(agent) == "" {
		return "unknown"
	}
	return safeListText(agent)
}

func sortSessionSources(sources []sessionSourceEntry) {
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Agent != sources[j].Agent {
			return sources[i].Agent < sources[j].Agent
		}
		if sources[i].NativeID != sources[j].NativeID {
			return sources[i].NativeID < sources[j].NativeID
		}
		return sources[i].DeviceID < sources[j].DeviceID
	})
}

func buildSessionDiscoverReport(identifierKey []byte, current project.Project, refs []adapter.SessionRef, registry sessionhub.Registry) (sessionDiscoverReport, error) {
	hubScope, projectScope, v2ProjectID, err := sessionHubAndProject(identifierKey, current)
	if err != nil {
		return sessionDiscoverReport{}, err
	}
	if hub, ok := registry.DefaultHub(); ok {
		hubScope = sessionHubScope{ID: hub.Descriptor.HubID, Name: hub.Descriptor.Name}
	}
	entries := make([]sessionDiscoverEntry, 0, len(refs))
	v1ProjectID, err := crypto.ProjectID(identifierKey, current.Identity.Value)
	if err != nil {
		return sessionDiscoverReport{}, err
	}
	for _, ref := range refs {
		agent := sessionAgentLabel(ref.Agent)
		nativeID := safeListText(ref.NativeID)
		legacyID, err := crypto.SessionID(identifierKey, v1ProjectID, ref.NativeID)
		if err != nil {
			continue
		}
		entry := sessionDiscoverEntry{
			State:     "unbound",
			Agent:     agent,
			NativeID:  nativeID,
			Title:     safeListText(ref.Title),
			CreatedAt: ref.CreatedAt.UTC(),
			UpdatedAt: ref.UpdatedAt.UTC(),
		}
		if record, ok := registry.FindSessionByNative(v2ProjectID, agent, nativeID, legacyID); ok {
			entry.State = "bound"
			entry.SessionID = record.Descriptor.SessionID
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Agent != entries[j].Agent {
			return entries[i].Agent < entries[j].Agent
		}
		return entries[i].NativeID < entries[j].NativeID
	})
	return sessionDiscoverReport{Scope: "project", Hub: hubScope, Project: projectScope, NativeSessions: entries}, nil
}

func sessionDeviceLabel(source sessionSourceEntry) string {
	if source.DeviceID == "" {
		return "unknown"
	}
	if source.Local {
		return "local"
	}
	return "device-" + source.DeviceID
}

func sessionSourceLabel(source sessionSourceEntry) string {
	label := source.Agent
	if source.NativeID != "" {
		label += ":" + source.NativeID
	}
	return label + "@" + sessionDeviceLabel(source)
}

func writeSessionListText(w io.Writer, report sessionListReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "hub: %s (%s)\n", safeListText(report.Hub.Name), safeListText(report.Hub.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "project: %s\n", safeListText(report.Project.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "sessions: %d\n", len(report.Sessions)); err != nil {
		return err
	}
	for _, entry := range report.Sessions {
		if _, err := fmt.Fprintf(w, "- %s", safeListText(entry.SessionID)); err != nil {
			return err
		}
		if entry.Title != "" {
			if _, err := fmt.Fprintf(w, " title=%q", safeListText(entry.Title)); err != nil {
				return err
			}
		}
		if !entry.UpdatedAt.IsZero() {
			if _, err := fmt.Fprintf(w, " updated=%s", entry.UpdatedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, " local=%t sources=", entry.Local); err != nil {
			return err
		}
		labels := make([]string, 0, len(entry.Sources))
		for _, source := range entry.Sources {
			labels = append(labels, sessionSourceLabel(source))
		}
		if _, err := fmt.Fprintf(w, "%s\n", strings.Join(labels, ",")); err != nil {
			return err
		}
	}
	return nil
}

func writeSessionDiscoverText(w io.Writer, report sessionDiscoverReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "hub: %s (%s)\n", safeListText(report.Hub.Name), safeListText(report.Hub.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "project: %s\n", safeListText(report.Project.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "native-sessions: %d\n", len(report.NativeSessions)); err != nil {
		return err
	}
	for _, entry := range report.NativeSessions {
		if _, err := fmt.Fprintf(w, "- %s/%s state=%s", safeListText(entry.Agent), safeListText(entry.NativeID), safeListText(entry.State)); err != nil {
			return err
		}
		if entry.SessionID != "" {
			if _, err := fmt.Fprintf(w, " session=%s", safeListText(entry.SessionID)); err != nil {
				return err
			}
		}
		if entry.Title != "" {
			if _, err := fmt.Fprintf(w, " title=%q", safeListText(entry.Title)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}
