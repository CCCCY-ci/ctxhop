package syncer

import (
	"errors"
	"fmt"
)

const (
	// v2ObjectPrefix is deliberately separate from the v1 project/session
	// namespace. A v2 reader may inspect legacy objects, but a v2 writer must
	// never rewrite one of them.
	v2ObjectPrefix = "v2/hubs"

	// Replica shards use the same fixed-width sequence convention as v1. The
	// payload format and digest chain are shared, while the namespace and
	// identity are different.
	replicaShardNameWidth = shardNameWidth

	replicaDescriptorName         = "meta"
	replicaTipName                = "tip"
	contributionDirName           = "contributions"
	environmentDirName            = "environment"
	environmentComponentsDirName  = "components"
	environmentAttachmentsDirName = "environments"
	environmentBodyName           = "000001"

	descriptorMetaName = "meta"
)

var errInvalidReplicaLayout = errors.New("syncer: invalid v2 replica layout")

// SessionHubLayout identifies the v2 namespace of one logical Session. It is
// useful for metadata-only listing, where the Replica and writer device are
// not known yet.
type SessionHubLayout struct {
	hubKey     string
	projectKey string
	sessionKey string
}

// ProjectHubLayout identifies the v2 namespace of one logical Project. It is
// used by project-level metadata listing, where the Session key is discovered
// from descriptor object paths rather than supplied by the caller.
type ProjectHubLayout struct {
	hubKey     string
	projectKey string
}

// EnvironmentHubLayout identifies the Hub-scoped environment component pool.
// Component bodies are keyed by their filtered content fingerprint and may be
// reused by Sessions in the same Hub without copying them into every branch.
type EnvironmentHubLayout struct {
	hubKey string
}

// NewEnvironmentHubLayout validates the opaque Hub key used by the shared
// environment component pool.
func NewEnvironmentHubLayout(hubKey string) (EnvironmentHubLayout, error) {
	if err := validateIdentifier(hubKey); err != nil {
		return EnvironmentHubLayout{}, fmt.Errorf("%w: hub key: %v", errInvalidReplicaLayout, err)
	}
	return EnvironmentHubLayout{hubKey: hubKey}, nil
}

// HubKey returns the validated Hub key carried by this environment layout.
func (l EnvironmentHubLayout) HubKey() string { return l.hubKey }

// HubPrefix returns the root of this Hub's environment component pool.
func (l EnvironmentHubLayout) HubPrefix() (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	return v2ObjectPrefix + "/" + l.hubKey + "/" + environmentDirName, nil
}

// ComponentPrefix returns one device-owned filtered component namespace.
func (l EnvironmentHubLayout) ComponentPrefix(componentKey, deviceID string) (string, error) {
	prefix, err := l.HubPrefix()
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(componentKey); err != nil {
		return "", fmt.Errorf("%w: component key: %v", errInvalidReplicaLayout, err)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/" + environmentComponentsDirName + "/" + componentKey + "/" + deviceID)
}

// ComponentDescriptorKey returns the immutable filtered component metadata
// object key.
func (l EnvironmentHubLayout) ComponentDescriptorKey(componentKey, deviceID string) (string, error) {
	prefix, err := l.ComponentPrefix(componentKey, deviceID)
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + descriptorMetaName)
}

// ComponentBodyKey returns the first immutable filtered component body shard.
func (l EnvironmentHubLayout) ComponentBodyKey(componentKey, deviceID string) (string, error) {
	prefix, err := l.ComponentPrefix(componentKey, deviceID)
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + environmentBodyName)
}

// NewProjectHubLayout validates the opaque keys used by a v2 Project prefix.
func NewProjectHubLayout(hubKey, projectKey string) (ProjectHubLayout, error) {
	if err := validateIdentifier(hubKey); err != nil {
		return ProjectHubLayout{}, fmt.Errorf("%w: hub key: %v", errInvalidReplicaLayout, err)
	}
	if err := validateIdentifier(projectKey); err != nil {
		return ProjectHubLayout{}, fmt.Errorf("%w: project key: %v", errInvalidReplicaLayout, err)
	}
	return ProjectHubLayout{hubKey: hubKey, projectKey: projectKey}, nil
}

