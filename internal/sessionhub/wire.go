package sessionhub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

type hubWire struct {
	Version   int          `json:"version"`
	HubID     string       `json:"hubId"`
	Name      string       `json:"name"`
	CreatedAt string       `json:"createdAt"`
	Lifecycle HubLifecycle `json:"lifecycle"`
}

type projectWire struct {
	Version             int                 `json:"version"`
	HubID               string              `json:"hubId"`
	ProjectID           string              `json:"projectId"`
	IdentityKind        ProjectIdentityKind `json:"identityKind"`
	IdentityFingerprint string              `json:"identityFingerprint"`
	CreatedAt           string              `json:"createdAt"`
	Lifecycle           ProjectLifecycle    `json:"lifecycle"`
}

type sessionCreatorWire struct {
	Agent    string `json:"agent"`
	DeviceID string `json:"deviceId"`
}

type sessionWire struct {
	Version   int                `json:"version"`
	SessionID string             `json:"sessionId"`
	ProjectID string             `json:"projectId"`
	Title     string             `json:"title"`
	CreatedAt string             `json:"createdAt"`
	CreatedBy sessionCreatorWire `json:"createdBy"`
	Lifecycle SessionLifecycle   `json:"lifecycle"`
}

type replicaWire struct {
	Version   int           `json:"version"`
	ReplicaID string        `json:"replicaId"`
	SessionID string        `json:"sessionId"`
	Source    NativeSource  `json:"source"`
	Origin    ReplicaOrigin `json:"origin"`
	CreatedAt string        `json:"createdAt"`
}

type tipWire struct {
	Version     int    `json:"version"`
	ReplicaID   string `json:"replicaId"`
	RecordCount uint64 `json:"recordCount"`
	ShardCount  uint64 `json:"shardCount"`
	LastShard   uint64 `json:"lastShard"`
	HeadDigest  string `json:"headDigest"`
	UpdatedAt   string `json:"updatedAt"`
}

type contributionWire struct {
	Version         int                `json:"version"`
	ContributionID  string             `json:"contributionId"`
	SessionID       string             `json:"sessionId"`
	Source          ContributionSource `json:"source"`
	Parents         []string           `json:"parents"`
	Ranges          []RangeRef         `json:"ranges"`
	EnvironmentRefs []string           `json:"environmentRefs"`
	CreatedAt       string             `json:"createdAt"`
}

type contributionIdentityWire struct {
	Version         int                `json:"version"`
	SessionID       string             `json:"sessionId"`
	Source          ContributionSource `json:"source"`
	Parents         []string           `json:"parents"`
	Ranges          []RangeRef         `json:"ranges"`
	EnvironmentRefs []string           `json:"environmentRefs"`
}

type environmentWire struct {
	Version                int                       `json:"version"`
	EnvironmentID          string                    `json:"environmentId"`
	SessionID              string                    `json:"sessionId"`
	SourceAgent            string                    `json:"sourceAgent"`
	ObservedAtContribution string                    `json:"observedAtContribution"`
	Components             []EnvironmentComponentRef `json:"components"`
}

// MarshalBinary encodes a Hub descriptor as compact, deterministic JSON.
func (h HubDescriptor) MarshalBinary() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return marshalCompact(hubWire{
		Version:   h.Version,
		HubID:     h.HubID,
		Name:      h.Name,
		CreatedAt: formatTime(h.CreatedAt),
		Lifecycle: h.Lifecycle,
	}, "hub descriptor", maxDescriptorBytes)
}

// ParseHubDescriptor strictly decodes a Hub descriptor from an untrusted
// envelope.
func ParseHubDescriptor(data []byte) (HubDescriptor, error) {
	var wire hubWire
	if err := decodeCompact(data, &wire, "hub descriptor", maxDescriptorBytes); err != nil {
		return HubDescriptor{}, err
	}
	createdAt, err := parseTime(wire.CreatedAt)
	if err != nil {
		return HubDescriptor{}, fmt.Errorf("%w: hub timestamp", ErrInvalidEnvelope)
	}
	hub := HubDescriptor{
		Version:   wire.Version,
		HubID:     wire.HubID,
		Name:      wire.Name,
		CreatedAt: createdAt,
		Lifecycle: wire.Lifecycle,
	}
	if err := hub.Validate(); err != nil {
		return HubDescriptor{}, err
	}
	return hub, nil
}

