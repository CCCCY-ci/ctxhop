package sessionhub

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxOpaqueIDLength   = 128
	maxAgentLength      = 64
	maxNameLength       = 128
	maxTitleLength      = 256
	maxFormatLength     = 128
	maxVersionLength    = 64
	maxParents          = 256
	maxRanges           = 256
	maxEnvironmentRefs  = 256
	maxComponents       = 128
	maxDescriptorBytes  = 1 << 20
	maxContributionSize = 1 << 20
)

var (
	// ErrInvalidModel reports a model value that cannot be used by any v2
	// layer.
	ErrInvalidModel = errors.New("sessionhub: invalid model")

	// ErrUnsupportedVersion reports a model/envelope version newer than this
	// build can safely interpret.
	ErrUnsupportedVersion = errors.New("sessionhub: unsupported model version")

	// ErrInvalidEnvelope reports malformed or non-canonical JSON input.
	ErrInvalidEnvelope = errors.New("sessionhub: invalid envelope")

	// ErrInvalidIdentity reports an identifier or identity component that is
	// not safe for the v2 namespace.
	ErrInvalidIdentity = errors.New("sessionhub: invalid identity")

	// ErrInvalidHierarchy reports a relationship between valid objects that is
	// not valid in the Domain → Hub → Project → Session hierarchy.
	ErrInvalidHierarchy = errors.New("sessionhub: invalid hierarchy")
)

func validateVersion(version int) error {
	switch {
	case version == ModelVersion:
		return nil
	case version > ModelVersion:
		return ErrUnsupportedVersion
	default:
		return fmt.Errorf("%w: version is invalid", ErrInvalidModel)
	}
}

func validateOpaqueID(value string) error {
	if value == "" || len(value) > maxOpaqueIDLength {
		return ErrInvalidIdentity
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return ErrInvalidIdentity
	}
	return nil
}

func validateAgent(value string) error {
	if value == "" || len(value) > maxAgentLength || !utf8.ValidString(value) {
		return ErrInvalidIdentity
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return ErrInvalidIdentity
	}
	return nil
}

func validateIdentityPart(value string) error {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return ErrInvalidIdentity
	}
	return nil
}

func validateText(value string, maxRunes int, required bool) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return ErrInvalidModel
	}
	if required && strings.TrimSpace(value) == "" {
		return ErrInvalidModel
	}
	if len([]rune(value)) > maxRunes {
		return ErrInvalidModel
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ErrInvalidModel
		}
	}
	return nil
}

func validateNativeSessionID(value string) error {
	if err := validateText(value, maxNameLength, true); err != nil {
		return err
	}
	if strings.ContainsAny(value, "/\\") {
		return ErrInvalidIdentity
	}
	return nil
}

func validateTime(value time.Time) error {
	if value.IsZero() {
		return ErrInvalidModel
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != 64 || value != strings.ToLower(value) {
		return ErrInvalidModel
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return ErrInvalidModel
	}
	return nil
}

func validateFingerprint(value string) error {
	if len(value) == 64 {
		return validateDigest(value)
	}
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return ErrInvalidModel
	}
	return validateDigest(strings.TrimPrefix(value, "sha256:"))
}

func validateLifecycle(value string) error {
	if value != string(HubActive) && value != string(HubArchived) {
		return ErrInvalidModel
	}
	return nil
}

func validateProjectLifecycle(value ProjectLifecycle) error {
	if value != ProjectActive && value != ProjectArchived {
		return ErrInvalidModel
	}
	return nil
}

func validateSessionLifecycle(value SessionLifecycle) error {
	if value != SessionActive && value != SessionArchived {
		return ErrInvalidModel
	}
	return nil
}

// Validate checks one Hub descriptor before it crosses a storage boundary.
func (h HubDescriptor) Validate() error {
	if err := validateVersion(h.Version); err != nil {
		return fmt.Errorf("%w: hub version", err)
	}
	if err := validateOpaqueID(h.HubID); err != nil {
		return fmt.Errorf("%w: hub id", err)
	}
	if err := validateText(h.Name, maxNameLength, true); err != nil {
		return fmt.Errorf("%w: hub name", err)
	}
	if err := validateTime(h.CreatedAt); err != nil {
		return fmt.Errorf("%w: hub timestamp", err)
	}
	if err := validateLifecycle(string(h.Lifecycle)); err != nil {
		return fmt.Errorf("%w: hub lifecycle", err)
	}
	return nil
}

