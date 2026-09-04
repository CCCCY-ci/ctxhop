package sessionhub

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

// MigrationLedgerVersion is the version of the local, metadata-only v1 to
// v2 migration ledger. Version 2 adds the local read-mode preference; version
// 1 ledgers remain readable and are upgraded only when local state is saved.
const MigrationLedgerVersion = 2

const migrationLedgerPreviousVersion = 1

// MigrationStatus describes the durable progress recorded by a migration
// ledger. Status is local progress metadata; it is not a substitute for
// authenticated v1 or v2 Remote objects.
type MigrationStatus string

const (
	MigrationStatusLazy      MigrationStatus = "lazy"
	MigrationStatusPartial   MigrationStatus = "partial"
	MigrationStatusPublished MigrationStatus = "published"
	MigrationStatusBlocked   MigrationStatus = "blocked"
)

// MigrationReadMode controls which local compatibility route is preferred for
// one legacy Session. It is local policy, not a claim about the immutable v1
// or v2 Remote objects.
type MigrationReadMode string

const (
	MigrationReadModeV2     MigrationReadMode = "v2"
	MigrationReadModeLegacy MigrationReadMode = "legacy-v1"
)

var (
	// ErrMigrationLedgerNotFound reports that no local ledger has been written
	// for the requested legacy session.
	ErrMigrationLedgerNotFound = errors.New("sessionhub: migration ledger does not exist")

	// ErrMigrationLedgerCorrupt reports malformed or semantically invalid
	// ledger bytes. A corrupt ledger must not be overwritten by a migration
	// retry because it may be the only local evidence of prior work.
	ErrMigrationLedgerCorrupt = errors.New("sessionhub: migration ledger is corrupt")

	// ErrMigrationLedgerConflict is the common sentinel for a ledger whose
	// stored identity cannot be reconciled with the requested scope or mapping.
	ErrMigrationLedgerConflict = errors.New("sessionhub: migration ledger conflicts with existing state")

	// ErrMigrationLedgerScopeConflict reports a Hub/Project scope mismatch.
	ErrMigrationLedgerScopeConflict = errors.New("sessionhub: migration ledger scope conflict")

	// ErrMigrationLedgerMappingConflict reports a legacy-to-logical Session
	// mapping mismatch. In particular, a retry cannot silently attach one v1
	// session to a different v2 logical Session.
	ErrMigrationLedgerMappingConflict = errors.New("sessionhub: migration ledger mapping conflict")
)

// LegacyMigrationRef is the metadata-only identity of one v1 device branch.
// It deliberately contains no branch records or native session identifier.
type LegacyMigrationRef struct {
	DeviceID         string `json:"deviceId"`
	BranchHeadDigest string `json:"branchHeadDigest"`
	RecordCount      uint64 `json:"recordCount"`
}

// MigrationLedger records local discovery and publication progress for one
// v1 session mapped into one v2 Project. It is a rebuildable local cache, not
// a Remote object and not an ownership record.
type MigrationLedger struct {
	Version           int                  `json:"version"`
	HubID             string               `json:"hubId"`
	ProjectID         string               `json:"projectId"`
	LegacySessionID   string               `json:"legacySessionId"`
	SessionID         string               `json:"sessionId"`
	LegacyRefs        []LegacyMigrationRef `json:"legacyRefs"`
	PublishedReplicas []string             `json:"publishedReplicas"`
	Status            MigrationStatus      `json:"status"`
	ReadMode          MigrationReadMode    `json:"readMode"`
	UpdatedAt         time.Time            `json:"updatedAt"`
}

// migrationLedgerWire keeps timestamps in the same canonical string format
// as the encrypted v2 descriptors. The exported model still uses time.Time
// for callers, while the persisted form remains strict and deterministic.
type migrationLedgerWire struct {
	Version           int                  `json:"version"`
	HubID             string               `json:"hubId"`
	ProjectID         string               `json:"projectId"`
	LegacySessionID   string               `json:"legacySessionId"`
	SessionID         string               `json:"sessionId"`
	LegacyRefs        []LegacyMigrationRef `json:"legacyRefs"`
	PublishedReplicas []string             `json:"publishedReplicas"`
	Status            MigrationStatus      `json:"status"`
	ReadMode          MigrationReadMode    `json:"readMode,omitempty"`
	UpdatedAt         string               `json:"updatedAt"`
}

const (
	maxMigrationRefs       = 1024
	maxPublishedReplicas   = 4096
	migrationLedgerDirName = "migrations"
)