// MarshalBinary encodes a Project descriptor as compact, deterministic JSON.
func (p ProjectDescriptor) MarshalBinary() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return marshalCompact(projectWire{
		Version:             p.Version,
		HubID:               p.HubID,
		ProjectID:           p.ProjectID,
		IdentityKind:        p.IdentityKind,
		IdentityFingerprint: p.IdentityFingerprint,
		CreatedAt:           formatTime(p.CreatedAt),
		Lifecycle:           p.Lifecycle,
	}, "project descriptor", maxDescriptorBytes)
}

// ParseProjectDescriptor strictly decodes a Project descriptor.
func ParseProjectDescriptor(data []byte) (ProjectDescriptor, error) {
	var wire projectWire
	if err := decodeCompact(data, &wire, "project descriptor", maxDescriptorBytes); err != nil {
		return ProjectDescriptor{}, err
	}
	createdAt, err := parseTime(wire.CreatedAt)
	if err != nil {
		return ProjectDescriptor{}, fmt.Errorf("%w: project timestamp", ErrInvalidEnvelope)
	}
	project := ProjectDescriptor{
		Version:             wire.Version,
		HubID:               wire.HubID,
		ProjectID:           wire.ProjectID,
		IdentityKind:        wire.IdentityKind,
		IdentityFingerprint: wire.IdentityFingerprint,
		CreatedAt:           createdAt,
		Lifecycle:           wire.Lifecycle,
	}
	if err := project.Validate(); err != nil {
		return ProjectDescriptor{}, err
	}
	return project, nil
}

// MarshalBinary encodes a logical Session descriptor as compact,
// deterministic JSON.
func (s SessionDescriptor) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return marshalCompact(sessionWire{
		Version:   s.Version,
		SessionID: s.SessionID,
		ProjectID: s.ProjectID,
		Title:     s.Title,
		CreatedAt: formatTime(s.CreatedAt),
		CreatedBy: sessionCreatorWire{
			Agent:    s.CreatedBy.Agent,
			DeviceID: s.CreatedBy.DeviceID,
		},
		Lifecycle: s.Lifecycle,
	}, "session descriptor", maxDescriptorBytes)
}

// ParseSessionDescriptor strictly decodes a logical Session descriptor.
func ParseSessionDescriptor(data []byte) (SessionDescriptor, error) {
	var wire sessionWire
	if err := decodeCompact(data, &wire, "session descriptor", maxDescriptorBytes); err != nil {
		return SessionDescriptor{}, err
	}
	createdAt, err := parseTime(wire.CreatedAt)
	if err != nil {
		return SessionDescriptor{}, fmt.Errorf("%w: session timestamp", ErrInvalidEnvelope)
	}
	session := SessionDescriptor{
		Version:   wire.Version,
		SessionID: wire.SessionID,
		ProjectID: wire.ProjectID,
		Title:     wire.Title,
		CreatedAt: createdAt,
		CreatedBy: SessionCreator{
			Agent:    wire.CreatedBy.Agent,
			DeviceID: wire.CreatedBy.DeviceID,
		},
		Lifecycle: wire.Lifecycle,
	}
	if err := session.Validate(); err != nil {
		return SessionDescriptor{}, err
	}
	return session, nil
}

// MarshalBinary encodes a NativeReplica descriptor as compact,
// deterministic JSON.
func (r NativeReplicaDescriptor) MarshalBinary() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	origin := r.Origin
	origin.BaseHeads = sortedStrings(r.Origin.BaseHeads)
	return marshalCompact(replicaWire{
		Version:   r.Version,
		ReplicaID: r.ReplicaID,
		SessionID: r.SessionID,
		Source:    r.Source,
		Origin:    origin,
		CreatedAt: formatTime(r.CreatedAt),
	}, "replica descriptor", maxDescriptorBytes)
}