// Validate checks one Project descriptor.
func (p ProjectDescriptor) Validate() error {
	if err := validateVersion(p.Version); err != nil {
		return fmt.Errorf("%w: project version", err)
	}
	if err := validateOpaqueID(p.HubID); err != nil {
		return fmt.Errorf("%w: project hub id", err)
	}
	if err := validateOpaqueID(p.ProjectID); err != nil {
		return fmt.Errorf("%w: project id", err)
	}
	if p.IdentityKind != ProjectIdentityRemote && p.IdentityKind != ProjectIdentityManual {
		return fmt.Errorf("%w: project identity kind", ErrInvalidModel)
	}
	if err := validateText(p.IdentityFingerprint, maxNameLength, true); err != nil {
		return fmt.Errorf("%w: project identity fingerprint", err)
	}
	if err := validateTime(p.CreatedAt); err != nil {
		return fmt.Errorf("%w: project timestamp", err)
	}
	if err := validateProjectLifecycle(p.Lifecycle); err != nil {
		return fmt.Errorf("%w: project lifecycle", err)
	}
	return nil
}

// Validate checks one logical Session descriptor.
func (s SessionDescriptor) Validate() error {
	if err := validateVersion(s.Version); err != nil {
		return fmt.Errorf("%w: session version", err)
	}
	if err := validateOpaqueID(s.SessionID); err != nil {
		return fmt.Errorf("%w: session id", err)
	}
	if err := validateOpaqueID(s.ProjectID); err != nil {
		return fmt.Errorf("%w: session project id", err)
	}
	if err := validateText(s.Title, maxTitleLength, false); err != nil {
		return fmt.Errorf("%w: session title", err)
	}
	if err := validateTime(s.CreatedAt); err != nil {
		return fmt.Errorf("%w: session timestamp", err)
	}
	if err := validateSessionCreator(s.CreatedBy); err != nil {
		return fmt.Errorf("%w: session creator", err)
	}
	if err := validateSessionLifecycle(s.Lifecycle); err != nil {
		return fmt.Errorf("%w: session lifecycle", err)
	}
	return nil
}

func validateSessionCreator(creator SessionCreator) error {
	if err := validateAgent(creator.Agent); err != nil {
		return err
	}
	return validateOpaqueID(creator.DeviceID)
}

// Validate checks the source identity of a NativeReplica.
func (s NativeSource) Validate() error {
	if err := validateAgent(s.Agent); err != nil {
		return fmt.Errorf("%w: source agent", err)
	}
	if err := validateOpaqueID(s.NativeSessionKey); err != nil {
		return fmt.Errorf("%w: source native session key", err)
	}
	if err := validateOpaqueID(s.DeviceID); err != nil {
		return fmt.Errorf("%w: source device id", err)
	}
	if s.Generation == 0 {
		return fmt.Errorf("%w: source generation", ErrInvalidModel)
	}
	if err := validateText(s.NativeFormat, maxFormatLength, true); err != nil {
		return fmt.Errorf("%w: source native format", err)
	}
	if s.AgentVersion != "" {
		if err := validateText(s.AgentVersion, maxVersionLength, false); err != nil {
			return fmt.Errorf("%w: source agent version", err)
		}
	}
	return nil
}

// Validate checks Replica provenance.
func (o ReplicaOrigin) Validate() error {
	switch o.Kind {
	case ReplicaOriginNative, ReplicaOriginSameAgentRestore:
	case ReplicaOriginLocalMaterialize:
		if len(o.BaseHeads) == 0 {
			return fmt.Errorf("%w: materialized replica needs a base head", ErrInvalidModel)
		}
	default:
		return fmt.Errorf("%w: replica origin kind", ErrInvalidModel)
	}
	if err := validateIDList(o.BaseHeads, maxParents, false); err != nil {
		return fmt.Errorf("%w: replica base heads", err)
	}
	return nil
}

// Validate checks one NativeReplica descriptor.
func (r NativeReplicaDescriptor) Validate() error {
	if err := validateVersion(r.Version); err != nil {
		return fmt.Errorf("%w: replica version", err)
	}
	if err := validateOpaqueID(r.ReplicaID); err != nil {
		return fmt.Errorf("%w: replica id", err)
	}
	if err := validateOpaqueID(r.SessionID); err != nil {
		return fmt.Errorf("%w: replica session id", err)
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("%w: replica source", err)
	}
	if err := r.Origin.Validate(); err != nil {
		return fmt.Errorf("%w: replica origin", err)
	}
	if err := validateTime(r.CreatedAt); err != nil {
		return fmt.Errorf("%w: replica timestamp", err)
	}
	return nil
}