// HubPrefix returns the root of this v2 Session Hub.
func (l ProjectHubLayout) HubPrefix() (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	return v2ObjectPrefix + "/" + l.hubKey, nil
}

// HubKey returns the validated opaque Hub key carried by this Project layout.
func (l ProjectHubLayout) HubKey() string { return l.hubKey }

// ProjectKey returns the validated opaque Project key carried by this layout.
func (l ProjectHubLayout) ProjectKey() string { return l.projectKey }

// ProjectPrefix returns the root containing all logical Sessions in this
// Project.
func (l ProjectHubLayout) ProjectPrefix() (string, error) {
	hub, err := l.HubPrefix()
	if err != nil {
		return "", err
	}
	return hub + "/projects/" + l.projectKey, nil
}

// DescriptorKey returns this device's Project descriptor object. It lives at
// the Project level so every Session does not duplicate the same metadata.
func (l ProjectHubLayout) DescriptorKey(deviceID string) (string, error) {
	prefix, err := l.ProjectPrefix()
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/descriptors/" + deviceID + "/" + descriptorMetaName)
}

// Session returns one logical Session namespace below this Project.
func (l ProjectHubLayout) Session(sessionKey string) (SessionHubLayout, error) {
	if err := l.validate(); err != nil {
		return SessionHubLayout{}, err
	}
	return NewSessionHubLayout(l.hubKey, l.projectKey, sessionKey)
}

// NewSessionHubLayout validates the opaque keys used by a v2 Session prefix.
func NewSessionHubLayout(hubKey, projectKey, sessionKey string) (SessionHubLayout, error) {
	for name, value := range map[string]string{
		"hub":     hubKey,
		"project": projectKey,
		"session": sessionKey,
	} {
		if err := validateIdentifier(value); err != nil {
			return SessionHubLayout{}, fmt.Errorf("%w: %s key: %v", errInvalidReplicaLayout, name, err)
		}
	}
	return SessionHubLayout{hubKey: hubKey, projectKey: projectKey, sessionKey: sessionKey}, nil
}