// ParseNativeReplicaDescriptor strictly decodes a NativeReplica descriptor.
func ParseNativeReplicaDescriptor(data []byte) (NativeReplicaDescriptor, error) {
	var wire replicaWire
	if err := decodeCompact(data, &wire, "replica descriptor", maxDescriptorBytes); err != nil {
		return NativeReplicaDescriptor{}, err
	}
	createdAt, err := parseTime(wire.CreatedAt)
	if err != nil {
		return NativeReplicaDescriptor{}, fmt.Errorf("%w: replica timestamp", ErrInvalidEnvelope)
	}
	replica := NativeReplicaDescriptor{
		Version:   wire.Version,
		ReplicaID: wire.ReplicaID,
		SessionID: wire.SessionID,
		Source:    wire.Source,
		Origin:    wire.Origin,
		CreatedAt: createdAt,
	}
	if err := replica.Validate(); err != nil {
		return NativeReplicaDescriptor{}, err
	}
	return replica, nil
}

// MarshalBinary encodes a Replica tip as compact, deterministic JSON.
func (t ReplicaTip) MarshalBinary() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return marshalCompact(tipWire{
		Version:     t.Version,
		ReplicaID:   t.ReplicaID,
		RecordCount: t.RecordCount,
		ShardCount:  t.ShardCount,
		LastShard:   t.LastShard,
		HeadDigest:  t.HeadDigest,
		UpdatedAt:   formatTime(t.UpdatedAt),
	}, "replica tip", maxDescriptorBytes)
}

// ParseReplicaTip strictly decodes a Replica tip.
func ParseReplicaTip(data []byte) (ReplicaTip, error) {
	var wire tipWire
	if err := decodeCompact(data, &wire, "replica tip", maxDescriptorBytes); err != nil {
		return ReplicaTip{}, err
	}
	updatedAt, err := parseTime(wire.UpdatedAt)
	if err != nil {
		return ReplicaTip{}, fmt.Errorf("%w: tip timestamp", ErrInvalidEnvelope)
	}
	tip := ReplicaTip{
		Version:     wire.Version,
		ReplicaID:   wire.ReplicaID,
		RecordCount: wire.RecordCount,
		ShardCount:  wire.ShardCount,
		LastShard:   wire.LastShard,
		HeadDigest:  wire.HeadDigest,
		UpdatedAt:   updatedAt,
	}
	if err := tip.Validate(); err != nil {
		return ReplicaTip{}, err
	}
	return tip, nil
}

// MarshalBinary encodes an immutable Contribution as compact, deterministic
// JSON. List fields are written in canonical order without mutating c.
func (c Contribution) MarshalBinary() ([]byte, error) {
	canonical, err := c.Canonical()
	if err != nil {
		return nil, err
	}
	if err := canonical.Validate(); err != nil {
		return nil, err
	}
	return marshalCompact(contributionWire{
		Version:         canonical.Version,
		ContributionID:  canonical.ContributionID,
		SessionID:       canonical.SessionID,
		Source:          canonical.Source,
		Parents:         canonical.Parents,
		Ranges:          canonical.Ranges,
		EnvironmentRefs: canonical.EnvironmentRefs,
		CreatedAt:       formatTime(canonical.CreatedAt),
	}, "contribution", maxContributionSize)
}

// CanonicalBytes is an explicit alias for the deterministic Contribution
// envelope used by identity and fixture tests.
func (c Contribution) CanonicalBytes() ([]byte, error) {
	return c.MarshalBinary()
}

// ParseContribution strictly decodes an immutable Contribution.
func ParseContribution(data []byte) (Contribution, error) {
	var wire contributionWire
	if err := decodeCompact(data, &wire, "contribution", maxContributionSize); err != nil {
		return Contribution{}, err
	}
	createdAt, err := parseTime(wire.CreatedAt)
	if err != nil {
		return Contribution{}, fmt.Errorf("%w: contribution timestamp", ErrInvalidEnvelope)
	}
	contribution := Contribution{
		Version:         wire.Version,
		ContributionID:  wire.ContributionID,
		SessionID:       wire.SessionID,
		Source:          wire.Source,
		Parents:         wire.Parents,
		Ranges:          wire.Ranges,
		EnvironmentRefs: wire.EnvironmentRefs,
		CreatedAt:       createdAt,
	}
	if err := contribution.Validate(); err != nil {
		return Contribution{}, err
	}
	return contribution, nil
}