// Validate checks an authenticated Replica tip summary.
func (t ReplicaTip) Validate() error {
	if err := validateVersion(t.Version); err != nil {
		return fmt.Errorf("%w: tip version", err)
	}
	if err := validateOpaqueID(t.ReplicaID); err != nil {
		return fmt.Errorf("%w: tip replica id", err)
	}
	if err := validateDigest(t.HeadDigest); err != nil {
		return fmt.Errorf("%w: tip digest", err)
	}
	if t.RecordCount == 0 {
		if t.ShardCount != 0 || t.LastShard != 0 {
			return fmt.Errorf("%w: empty tip has shards", ErrInvalidModel)
		}
	} else if t.ShardCount == 0 || t.LastShard != t.ShardCount {
		return fmt.Errorf("%w: tip shard count", ErrInvalidModel)
	}
	if err := validateTime(t.UpdatedAt); err != nil {
		return fmt.Errorf("%w: tip timestamp", err)
	}
	return nil
}

func validateContributionSource(source ContributionSource) error {
	if err := validateAgent(source.Agent); err != nil {
		return fmt.Errorf("%w: contribution source agent", err)
	}
	if err := validateOpaqueID(source.ReplicaID); err != nil {
		return fmt.Errorf("%w: contribution source replica", err)
	}
	if err := validateOpaqueID(source.DeviceID); err != nil {
		return fmt.Errorf("%w: contribution source device", err)
	}
	if source.Generation == 0 {
		return fmt.Errorf("%w: contribution source generation", ErrInvalidModel)
	}
	return nil
}

func validateRange(r RangeRef) error {
	if err := validateOpaqueID(r.ReplicaID); err != nil {
		return fmt.Errorf("%w: range replica", err)
	}
	if r.StartRecord >= r.EndRecord {
		return fmt.Errorf("%w: range is empty or reversed", ErrInvalidModel)
	}
	if err := validateDigest(r.PrefixDigest); err != nil {
		return fmt.Errorf("%w: range prefix digest", err)
	}
	if err := validateDigest(r.RangeDigest); err != nil {
		return fmt.Errorf("%w: range digest", err)
	}
	return nil
}

func validateIDList(values []string, max int, required bool) error {
	if required && len(values) == 0 {
		return ErrInvalidModel
	}
	if len(values) > max {
		return ErrInvalidModel
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateOpaqueID(value); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return ErrInvalidModel
		}
		seen[value] = struct{}{}
	}
	return nil
}

// Validate checks a Contribution without requiring its ID to have been
// derived. This permits callers to construct the identity digest first; an
// encoded envelope must still carry a non-empty ContributionID.
func (c Contribution) Validate() error {
	return c.validate(true)
}

func (c Contribution) validate(requireID bool) error {
	if err := validateVersion(c.Version); err != nil {
		return fmt.Errorf("%w: contribution version", err)
	}
	if requireID {
		if err := validateOpaqueID(c.ContributionID); err != nil {
			return fmt.Errorf("%w: contribution id", err)
		}
	} else if c.ContributionID != "" {
		if err := validateOpaqueID(c.ContributionID); err != nil {
			return fmt.Errorf("%w: contribution id", err)
		}
	}
	if err := validateOpaqueID(c.SessionID); err != nil {
		return fmt.Errorf("%w: contribution session id", err)
	}
	if err := validateContributionSource(c.Source); err != nil {
		return err
	}
	if err := validateIDList(c.Parents, maxParents, false); err != nil {
		return fmt.Errorf("%w: contribution parents", err)
	}
	if len(c.Ranges) == 0 || len(c.Ranges) > maxRanges {
		return fmt.Errorf("%w: contribution ranges", ErrInvalidModel)
	}
	ranges := append([]RangeRef(nil), c.Ranges...)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].ReplicaID != ranges[j].ReplicaID {
			return ranges[i].ReplicaID < ranges[j].ReplicaID
		}
		if ranges[i].StartRecord != ranges[j].StartRecord {
			return ranges[i].StartRecord < ranges[j].StartRecord
		}
		return ranges[i].EndRecord < ranges[j].EndRecord
	})
	previousEnd := uint64(0)
	for i, r := range ranges {
		if err := validateRange(r); err != nil {
			return fmt.Errorf("%w: contribution range", err)
		}
		if r.ReplicaID != c.Source.ReplicaID {
			return fmt.Errorf("%w: contribution range uses another replica", ErrInvalidModel)
		}
		if i > 0 && r.StartRecord < previousEnd {
			return fmt.Errorf("%w: contribution ranges overlap", ErrInvalidModel)
		}
		previousEnd = r.EndRecord
	}
	if err := validateIDList(c.EnvironmentRefs, maxEnvironmentRefs, false); err != nil {
		return fmt.Errorf("%w: contribution environment refs", err)
	}
	if err := validateTime(c.CreatedAt); err != nil {
		return fmt.Errorf("%w: contribution timestamp", err)
	}
	return nil
}

