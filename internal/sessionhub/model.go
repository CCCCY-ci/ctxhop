// Package sessionhub contains the v2 Session Hub model and its local
// metadata-only registry.
//
// The model and envelope types deliberately have no Remote or Agent-adapter
// dependency. The registry adds only atomic local persistence for logical
// descriptors and NativeSession bindings; it never stores native session
// bodies.
package sessionhub

import "time"

// ModelVersion is the version of the first Session Hub model/envelope.
const ModelVersion = 1

// HubLifecycle describes whether a SessionHub can receive new project
// bindings.
type HubLifecycle string

const (
	HubActive   HubLifecycle = "active"
	HubArchived HubLifecycle = "archived"
)

// ProjectIdentityKind identifies the stable source used to match a project
// across devices. Local paths are intentionally not a kind.
type ProjectIdentityKind string

const (
	ProjectIdentityRemote ProjectIdentityKind = "remote"
	ProjectIdentityManual ProjectIdentityKind = "manual"
)

// ProjectLifecycle describes whether a project is accepting normal activity.
type ProjectLifecycle string

const (
	ProjectActive   ProjectLifecycle = "active"
	ProjectArchived ProjectLifecycle = "archived"
)

// SessionLifecycle describes whether a logical session can receive new
// native replicas and contributions.
type SessionLifecycle string

const (
	SessionActive   SessionLifecycle = "active"
	SessionArchived SessionLifecycle = "archived"
)

// ReplicaOriginKind records how a NativeSession entered its current native
// lineage.
type ReplicaOriginKind string

const (
	ReplicaOriginNative           ReplicaOriginKind = "native"
	ReplicaOriginSameAgentRestore ReplicaOriginKind = "same-agent-restore"
	ReplicaOriginLocalMaterialize ReplicaOriginKind = "local-materialization"
)

// SessionCreator is non-authoritative display/provenance information on a
// logical Session. It does not make one Agent the owner of that Session.
type SessionCreator struct {
	Agent    string `json:"agent"`
	DeviceID string `json:"deviceId"`
}

// HubDescriptor is the encrypted descriptor for one SessionHub.
type HubDescriptor struct {
	Version   int          `json:"version"`
	HubID     string       `json:"hubId"`
	Name      string       `json:"name"`
	CreatedAt time.Time    `json:"createdAt"`
	Lifecycle HubLifecycle `json:"lifecycle"`
}

// ProjectDescriptor identifies one Project inside one SessionHub.
type ProjectDescriptor struct {
	Version             int                 `json:"version"`
	HubID               string              `json:"hubId"`
	ProjectID           string              `json:"projectId"`
	IdentityKind        ProjectIdentityKind `json:"identityKind"`
	IdentityFingerprint string              `json:"identityFingerprint"`
	CreatedAt           time.Time           `json:"createdAt"`
	Lifecycle           ProjectLifecycle    `json:"lifecycle"`
}

// SessionDescriptor is the logical, cross-Agent session metadata. Native
// records are never stored in this descriptor.
type SessionDescriptor struct {
	Version   int              `json:"version"`
	SessionID string           `json:"sessionId"`
	ProjectID string           `json:"projectId"`
	Title     string           `json:"title"`
	CreatedAt time.Time        `json:"createdAt"`
	CreatedBy SessionCreator   `json:"createdBy"`
	Lifecycle SessionLifecycle `json:"lifecycle"`
}

// NativeSource identifies the Agent-native session represented by a Replica.
// NativeSessionKey is keyed/opaque; the plaintext native ID remains local.
type NativeSource struct {
	Agent            string `json:"agent"`
	NativeSessionKey string `json:"nativeSessionKey"`
	DeviceID         string `json:"deviceId"`
	Generation       uint64 `json:"generation"`
	NativeFormat     string `json:"nativeFormat"`
	AgentVersion     string `json:"agentVersion,omitempty"`
}

// ReplicaOrigin records non-content provenance for a NativeReplica.
type ReplicaOrigin struct {
	Kind      ReplicaOriginKind `json:"kind"`
	BaseHeads []string          `json:"baseHeads"`
}

// NativeReplicaDescriptor identifies one Agent-native append stream. Mutable
// record count and digest belong to ReplicaTip, not to this identity.
type NativeReplicaDescriptor struct {
	Version   int           `json:"version"`
	ReplicaID string        `json:"replicaId"`
	SessionID string        `json:"sessionId"`
	Source    NativeSource  `json:"source"`
	Origin    ReplicaOrigin `json:"origin"`
	CreatedAt time.Time     `json:"createdAt"`
}