// Replica returns a writer-owned v2 Replica namespace below this Session.
func (l SessionHubLayout) Replica(replicaKey, deviceID string) (ReplicaLayout, error) {
	if err := l.validate(); err != nil {
		return ReplicaLayout{}, err
	}
	if err := validateIdentifier(replicaKey); err != nil {
		return ReplicaLayout{}, fmt.Errorf("%w: replica key: %v", errInvalidReplicaLayout, err)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return ReplicaLayout{}, fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	return ReplicaLayout{
		hubKey:     l.hubKey,
		projectKey: l.projectKey,
		sessionKey: l.sessionKey,
		replicaKey: replicaKey,
		deviceID:   deviceID,
	}, nil
}

// HubPrefix returns the root of one v2 Session Hub.
func (l SessionHubLayout) HubPrefix() (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	return v2ObjectPrefix + "/" + l.hubKey, nil
}

// HubKey returns the validated opaque Hub key carried by this Session layout.
func (l SessionHubLayout) HubKey() string { return l.hubKey }

// ProjectKey returns the validated opaque Project key carried by this Session
// layout.
func (l SessionHubLayout) ProjectKey() string { return l.projectKey }

// SessionKey returns the validated opaque logical Session key carried by this
// layout.
func (l SessionHubLayout) SessionKey() string { return l.sessionKey }

// ProjectPrefix returns the root of one v2 Project.
func (l SessionHubLayout) ProjectPrefix() (string, error) {
	hub, err := l.HubPrefix()
	if err != nil {
		return "", err
	}
	return hub + "/projects/" + l.projectKey, nil
}

// SessionPrefix returns the root containing one logical Session's descriptors,
// Replica streams and Contribution objects.
func (l SessionHubLayout) SessionPrefix() (string, error) {
	project, err := l.ProjectPrefix()
	if err != nil {
		return "", err
	}
	return project + "/sessions/" + l.sessionKey, nil
}

// ContributionPrefix returns the shared immutable Contribution namespace for
// this logical Session. Contributions are Session-level facts, not children of
// an Agent-native Replica, so every source Agent can append a distinct object
// without rewriting another Agent's history.
func (l SessionHubLayout) ContributionPrefix() (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/" + contributionDirName, nil
}

// ContributionPrefixForDevice returns the device-owned immutable Contribution
// namespace. The device segment prevents unrelated writers from racing on
// one remote key while the Contribution ID remains the content identity.
func (l SessionHubLayout) ContributionPrefixForDevice(deviceID string) (string, error) {
	prefix, err := l.ContributionPrefix()
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("%w: contribution device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/" + deviceID)
}

// ContributionKey returns the immutable object key for one device-owned
// Contribution. The Contribution ID is already an opaque, deterministic
// identity and is authenticated again by the encrypted object envelope.
func (l SessionHubLayout) ContributionKey(contributionID, deviceID string) (string, error) {
	prefix, err := l.ContributionPrefixForDevice(deviceID)
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(contributionID); err != nil {
		return "", fmt.Errorf("%w: contribution key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/" + contributionID)
}

// EnvironmentAttachmentPrefix returns one device-owned Session environment
// attachment namespace. The attachment binds a filtered component set to the
// Contribution at which it was observed.
func (l SessionHubLayout) EnvironmentAttachmentPrefix(environmentKey, deviceID string) (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(environmentKey); err != nil {
		return "", fmt.Errorf("%w: environment key: %v", errInvalidReplicaLayout, err)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("%w: environment device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/" + environmentAttachmentsDirName + "/" + environmentKey + "/" + deviceID)
}

// EnvironmentAttachmentDescriptorKey returns the immutable attachment
// metadata object key.
func (l SessionHubLayout) EnvironmentAttachmentDescriptorKey(environmentKey, deviceID string) (string, error) {
	prefix, err := l.EnvironmentAttachmentPrefix(environmentKey, deviceID)
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + descriptorMetaName)
}

// EnvironmentAttachmentBodyKey returns the first immutable attachment body
// shard.
func (l SessionHubLayout) EnvironmentAttachmentBodyKey(environmentKey, deviceID string) (string, error) {
	prefix, err := l.EnvironmentAttachmentPrefix(environmentKey, deviceID)
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + environmentBodyName)
}

// DescriptorKey returns this device's logical Session descriptor object.
func (l SessionHubLayout) DescriptorKey(deviceID string) (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(prefix + "/descriptors/" + deviceID + "/" + descriptorMetaName)
}

// ReplicaLayout identifies one Agent/device/generation Replica namespace.
// All fields are private so callers cannot mutate a validated layout into a
// different writer namespace after construction.
type ReplicaLayout struct {
	hubKey     string
	projectKey string
	sessionKey string
	replicaKey string
	deviceID   string
}

// NewReplicaLayout validates all opaque identifiers used by a Replica.
func NewReplicaLayout(hubKey, projectKey, sessionKey, replicaKey, deviceID string) (ReplicaLayout, error) {
	session, err := NewSessionHubLayout(hubKey, projectKey, sessionKey)
	if err != nil {
		return ReplicaLayout{}, err
	}
	return session.Replica(replicaKey, deviceID)
}

// SessionLayout returns the parent logical Session namespace.
func (l ReplicaLayout) SessionLayout() (SessionHubLayout, error) {
	if err := l.validate(); err != nil {
		return SessionHubLayout{}, err
	}
	return SessionHubLayout{hubKey: l.hubKey, projectKey: l.projectKey, sessionKey: l.sessionKey}, nil
}

// SessionPrefix returns the parent logical Session prefix.
func (l ReplicaLayout) SessionPrefix() (string, error) {
	session, err := l.SessionLayout()
	if err != nil {
		return "", err
	}
	return session.SessionPrefix()
}

// HubKey returns the validated opaque Hub key carried by this layout.
func (l ReplicaLayout) HubKey() string { return l.hubKey }

// ProjectKey returns the validated opaque Project key carried by this layout.
func (l ReplicaLayout) ProjectKey() string { return l.projectKey }

// SessionKey returns the validated opaque logical Session key carried by this
// layout.
func (l ReplicaLayout) SessionKey() string { return l.sessionKey }

// ReplicaKey returns the validated opaque NativeReplica key carried by this
// layout.
func (l ReplicaLayout) ReplicaKey() string { return l.replicaKey }

// DeviceID returns the device segment that owns this Replica prefix.
func (l ReplicaLayout) DeviceID() string { return l.deviceID }

// DescriptorPrefix returns the device-owned descriptor namespace. Descriptor
// objects are written below a device segment so separate devices never race
// on one mutable key.
func (l ReplicaLayout) DescriptorPrefix() (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/descriptors/" + l.deviceID, nil
}

// HubDescriptorKey returns this device's Hub descriptor object key.
func (l ReplicaLayout) HubDescriptorKey() (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	if err := validateIdentifier(l.deviceID); err != nil {
		return "", fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	return checkedKey(v2ObjectPrefix + "/" + l.hubKey + "/descriptors/" + l.deviceID + "/" + descriptorMetaName)
}

// ProjectDescriptorKey returns this device's Project descriptor object key.
func (l ReplicaLayout) ProjectDescriptorKey() (string, error) {
	project, err := NewProjectHubLayout(l.hubKey, l.projectKey)
	if err != nil {
		return "", err
	}
	return project.DescriptorKey(l.deviceID)
}

// SessionDescriptorKey returns this device's logical Session descriptor key.
func (l ReplicaLayout) SessionDescriptorKey() (string, error) {
	session, err := l.SessionLayout()
	if err != nil {
		return "", err
	}
	return session.DescriptorKey(l.deviceID)
}

// ReplicaPrefix returns the writer-owned Replica root.
func (l ReplicaLayout) ReplicaPrefix() (string, error) {
	prefix, err := l.SessionPrefix()
	if err != nil {
		return "", err
	}
	return prefix + "/replicas/" + l.replicaKey + "/" + l.deviceID, nil
}

// ReplicaDescriptorKey returns the immutable Replica identity object key.
func (l ReplicaLayout) ReplicaDescriptorKey() (string, error) {
	prefix, err := l.ReplicaPrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + replicaDescriptorName)
}

// ReplicaTipKey returns the device-owned current-prefix tip key.
func (l ReplicaLayout) ReplicaTipKey() (string, error) {
	prefix, err := l.ReplicaPrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + replicaTipName)
}