// DeriveLegacySessionLogicalID is the spec-named stable v1 compatibility
// helper. It intentionally delegates to DeriveLegacySessionKey so existing
// callers and already-derived logical IDs retain exactly the same key rule.
func DeriveLegacySessionLogicalID(identifierKey []byte, projectKey, legacySessionID string) (string, error) {
	return DeriveLegacySessionKey(identifierKey, projectKey, legacySessionID)
}

// Validate checks one migration ledger before it is persisted or used as a
// local migration input. It validates metadata and identities only; it never
// reads a native session body or contacts a Remote.
func (l MigrationLedger) Validate() error {
	switch {
	case l.Version == MigrationLedgerVersion:
	case l.Version == migrationLedgerPreviousVersion:
	case l.Version > MigrationLedgerVersion:
		return fmt.Errorf("%w: migration ledger version %d", ErrUnsupportedVersion, l.Version)
	default:
		return fmt.Errorf("%w: migration ledger version %d", ErrInvalidModel, l.Version)
	}
	for name, value := range map[string]string{
		"hub":     l.HubID,
		"project": l.ProjectID,
		"legacy":  l.LegacySessionID,
		"session": l.SessionID,
	} {
		if err := validateOpaqueID(value); err != nil {
			return fmt.Errorf("%w: migration ledger %s id", ErrInvalidIdentity, name)
		}
	}
	if len(l.LegacyRefs) > maxMigrationRefs {
		return fmt.Errorf("%w: too many legacy migration refs", ErrInvalidModel)
	}
	seenDevices := make(map[string]struct{}, len(l.LegacyRefs))
	for _, ref := range l.LegacyRefs {
		if err := validateOpaqueID(ref.DeviceID); err != nil {
			return fmt.Errorf("%w: legacy migration ref device id", ErrInvalidIdentity)
		}
		if err := validateFingerprint(ref.BranchHeadDigest); err != nil {
			return fmt.Errorf("%w: legacy migration ref branch digest", ErrInvalidModel)
		}
		if _, exists := seenDevices[ref.DeviceID]; exists {
			return fmt.Errorf("%w: duplicate legacy migration ref for device %q", ErrInvalidModel, ref.DeviceID)
		}
		seenDevices[ref.DeviceID] = struct{}{}
	}
	if len(l.PublishedReplicas) > maxPublishedReplicas {
		return fmt.Errorf("%w: too many published Replica references", ErrInvalidModel)
	}
	for _, replicaID := range l.PublishedReplicas {
		if err := validateOpaqueID(replicaID); err != nil {
			return fmt.Errorf("%w: published Replica id", ErrInvalidIdentity)
		}
	}
	switch l.Status {
	case MigrationStatusLazy, MigrationStatusPartial, MigrationStatusPublished, MigrationStatusBlocked:
	default:
		return fmt.Errorf("%w: migration ledger status", ErrInvalidModel)
	}
	if l.ReadMode != "" && l.ReadMode != MigrationReadModeV2 && l.ReadMode != MigrationReadModeLegacy {
		return fmt.Errorf("%w: migration ledger read mode", ErrInvalidModel)
	}
	if err := validateTime(l.UpdatedAt); err != nil {
		return fmt.Errorf("%w: migration ledger timestamp", ErrInvalidModel)
	}
	return nil
}

// MarshalBinary encodes a migration ledger as compact, deterministic JSON.
// Legacy refs are sorted by DeviceID and published Replica IDs are
// de-duplicated and sorted before encoding.
func (l MigrationLedger) MarshalBinary() ([]byte, error) {
	canonical, err := canonicalMigrationLedger(l)
	if err != nil {
		return nil, err
	}
	return marshalCompact(migrationLedgerWire{
		Version:           canonical.Version,
		HubID:             canonical.HubID,
		ProjectID:         canonical.ProjectID,
		LegacySessionID:   canonical.LegacySessionID,
		SessionID:         canonical.SessionID,
		LegacyRefs:        canonical.LegacyRefs,
		PublishedReplicas: canonical.PublishedReplicas,
		Status:            canonical.Status,
		ReadMode:          canonical.ReadMode,
		UpdatedAt:         formatTime(canonical.UpdatedAt),
	}, "migration ledger", maxDescriptorBytes)
}