// ReplicaTip is the device-owned authenticated summary of a complete Replica
// prefix. The body must still be read and verified before restore.
type ReplicaTip struct {
	Version     int       `json:"version"`
	ReplicaID   string    `json:"replicaId"`
	RecordCount uint64    `json:"recordCount"`
	ShardCount  uint64    `json:"shardCount"`
	LastShard   uint64    `json:"lastShard"`
	HeadDigest  string    `json:"headDigest"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// RangeRef points to a half-open range in one NativeReplica.
type RangeRef struct {
	ReplicaID    string `json:"replicaId"`
	StartRecord  uint64 `json:"startRecord"`
	EndRecord    uint64 `json:"endRecord"`
	PrefixDigest string `json:"prefixDigest"`
	RangeDigest  string `json:"rangeDigest"`
}

// ContributionSource identifies the source of a Contribution's ranges.
type ContributionSource struct {
	Agent      string `json:"agent"`
	ReplicaID  string `json:"replicaId"`
	DeviceID   string `json:"deviceId"`
	Generation uint64 `json:"generation"`
}

// Contribution is an immutable causal index entry. It carries no record
// body; ranges point back into a NativeReplica.
type Contribution struct {
	Version         int                `json:"version"`
	ContributionID  string             `json:"contributionId"`
	SessionID       string             `json:"sessionId"`
	Source          ContributionSource `json:"source"`
	Parents         []string           `json:"parents"`
	Ranges          []RangeRef         `json:"ranges"`
	EnvironmentRefs []string           `json:"environmentRefs"`
	CreatedAt       time.Time          `json:"createdAt"`
}

// EnvironmentComponentRef is the safe, non-sensitive reference stored in an
// EnvironmentAttachment. Component bodies are owned by the environment layer.
type EnvironmentComponentRef struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	ProjectID   string `json:"projectId,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Portability string `json:"portability"`
}

// EnvironmentAttachment associates filtered environment facts with the
// Contribution at which they were observed.
type EnvironmentAttachment struct {
	Version                int                       `json:"version"`
	EnvironmentID          string                    `json:"environmentId"`
	SessionID              string                    `json:"sessionId"`
	SourceAgent            string                    `json:"sourceAgent"`
	ObservedAtContribution string                    `json:"observedAtContribution"`
	Components             []EnvironmentComponentRef `json:"components"`
}

// ReplicaCursor proves how much of a NativeReplica body this local writer has
// published.
type ReplicaCursor struct {
	NextShard   uint64 `json:"nextShard"`
	RecordCount uint64 `json:"recordCount"`
	HeadDigest  string `json:"headDigest"`
}

// ContributionCursor proves which records after an optional imported prefix
// have already been published as Contributions.
type ContributionCursor struct {
	EndRecord          uint64 `json:"endRecord"`
	LastContributionID string `json:"lastContributionId"`
}

// ImportBoundary marks the end of a materialized/imported prefix in a target
// NativeSession.
type ImportBoundary struct {
	RecordCount  uint64 `json:"recordCount"`
	PrefixDigest string `json:"prefixDigest"`
}

// ConverterProvenance identifies the local conversion implementation used to
// create a target NativeSession.
type ConverterProvenance struct {
	SourceViewVersion    int    `json:"sourceViewVersion"`
	TargetAdapterVersion string `json:"targetAdapterVersion"`
}

// BindingOrigin records how a local NativeSession was initially associated
// with a Session. It is local state and is never a Remote materialization
// object.
type BindingOrigin struct {
	Kind           ReplicaOriginKind    `json:"kind"`
	BaseHeads      []string             `json:"baseHeads"`
	ImportBoundary *ImportBoundary      `json:"importBoundary,omitempty"`
	Converter      *ConverterProvenance `json:"converter,omitempty"`
}

// LocalBinding maps a plaintext local Agent NativeSession to a logical
// Session. NativeSessionID is intentionally local-only.
type LocalBinding struct {
	Version            int                `json:"version"`
	HubID              string             `json:"hubId"`
	ProjectID          string             `json:"projectId"`
	SessionID          string             `json:"sessionId"`
	Agent              string             `json:"agent"`
	NativeSessionID    string             `json:"nativeSessionId"`
	ReplicaID          string             `json:"replicaId"`
	Generation         uint64             `json:"generation"`
	ReplicaCursor      ReplicaCursor      `json:"replicaCursor"`
	ContributionCursor ContributionCursor `json:"contributionCursor"`
	Origin             BindingOrigin      `json:"origin"`
}