// ReplicaShardKey returns the immutable key for one Replica shard.
func (l ReplicaLayout) ReplicaShardKey(number uint64) (string, error) {
	if number == 0 || number > maxShardNumber {
		return "", fmt.Errorf("%w: shard sequence must be between 1 and %d", errInvalidReplicaLayout, maxShardNumber)
	}
	prefix, err := l.ReplicaPrefix()
	if err != nil {
		return "", err
	}
	return checkedKey(prefix + "/" + fmt.Sprintf("%0*d", replicaShardNameWidth, number))
}

func (l SessionHubLayout) validate() error {
	for name, value := range map[string]string{
		"hub":     l.hubKey,
		"project": l.projectKey,
		"session": l.sessionKey,
	} {
		if err := validateIdentifier(value); err != nil {
			return fmt.Errorf("%w: %s key: %v", errInvalidReplicaLayout, name, err)
		}
	}
	return nil
}

func (l ProjectHubLayout) validate() error {
	if err := validateIdentifier(l.hubKey); err != nil {
		return fmt.Errorf("%w: hub key: %v", errInvalidReplicaLayout, err)
	}
	if err := validateIdentifier(l.projectKey); err != nil {
		return fmt.Errorf("%w: project key: %v", errInvalidReplicaLayout, err)
	}
	return nil
}

func (l EnvironmentHubLayout) validate() error {
	if err := validateIdentifier(l.hubKey); err != nil {
		return fmt.Errorf("%w: hub key: %v", errInvalidReplicaLayout, err)
	}
	return nil
}

func (l ReplicaLayout) validate() error {
	if err := (SessionHubLayout{hubKey: l.hubKey, projectKey: l.projectKey, sessionKey: l.sessionKey}).validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"replica": l.replicaKey,
		"device":  l.deviceID,
	} {
		if err := validateIdentifier(value); err != nil {
			return fmt.Errorf("%w: %s key: %v", errInvalidReplicaLayout, name, err)
		}
	}
	return nil
}