// ParseMigrationLedger strictly decodes a canonical local migration ledger.
// A future version is kept distinguishable from malformed current bytes so a
// caller can fail closed and request a compatible client.
func ParseMigrationLedger(data []byte) (MigrationLedger, error) {
	var wire migrationLedgerWire
	if err := decodeCompact(data, &wire, "migration ledger", maxDescriptorBytes); err != nil {
		return MigrationLedger{}, migrationLedgerCorrupt(err)
	}
	if wire.Version > MigrationLedgerVersion {
		return MigrationLedger{}, fmt.Errorf("%w: migration ledger version %d", ErrUnsupportedVersion, wire.Version)
	}
	updatedAt, err := parseTime(wire.UpdatedAt)
	if err != nil {
		return MigrationLedger{}, migrationLedgerCorrupt(fmt.Errorf("%w: migration ledger timestamp", err))
	}
	ledger := MigrationLedger{
		Version:           wire.Version,
		HubID:             wire.HubID,
		ProjectID:         wire.ProjectID,
		LegacySessionID:   wire.LegacySessionID,
		SessionID:         wire.SessionID,
		LegacyRefs:        wire.LegacyRefs,
		PublishedReplicas: wire.PublishedReplicas,
		Status:            wire.Status,
		ReadMode:          wire.ReadMode,
		UpdatedAt:         updatedAt,
	}
	if err := ledger.Validate(); err != nil {
		if errors.Is(err, ErrUnsupportedVersion) {
			return MigrationLedger{}, err
		}
		return MigrationLedger{}, migrationLedgerCorrupt(err)
	}
	canonical, err := canonicalMigrationLedger(ledger)
	if err != nil {
		return MigrationLedger{}, migrationLedgerCorrupt(err)
	}
	return canonical, nil
}

// MigrationLedgerPath returns the local-only ledger path for one legacy
// session. Every path component derived from session state is an opaque v2 or
// v1 identifier; native IDs, body data and secrets never become path input.
func MigrationLedgerPath(dir, hubID, projectID, legacySessionID string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("sessionhub: migration ledger directory is required")
	}
	for name, value := range map[string]string{
		"hub": hubID, "project": projectID, "legacy session": legacySessionID,
	} {
		if err := validateOpaqueID(value); err != nil {
			return "", fmt.Errorf("%w: migration ledger %s id", ErrInvalidIdentity, name)
		}
	}
	return filepath.Join(dir, "state", "v2", "hubs", hubID, "projects", projectID, migrationLedgerDirName, legacySessionID+".json"), nil
}

