package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

const (
	projectAnnouncementVersion           = 1
	projectAnnouncementSegment           = "announcements"
	maxProjectAnnouncementBytes          = 16 << 10
	maxEncryptedProjectAnnouncementBytes = maxProjectAnnouncementBytes + 1024
	maxProjectIdentityBytes              = 4 << 10
)

var (
	// ErrInvalidProjectAnnouncement reports malformed or incomplete project
	// discovery metadata.
	ErrInvalidProjectAnnouncement = errors.New("syncer: invalid project announcement")

	// ErrUnsupportedProjectAnnouncement reports a project announcement newer
	// than this build understands.
	ErrUnsupportedProjectAnnouncement = errors.New("syncer: project announcement format is newer than this build")

	// ErrDuplicateProjectAnnouncement reports duplicate objects for one
	// project/device announcement key.
	ErrDuplicateProjectAnnouncement = errors.New("syncer: duplicate project announcement")

	// ErrConflictingProjectAnnouncement reports different identities advertised
	// for the same opaque project ID.
	ErrConflictingProjectAnnouncement = errors.New("syncer: conflicting project announcement")

	// ErrRemoteProjectAnnouncementTooLarge reports an encrypted announcement
	// above the bounded read size.
	ErrRemoteProjectAnnouncementTooLarge = errors.New("syncer: remote project announcement is too large")
)

// ProjectAnnouncement is an encrypted, device-owned record that makes one
// project discoverable to other authorized devices. It contains no local path;
// paths have meaning only on the device that owns them.
type ProjectAnnouncement struct {
	ProjectID    string
	DeviceID     string
	IdentityKind string
	Identity     string
	AnnouncedAt  time.Time
}

// NewProjectAnnouncement validates one project discovery record.
func NewProjectAnnouncement(projectID, deviceID, identityKind, identity string, announcedAt time.Time) (ProjectAnnouncement, error) {
	record := ProjectAnnouncement{
		ProjectID:    projectID,
		DeviceID:     deviceID,
		IdentityKind: strings.TrimSpace(identityKind),
		Identity:     strings.TrimSpace(identity),
		AnnouncedAt:  announcedAt.UTC(),
	}
	if err := record.Validate(); err != nil {
		return ProjectAnnouncement{}, err
	}
	return record, nil
}

// Validate checks fields that are persisted in a project announcement.
func (r ProjectAnnouncement) Validate() error {
	if err := validateIdentifier(r.ProjectID); err != nil {
		return fmt.Errorf("%w: project ID: %v", ErrInvalidProjectAnnouncement, err)
	}
	if err := validateIdentifier(r.DeviceID); err != nil {
		return fmt.Errorf("%w: device ID: %v", ErrInvalidProjectAnnouncement, err)
	}
	switch r.IdentityKind {
	case "remote", "manual":
	default:
		return fmt.Errorf("%w: unsupported identity kind %q", ErrInvalidProjectAnnouncement, r.IdentityKind)
	}
	if r.Identity == "" || len([]byte(r.Identity)) > maxProjectIdentityBytes || strings.ContainsRune(r.Identity, 0) {
		return fmt.Errorf("%w: project identity is empty or too large", ErrInvalidProjectAnnouncement)
	}
	if r.AnnouncedAt.IsZero() {
		return fmt.Errorf("%w: announcement time is required", ErrInvalidProjectAnnouncement)
	}
	return nil
}

// ProjectAnnouncementKey returns the device-owned key for a project discovery
// record. Each device writes only its own key, so discovery needs no lock.
func ProjectAnnouncementKey(projectID, deviceID string) (string, error) {
	if err := validateIdentifier(projectID); err != nil {
		return "", fmt.Errorf("syncer: invalid project identifier: %w", err)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return "", fmt.Errorf("syncer: invalid device identifier: %w", err)
	}
	return checkedKey(objectPrefix + "/" + projectID + "/" + projectAnnouncementSegment + "/" + deviceID)
}