// MarshalBinary encodes a filtered environment attachment. Component order is
// canonicalized without changing the source value.
func (a EnvironmentAttachment) MarshalBinary() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	components := append([]EnvironmentComponentRef(nil), a.Components...)
	sort.Slice(components, func(i, j int) bool {
		return environmentComponentKey(components[i]) < environmentComponentKey(components[j])
	})
	return marshalCompact(environmentWire{
		Version:                a.Version,
		EnvironmentID:          a.EnvironmentID,
		SessionID:              a.SessionID,
		SourceAgent:            a.SourceAgent,
		ObservedAtContribution: a.ObservedAtContribution,
		Components:             components,
	}, "environment attachment", maxDescriptorBytes)
}

// ParseEnvironmentAttachment strictly decodes a filtered environment
// attachment.
func ParseEnvironmentAttachment(data []byte) (EnvironmentAttachment, error) {
	var wire environmentWire
	if err := decodeCompact(data, &wire, "environment attachment", maxDescriptorBytes); err != nil {
		return EnvironmentAttachment{}, err
	}
	attachment := EnvironmentAttachment{
		Version:                wire.Version,
		EnvironmentID:          wire.EnvironmentID,
		SessionID:              wire.SessionID,
		SourceAgent:            wire.SourceAgent,
		ObservedAtContribution: wire.ObservedAtContribution,
		Components:             wire.Components,
	}
	if err := attachment.Validate(); err != nil {
		return EnvironmentAttachment{}, err
	}
	return attachment, nil
}

// MarshalBinary encodes local binding state for atomic local persistence. It
// is not a Remote materialization envelope.
func (b LocalBinding) MarshalBinary() ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	copyBinding := b
	copyBinding.Origin.BaseHeads = sortedStrings(b.Origin.BaseHeads)
	return marshalCompact(copyBinding, "local binding", maxDescriptorBytes)
}

// ParseLocalBinding strictly decodes local binding state.
func ParseLocalBinding(data []byte) (LocalBinding, error) {
	var binding LocalBinding
	if err := decodeCompact(data, &binding, "local binding", maxDescriptorBytes); err != nil {
		return LocalBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return LocalBinding{}, err
	}
	return binding, nil
}

func marshalCompact(value any, kind string, maxBytes int) ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("%w: encode %s", ErrInvalidEnvelope, kind)
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if len(data) == 0 || len(data) > maxBytes {
		return nil, fmt.Errorf("%w: %s exceeds size limit", ErrInvalidEnvelope, kind)
	}
	return append([]byte(nil), data...), nil
}

func decodeCompact(data []byte, destination any, kind string, maxBytes int) error {
	if len(data) == 0 || len(data) > maxBytes {
		return fmt.Errorf("%w: %s size is invalid", ErrInvalidEnvelope, kind)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return fmt.Errorf("%w: %s is not valid JSON", ErrInvalidEnvelope, kind)
	}
	if !bytes.Equal(compact.Bytes(), data) {
		return fmt.Errorf("%w: %s is not compact", ErrInvalidEnvelope, kind)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: decode %s", ErrInvalidEnvelope, kind)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("%w: %s contains trailing JSON", ErrInvalidEnvelope, kind)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s has trailing data", ErrInvalidEnvelope, kind)
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Round(0).Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || value != parsed.UTC().Format(time.RFC3339Nano) {
		return time.Time{}, errors.New("timestamp is not canonical RFC3339")
	}
	return parsed.UTC(), nil
}

func sortedStrings(values []string) []string {
	copyValues := append([]string(nil), values...)
	if copyValues == nil {
		copyValues = []string{}
	}
	sort.Strings(copyValues)
	return copyValues
}

func environmentComponentKey(c EnvironmentComponentRef) string {
	return c.Kind + "\x00" + c.Name + "\x00" + c.Scope + "\x00" + c.ProjectID + "\x00" + c.Fingerprint
}