// LoadMigrationLedger reads and validates one local ledger without creating
// any local state when it is absent.
func LoadMigrationLedger(dir, hubID, projectID, legacySessionID string) (MigrationLedger, error) {
	path, err := MigrationLedgerPath(dir, hubID, projectID, legacySessionID)
	if err != nil {
		return MigrationLedger{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return MigrationLedger{}, ErrMigrationLedgerNotFound
	}
	if err != nil {
		return MigrationLedger{}, fmt.Errorf("sessionhub: read migration ledger: %w", err)
	}
	ledger, err := ParseMigrationLedger(data)
	if err != nil {
		return MigrationLedger{}, err
	}
	if err := validateMigrationLedgerLookup(ledger, hubID, projectID, legacySessionID); err != nil {
		return MigrationLedger{}, err
	}
	return ledger, nil
}

// SaveMigrationLedger atomically persists one ledger and merges it with an
// existing ledger for the same legacy session. Merging is idempotent:
// device refs are keyed by DeviceID, published Replica IDs are a set, and the
// status never moves backward in local publication progress.
func SaveMigrationLedger(dir string, ledger MigrationLedger) error {
	path, err := MigrationLedgerPath(dir, ledger.HubID, ledger.ProjectID, ledger.LegacySessionID)
	if err != nil {
		return err
	}
	canonical, err := canonicalMigrationLedger(ledger)
	if err != nil {
		return err
	}
	data, err := canonical.MarshalBinary()
	if err != nil {
		return err
	}

	if existingData, readErr := os.ReadFile(path); readErr == nil {
		current, parseErr := ParseMigrationLedger(existingData)
		if parseErr != nil {
			return parseErr
		}
		if err := validateMigrationLedgerLookup(current, canonical.HubID, canonical.ProjectID, canonical.LegacySessionID); err != nil {
			return err
		}
		merged, mergeErr := MergeMigrationLedger(current, canonical)
		if mergeErr != nil {
			return mergeErr
		}
		data, err = merged.MarshalBinary()
		if err != nil {
			return err
		}
		if bytes.Equal(existingData, data) {
			return nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("sessionhub: inspect migration ledger: %w", readErr)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sessionhub: create migration ledger directory: %w", err)
	}
	if err := atomicfile.WriteBytes(path, data); err != nil {
		return fmt.Errorf("sessionhub: write migration ledger: %w", err)
	}
	return nil
}

// MergeMigrationLedger combines two validated ledgers for the same mapping.
// The incoming ref is authoritative for a DeviceID, allowing a later v1
// metadata observation to update its digest/count while preserving all other
// device branches.
func MergeMigrationLedger(existing, incoming MigrationLedger) (MigrationLedger, error) {
	if err := existing.Validate(); err != nil {
		return MigrationLedger{}, err
	}
	if err := incoming.Validate(); err != nil {
		return MigrationLedger{}, err
	}
	if existing.HubID != incoming.HubID || existing.ProjectID != incoming.ProjectID {
		return MigrationLedger{}, migrationLedgerConflict(ErrMigrationLedgerScopeConflict, "Hub/Project scope differs")
	}
	if existing.LegacySessionID != incoming.LegacySessionID || existing.SessionID != incoming.SessionID {
		return MigrationLedger{}, migrationLedgerConflict(ErrMigrationLedgerMappingConflict, "legacy-to-logical Session mapping differs")
	}

	merged := existing
	merged.LegacyRefs = append([]LegacyMigrationRef(nil), existing.LegacyRefs...)
	refsByDevice := make(map[string]LegacyMigrationRef, len(merged.LegacyRefs)+len(incoming.LegacyRefs))
	for _, ref := range merged.LegacyRefs {
		refsByDevice[ref.DeviceID] = ref
	}
	for _, ref := range incoming.LegacyRefs {
		refsByDevice[ref.DeviceID] = ref
	}
	merged.LegacyRefs = merged.LegacyRefs[:0]
	for _, ref := range refsByDevice {
		merged.LegacyRefs = append(merged.LegacyRefs, ref)
	}
	sort.Slice(merged.LegacyRefs, func(i, j int) bool {
		return merged.LegacyRefs[i].DeviceID < merged.LegacyRefs[j].DeviceID
	})

	merged.PublishedReplicas = append(append([]string(nil), existing.PublishedReplicas...), incoming.PublishedReplicas...)
	merged.PublishedReplicas = sortedUniqueMigrationIDs(merged.PublishedReplicas)
	if migrationStatusRank(incoming.Status) > migrationStatusRank(existing.Status) {
		merged.Status = incoming.Status
	}
	if incoming.UpdatedAt.After(existing.UpdatedAt) {
		merged.UpdatedAt = incoming.UpdatedAt
		merged.ReadMode = incoming.ReadMode
	}
	return canonicalMigrationLedger(merged)
}

func canonicalMigrationLedger(l MigrationLedger) (MigrationLedger, error) {
	if err := l.Validate(); err != nil {
		return MigrationLedger{}, err
	}
	copyLedger := l
	copyLedger.LegacyRefs = append([]LegacyMigrationRef(nil), l.LegacyRefs...)
	sort.Slice(copyLedger.LegacyRefs, func(i, j int) bool {
		return copyLedger.LegacyRefs[i].DeviceID < copyLedger.LegacyRefs[j].DeviceID
	})
	copyLedger.PublishedReplicas = sortedUniqueMigrationIDs(l.PublishedReplicas)
	if copyLedger.ReadMode == "" {
		copyLedger.ReadMode = MigrationReadModeV2
	}
	if copyLedger.Version < MigrationLedgerVersion {
		copyLedger.Version = MigrationLedgerVersion
	}
	if copyLedger.LegacyRefs == nil {
		copyLedger.LegacyRefs = []LegacyMigrationRef{}
	}
	if copyLedger.PublishedReplicas == nil {
		copyLedger.PublishedReplicas = []string{}
	}
	copyLedger.UpdatedAt = copyLedger.UpdatedAt.UTC().Round(0)
	return copyLedger, nil
}

func sortedUniqueMigrationIDs(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for _, value := range sorted {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}

func migrationStatusRank(status MigrationStatus) int {
	switch status {
	case MigrationStatusLazy:
		return 1
	case MigrationStatusPartial:
		return 2
	case MigrationStatusBlocked:
		return 3
	case MigrationStatusPublished:
		return 4
	default:
		return 0
	}
}

func validateMigrationLedgerLookup(ledger MigrationLedger, hubID, projectID, legacySessionID string) error {
	if ledger.HubID != hubID || ledger.ProjectID != projectID {
		return migrationLedgerConflict(ErrMigrationLedgerScopeConflict, "stored Hub/Project scope differs from lookup")
	}
	if ledger.LegacySessionID != legacySessionID {
		return migrationLedgerConflict(ErrMigrationLedgerMappingConflict, "stored legacy Session differs from lookup")
	}
	return nil
}

func migrationLedgerConflict(kind error, detail string) error {
	return fmt.Errorf("%w: %s", errors.Join(ErrMigrationLedgerConflict, kind), detail)
}

func migrationLedgerCorrupt(cause error) error {
	if cause == nil {
		return ErrMigrationLedgerCorrupt
	}
	return fmt.Errorf("%w: %w", ErrMigrationLedgerCorrupt, cause)
}