// SealProjectAnnouncement encodes and encrypts a project discovery record.
func SealProjectAnnouncement(recipient *ecdh.PublicKey, objectKey string, record ProjectAnnouncement) ([]byte, error) {
	projectID, deviceID, ok := parseProjectAnnouncementKey(objectKey)
	if !ok {
		return nil, fmt.Errorf("syncer: seal project announcement: invalid object key")
	}
	if record.ProjectID != projectID || record.DeviceID != deviceID {
		return nil, fmt.Errorf("%w: record does not match object key", ErrInvalidProjectAnnouncement)
	}
	plaintext, err := marshalProjectAnnouncement(record)
	if err != nil {
		return nil, fmt.Errorf("syncer: encode project announcement: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, plaintext)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt project announcement: %w", err)
	}
	return sealed, nil
}

// OpenProjectAnnouncement decrypts and validates a discovery record.
func OpenProjectAnnouncement(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (ProjectAnnouncement, error) {
	projectID, deviceID, ok := parseProjectAnnouncementKey(objectKey)
	if !ok {
		return ProjectAnnouncement{}, fmt.Errorf("syncer: open project announcement: invalid object key")
	}
	plaintext, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return ProjectAnnouncement{}, fmt.Errorf("syncer: decrypt project announcement: %w", err)
	}
	record, err := parseProjectAnnouncement(projectID, deviceID, plaintext)
	if err != nil {
		return ProjectAnnouncement{}, fmt.Errorf("syncer: parse project announcement: %w", err)
	}
	return record, nil
}

// PutProjectAnnouncement publishes a device-owned encrypted discovery record.
func PutProjectAnnouncement(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, record ProjectAnnouncement) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	key, err := ProjectAnnouncementKey(record.ProjectID, record.DeviceID)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish project announcement: %w", err)
	}
	sealed, err := SealProjectAnnouncement(recipient, key, record)
	if err != nil {
		return err
	}
	if len(sealed) > maxEncryptedProjectAnnouncementBytes {
		return fmt.Errorf("%w: maximum is %d bytes", ErrRemoteProjectAnnouncementTooLarge, maxEncryptedProjectAnnouncementBytes)
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish project announcement: %w", err)
	}
	return nil
}

// FetchProjectAnnouncements discovers projects from the encrypted, device-owned
// announcement keys. It returns an empty slice when no projects have been
// announced.
func FetchProjectAnnouncements(ctx context.Context, store remote.Remote, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]ProjectAnnouncement, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, objectPrefix+"/")
	if err != nil {
		return nil, fmt.Errorf("syncer: list project announcements: %w", err)
	}

	keys := make([]string, 0)
	seen := make(map[string]struct{})
	for _, object := range objects {
		_, deviceID, ok := parseProjectAnnouncementKey(object.Key)
		if !ok {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[deviceID]; !ok {
				continue
			}
		}
		if _, exists := seen[object.Key]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateProjectAnnouncement, object.Key)
		}
		seen[object.Key] = struct{}{}
		keys = append(keys, object.Key)
	}
	sort.Strings(keys)

	out := make([]ProjectAnnouncement, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("syncer: read project announcements: %w", err)
		}
		sealed, err := readRemoteProjectAnnouncement(ctx, store, key)
		if err != nil {
			return nil, err
		}
		record, err := openProjectAnnouncementWithIdentities(identities, key, sealed)
		if err != nil {
			return nil, fmt.Errorf("syncer: open project announcement: %w", err)
		}
		out = append(out, record)
	}
	return out, nil
}