func validateEnvironmentComponent(c EnvironmentComponentRef) error {
	switch c.Kind {
	case "skill", "mcp", "mcp-intent", "settings", "hook", "tool-requirement":
	default:
		return ErrInvalidModel
	}
	if err := validateText(c.Name, maxNameLength, true); err != nil {
		return err
	}
	if c.Scope != "global" && c.Scope != "project" {
		return ErrInvalidModel
	}
	if c.Scope == "project" {
		if err := validateOpaqueID(c.ProjectID); err != nil {
			return err
		}
	} else if c.ProjectID != "" {
		return ErrInvalidModel
	}
	if err := validateFingerprint(c.Fingerprint); err != nil {
		return err
	}
	switch c.Portability {
	case "portable", "platform-specific", "unsupported", "review-required":
	default:
		return ErrInvalidModel
	}
	return nil
}

// Validate checks a filtered environment attachment.
func (a EnvironmentAttachment) Validate() error {
	if err := validateVersion(a.Version); err != nil {
		return fmt.Errorf("%w: environment version", err)
	}
	if err := validateOpaqueID(a.EnvironmentID); err != nil {
		return fmt.Errorf("%w: environment id", err)
	}
	if err := validateOpaqueID(a.SessionID); err != nil {
		return fmt.Errorf("%w: environment session id", err)
	}
	if err := validateAgent(a.SourceAgent); err != nil {
		return fmt.Errorf("%w: environment source agent", err)
	}
	if err := validateOpaqueID(a.ObservedAtContribution); err != nil {
		return fmt.Errorf("%w: environment contribution", err)
	}
	if len(a.Components) == 0 || len(a.Components) > maxComponents {
		return fmt.Errorf("%w: environment components", ErrInvalidModel)
	}
	seen := make(map[string]struct{}, len(a.Components))
	for _, component := range a.Components {
		if err := validateEnvironmentComponent(component); err != nil {
			return fmt.Errorf("%w: environment component", err)
		}
		key := strings.Join([]string{component.Kind, component.Name, component.Scope, component.ProjectID}, "\x00")
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: duplicate environment component", ErrInvalidModel)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// Validate checks a local Replica cursor.
func (c ReplicaCursor) Validate() error {
	if c.NextShard == 0 {
		return fmt.Errorf("%w: next shard is zero", ErrInvalidModel)
	}
	if err := validateDigest(c.HeadDigest); err != nil {
		return fmt.Errorf("%w: cursor digest", err)
	}
	return nil
}

// Validate checks the Contribution cursor.
func (c ContributionCursor) Validate() error {
	if c.LastContributionID != "" {
		if err := validateOpaqueID(c.LastContributionID); err != nil {
			return fmt.Errorf("%w: cursor contribution id", err)
		}
	}
	return nil
}

// Validate checks an imported prefix boundary.
func (b ImportBoundary) Validate() error {
	if err := validateDigest(b.PrefixDigest); err != nil {
		return fmt.Errorf("%w: import boundary digest", err)
	}
	return nil
}

// Validate checks conversion provenance.
func (p ConverterProvenance) Validate() error {
	if p.SourceViewVersion <= 0 {
		return fmt.Errorf("%w: source view version", ErrInvalidModel)
	}
	if err := validateText(p.TargetAdapterVersion, maxVersionLength, true); err != nil {
		return fmt.Errorf("%w: target adapter version", err)
	}
	return nil
}

// Validate checks how a local NativeSession entered a logical Session.
func (o BindingOrigin) Validate() error {
	if err := validateIDList(o.BaseHeads, maxParents, false); err != nil {
		return fmt.Errorf("%w: binding base heads", err)
	}
	switch o.Kind {
	case ReplicaOriginNative:
		if len(o.BaseHeads) != 0 || o.ImportBoundary != nil || o.Converter != nil {
			return fmt.Errorf("%w: native origin has conversion state", ErrInvalidModel)
		}
	case ReplicaOriginSameAgentRestore:
		if o.ImportBoundary != nil || o.Converter != nil {
			return fmt.Errorf("%w: same-agent origin has conversion state", ErrInvalidModel)
		}
	case ReplicaOriginLocalMaterialize:
		if len(o.BaseHeads) == 0 || o.ImportBoundary == nil || o.Converter == nil {
			return fmt.Errorf("%w: materialized origin is incomplete", ErrInvalidModel)
		}
		if err := o.ImportBoundary.Validate(); err != nil {
			return err
		}
		if err := o.Converter.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: binding origin kind", ErrInvalidModel)
	}
	return nil
}

// Validate checks a local-only NativeSession binding.
func (b LocalBinding) Validate() error {
	if err := validateVersion(b.Version); err != nil {
		return fmt.Errorf("%w: binding version", err)
	}
	for name, value := range map[string]string{
		"binding hub id":     b.HubID,
		"binding project id": b.ProjectID,
		"binding session id": b.SessionID,
		"binding replica id": b.ReplicaID,
	} {
		if err := validateOpaqueID(value); err != nil {
			return fmt.Errorf("%w: %s", err, name)
		}
	}
	if err := validateAgent(b.Agent); err != nil {
		return fmt.Errorf("%w: binding agent", err)
	}
	if err := validateNativeSessionID(b.NativeSessionID); err != nil {
		return fmt.Errorf("%w: binding native session id", err)
	}
	if b.Generation == 0 {
		return fmt.Errorf("%w: binding generation", ErrInvalidModel)
	}
	if err := b.ReplicaCursor.Validate(); err != nil {
		return fmt.Errorf("%w: binding replica cursor", err)
	}
	if err := b.ContributionCursor.Validate(); err != nil {
		return fmt.Errorf("%w: binding contribution cursor", err)
	}
	if b.ContributionCursor.EndRecord > b.ReplicaCursor.RecordCount {
		return fmt.Errorf("%w: contribution cursor is ahead of replica cursor", ErrInvalidModel)
	}
	if err := b.Origin.Validate(); err != nil {
		return fmt.Errorf("%w: binding origin", err)
	}
	if b.Origin.ImportBoundary != nil {
		if b.Origin.ImportBoundary.RecordCount > b.ContributionCursor.EndRecord {
			return fmt.Errorf("%w: contribution cursor precedes import boundary", ErrInvalidModel)
		}
	}
	return nil
}

// ValidateHierarchy validates the parent-child relationships of the core
// descriptors. It intentionally does not scan or contact a Remote.
func ValidateHierarchy(h HubDescriptor, p ProjectDescriptor, s SessionDescriptor) error {
	if err := h.Validate(); err != nil {
		return fmt.Errorf("%w: hub: %v", ErrInvalidHierarchy, err)
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: project: %v", ErrInvalidHierarchy, err)
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: session: %v", ErrInvalidHierarchy, err)
	}
	if h.HubID != p.HubID || p.ProjectID != s.ProjectID {
		return fmt.Errorf("%w: parent identifiers do not match", ErrInvalidHierarchy)
	}
	return nil
}

// ValidateReplicaForSession checks that a Replica belongs to the supplied
// logical Session.
func ValidateReplicaForSession(r NativeReplicaDescriptor, s SessionDescriptor) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: session: %v", ErrInvalidHierarchy, err)
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("%w: replica: %v", ErrInvalidHierarchy, err)
	}
	if r.SessionID != s.SessionID {
		return fmt.Errorf("%w: replica belongs to another session", ErrInvalidHierarchy)
	}
	return nil
}

// ValidateContributionForSession checks that a Contribution belongs to the
// supplied logical Session.
func ValidateContributionForSession(c Contribution, s SessionDescriptor) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: session: %v", ErrInvalidHierarchy, err)
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: contribution: %v", ErrInvalidHierarchy, err)
	}
	if c.SessionID != s.SessionID {
		return fmt.Errorf("%w: contribution belongs to another session", ErrInvalidHierarchy)
	}
	return nil
}