func openProjectAnnouncementWithIdentities(identities []*ecdh.PrivateKey, key string, sealed []byte) (ProjectAnnouncement, error) {
	var last error
	for _, identity := range identities {
		record, err := OpenProjectAnnouncement(identity, key, sealed)
		if err == nil {
			return record, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("syncer: no project announcement identity succeeded")
	}
	return ProjectAnnouncement{}, last
}

func readRemoteProjectAnnouncement(ctx context.Context, store remote.Remote, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("syncer: get project announcement: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedProjectAnnouncementBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("syncer: read project announcement: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("syncer: read project announcement: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("syncer: close project announcement: %w", closeErr)
	}
	if len(data) > maxEncryptedProjectAnnouncementBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrRemoteProjectAnnouncementTooLarge, maxEncryptedProjectAnnouncementBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("syncer: read project announcement: %w", err)
	}
	return data, nil
}

type projectAnnouncementWire struct {
	Version      int    `json:"version"`
	ProjectID    string `json:"projectId"`
	DeviceID     string `json:"deviceId"`
	IdentityKind string `json:"identityKind"`
	Identity     string `json:"identity"`
	AnnouncedAt  string `json:"announcedAt"`
}

func marshalProjectAnnouncement(record ProjectAnnouncement) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	wire := projectAnnouncementWire{
		Version:      projectAnnouncementVersion,
		ProjectID:    record.ProjectID,
		DeviceID:     record.DeviceID,
		IdentityKind: record.IdentityKind,
		Identity:     record.Identity,
		AnnouncedAt:  record.AnnouncedAt.UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal project announcement: %w", err)
	}
	if len(data) > maxProjectAnnouncementBytes {
		return nil, fmt.Errorf("%w: encoded announcement exceeds %d bytes", ErrInvalidProjectAnnouncement, maxProjectAnnouncementBytes)
	}
	return data, nil
}

func parseProjectAnnouncement(projectID, deviceID string, data []byte) (ProjectAnnouncement, error) {
	if len(data) == 0 {
		return ProjectAnnouncement{}, fmt.Errorf("%w: empty envelope", ErrInvalidProjectAnnouncement)
	}
	if len(data) > maxProjectAnnouncementBytes {
		return ProjectAnnouncement{}, fmt.Errorf("%w: encoded announcement exceeds %d bytes", ErrInvalidProjectAnnouncement, maxProjectAnnouncementBytes)
	}
	var wire projectAnnouncementWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return ProjectAnnouncement{}, fmt.Errorf("%w: decode envelope: %v", ErrInvalidProjectAnnouncement, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return ProjectAnnouncement{}, fmt.Errorf("%w: envelope contains trailing JSON", ErrInvalidProjectAnnouncement)
	} else if !errors.Is(err, io.EOF) {
		return ProjectAnnouncement{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidProjectAnnouncement, err)
	}
	if wire.Version > projectAnnouncementVersion {
		return ProjectAnnouncement{}, fmt.Errorf("%w: version %d", ErrUnsupportedProjectAnnouncement, wire.Version)
	}
	if wire.Version != projectAnnouncementVersion {
		return ProjectAnnouncement{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidProjectAnnouncement, wire.Version)
	}
	announcedAt, err := time.Parse(time.RFC3339Nano, wire.AnnouncedAt)
	if err != nil {
		return ProjectAnnouncement{}, fmt.Errorf("%w: announcement time: %v", ErrInvalidProjectAnnouncement, err)
	}
	record, err := NewProjectAnnouncement(projectID, deviceID, wire.IdentityKind, wire.Identity, announcedAt)
	if err != nil {
		return ProjectAnnouncement{}, err
	}
	if wire.ProjectID != projectID || wire.DeviceID != deviceID {
		return ProjectAnnouncement{}, fmt.Errorf("%w: envelope identity does not match object key", ErrInvalidProjectAnnouncement)
	}
	return record, nil
}

func parseProjectAnnouncementKey(key string) (string, string, bool) {
	prefix := objectPrefix + "/"
	if key == "" || !strings.HasPrefix(key, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
	if len(parts) != 3 || parts[1] != projectAnnouncementSegment {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil {
		return "", "", false
	}
	expected, err := ProjectAnnouncementKey(parts[0], parts[2])
	if err != nil || expected != key {
		return "", "", false
	}
	return parts[0], parts[2], true
}
