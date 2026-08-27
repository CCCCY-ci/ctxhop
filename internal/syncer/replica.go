package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

const (
	// v2 descriptors are compact Session Hub envelopes encrypted with the same
	// domain content key as v1 objects. Keep a bound before decryption so a
	// damaged object cannot cause an unbounded allocation.
	maxEncryptedReplicaDescriptorBytes = (1 << 20) + 1024

	maxReplicaShardBytes = maxEncryptedShardBytes
)

var (
	// ErrNoReplicaMetadata reports a v2 Session prefix without a Replica
	// descriptor. A shard-only prefix is never promoted into a Replica.
	ErrNoReplicaMetadata = errors.New("syncer: v2 session has no replica metadata")

	// ErrReplicaDescriptorMissing reports a Replica bootstrap that has not yet
	// published its immutable descriptor.
	ErrReplicaDescriptorMissing = errors.New("syncer: v2 replica descriptor is missing")

	// ErrReplicaTipMissing reports a Replica whose body may exist but whose
	// authenticated complete-prefix tip has not been published.
	ErrReplicaTipMissing = errors.New("syncer: v2 replica tip is missing")

	// ErrReplicaIncomplete reports a Replica that cannot be proven complete from
	// its descriptor, tip and visible shard sequence.
	ErrReplicaIncomplete = errors.New("syncer: v2 replica is incomplete")

	// ErrReplicaImmutableConflict prevents an existing immutable descriptor or
	// shard from being replaced. A caller with a retained private identity can
	// use the WithIdentities form to turn an identical retry into success.
	ErrReplicaImmutableConflict = errors.New("syncer: v2 immutable replica object conflicts")

	// ErrDuplicateReplicaShard reports two remote listing entries claiming one
	// Replica sequence number.
	ErrDuplicateReplicaShard = errors.New("syncer: duplicate v2 replica shard")

	// ErrReplicaIdentityMismatch reports a valid object placed under a
	// different v2 identity tuple.
	ErrReplicaIdentityMismatch = errors.New("syncer: v2 replica identity mismatch")

	// ErrReplicaObjectTooLarge reports an encrypted Replica object that exceeds
	// the bounded size accepted by this layer.
	ErrReplicaObjectTooLarge = errors.New("syncer: v2 replica object is too large")
)

// ReplicaMetadata is the metadata-only view of one NativeReplica. A missing
// Tip means the Replica is still a bootstrap/incomplete candidate; callers
// must not use it for restore.
type ReplicaMetadata struct {
	Layout     ReplicaLayout
	Descriptor sessionhub.NativeReplicaDescriptor
	Tip        *sessionhub.ReplicaTip
}

// ProjectReplicaMetadataRef groups metadata-only Replica views by logical
// Session. It is the v2 counterpart of ProjectMetadataRef and never includes
// shard bodies.
type ProjectReplicaMetadataRef struct {
	SessionID         string
	SessionDescriptor *sessionhub.SessionDescriptor
	Replicas          []ReplicaMetadata
}

// ReplicaSnapshot is returned only after every visible shard has been
// decrypted, assembled and checked against the authenticated tip.
type ReplicaSnapshot struct {
	Layout     ReplicaLayout
	Descriptor sessionhub.NativeReplicaDescriptor
	Tip        sessionhub.ReplicaTip
	Records    [][]byte
	HeadDigest [32]byte
}

// ReplicaPushOptions controls one source-native Replica append. Identities
// are optional for a first unattended write. When supplied, they let a retry
// verify an existing immutable object instead of replacing it.
type ReplicaPushOptions struct {
	Plan       PlanOptions
	Identities []*ecdh.PrivateKey
	Now        time.Time
}

// DefaultReplicaPushOptions returns the same conservative shard limits as v1.
func DefaultReplicaPushOptions() ReplicaPushOptions {
	return ReplicaPushOptions{Plan: DefaultPlanOptions()}
}

// ReplicaPushResult reports the last durable local cursor and the tip that was
// published, if the operation reached that step. On an error, Cursor still
// describes the last shard known to be durable in the local execution.
type ReplicaPushResult struct {
	Cursor          PushCursor
	Tip             sessionhub.ReplicaTip
	PublishedShards int
}

// PutHubDescriptor publishes this device's encrypted Hub descriptor. The
// descriptor path is device-owned, so another device can publish its own
// descriptor without racing on one mutable object.
func PutHubDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.HubDescriptor) error {
	key, err := layout.HubDescriptorKey()
	if err != nil {
		return err
	}
	if descriptor.HubID != layout.hubKey {
		return fmt.Errorf("%w: Hub descriptor belongs to another Hub", ErrReplicaIdentityMismatch)
	}
	return putHubDescriptorAt(ctx, store, recipient, key, descriptor)
}

// PutHubDescriptorForDevice publishes a Hub descriptor without requiring a
// Session or Replica key. Hub metadata is device-owned and therefore belongs
// at the Hub level, not once per Session.
func PutHubDescriptorForDevice(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, hubKey, deviceID string, descriptor sessionhub.HubDescriptor) error {
	if err := validateIdentifier(hubKey); err != nil {
		return fmt.Errorf("%w: hub key: %v", errInvalidReplicaLayout, err)
	}
	if descriptor.HubID != hubKey {
		return fmt.Errorf("%w: Hub descriptor belongs to another Hub", ErrReplicaIdentityMismatch)
	}
	if err := validateIdentifier(deviceID); err != nil {
		return fmt.Errorf("%w: device key: %v", errInvalidReplicaLayout, err)
	}
	key := v2ObjectPrefix + "/" + hubKey + "/descriptors/" + deviceID + "/" + descriptorMetaName
	return putHubDescriptorAt(ctx, store, recipient, key, descriptor)
}

func putHubDescriptorAt(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, key string, descriptor sessionhub.HubDescriptor) error {
	payload, err := descriptor.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Hub descriptor: %w", err)
	}
	return putReplicaMutable(ctx, store, recipient, key, payload, "Hub descriptor")
}

// PutProjectDescriptor publishes this device's encrypted Project descriptor.
func PutProjectDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.ProjectDescriptor) error {
	key, err := layout.ProjectDescriptorKey()
	if err != nil {
		return err
	}
	if descriptor.HubID != layout.hubKey || descriptor.ProjectID != layout.projectKey {
		return fmt.Errorf("%w: Project descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return putProjectDescriptorAt(ctx, store, recipient, key, descriptor)
}

// PutProjectDescriptorForDevice publishes a Project descriptor at the
// Project level without requiring a child Session/Replica layout.
func PutProjectDescriptorForDevice(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ProjectHubLayout, deviceID string, descriptor sessionhub.ProjectDescriptor) error {
	key, err := layout.DescriptorKey(deviceID)
	if err != nil {
		return err
	}
	if descriptor.HubID != layout.hubKey || descriptor.ProjectID != layout.projectKey {
		return fmt.Errorf("%w: Project descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return putProjectDescriptorAt(ctx, store, recipient, key, descriptor)
}

func putProjectDescriptorAt(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, key string, descriptor sessionhub.ProjectDescriptor) error {
	payload, err := descriptor.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Project descriptor: %w", err)
	}
	return putReplicaMutable(ctx, store, recipient, key, payload, "Project descriptor")
}

// PutSessionDescriptor publishes this device's encrypted logical Session
// descriptor.
func PutSessionDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.SessionDescriptor) error {
	key, err := layout.SessionDescriptorKey()
	if err != nil {
		return err
	}
	if descriptor.SessionID != layout.sessionKey || descriptor.ProjectID != layout.projectKey {
		return fmt.Errorf("%w: Session descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return putSessionDescriptorAt(ctx, store, recipient, key, descriptor)
}

// PutSessionDescriptorForDevice publishes a logical Session descriptor
// without requiring a Replica key.
func PutSessionDescriptorForDevice(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, deviceID string, descriptor sessionhub.SessionDescriptor) error {
	key, err := layout.DescriptorKey(deviceID)
	if err != nil {
		return err
	}
	if descriptor.SessionID != layout.sessionKey || descriptor.ProjectID != layout.projectKey {
		return fmt.Errorf("%w: Session descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return putSessionDescriptorAt(ctx, store, recipient, key, descriptor)
}

func putSessionDescriptorAt(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, key string, descriptor sessionhub.SessionDescriptor) error {
	payload, err := descriptor.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Session descriptor: %w", err)
	}
	return putReplicaMutable(ctx, store, recipient, key, payload, "Session descriptor")
}

// PutReplicaDescriptor publishes the immutable NativeReplica identity. An
// existing object is never replaced. Callers that have a private content key
// should use PutReplicaDescriptorWithIdentities for safe identical retries.
func PutReplicaDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor) error {
	return putReplicaDescriptor(ctx, store, recipient, layout, descriptor, nil)
}

// PutReplicaDescriptorWithIdentities publishes or verifies one immutable
// Replica descriptor. An existing descriptor is accepted only when it opens
// to the exact same canonical descriptor with one supplied identity.
func PutReplicaDescriptorWithIdentities(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, identities []*ecdh.PrivateKey) error {
	if err := validateIdentities(identities); err != nil {
		return err
	}
	return putReplicaDescriptor(ctx, store, recipient, layout, descriptor, identities)
}

func putReplicaDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, identities []*ecdh.PrivateKey) error {
	key, err := layout.ReplicaDescriptorKey()
	if err != nil {
		return err
	}
	if err := validateReplicaDescriptorForLayout(layout, descriptor); err != nil {
		return err
	}
	payload, err := descriptor.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Replica descriptor: %w", err)
	}
	sealed, err := sealReplicaPayload(recipient, key, payload)
	if err != nil {
		return fmt.Errorf("syncer: encrypt v2 Replica descriptor: %w", err)
	}
	return putReplicaImmutable(ctx, store, key, sealed, maxEncryptedReplicaDescriptorBytes, identities, func(existing []byte) error {
		opened, err := openReplicaPayloadWithIdentities(identities, key, existing)
		if err != nil {
			return err
		}
		parsed, err := sessionhub.ParseNativeReplicaDescriptor(opened)
		if err != nil {
			return err
		}
		canonical, err := parsed.MarshalBinary()
		if err != nil || !bytes.Equal(canonical, payload) {
			return ErrReplicaImmutableConflict
		}
		return nil
	})
}

// PutReplicaTip publishes the device-owned authenticated complete-prefix tip.
// Unlike a descriptor or shard, the tip is deliberately replaceable: it is
// advanced only after all shards it covers are durable.
func PutReplicaTip(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, tip sessionhub.ReplicaTip) error {
	key, err := layout.ReplicaTipKey()
	if err != nil {
		return err
	}
	if tip.ReplicaID != layout.replicaKey {
		return fmt.Errorf("%w: tip belongs to another Replica", ErrReplicaIdentityMismatch)
	}
	payload, err := tip.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Replica tip: %w", err)
	}
	return putReplicaMutable(ctx, store, recipient, key, payload, "Replica tip")
}

// PutReplicaShard publishes one cursor-checked immutable Replica shard.
// Existing objects are never overwritten. Use PutReplicaShardWithIdentities
// when a retry must prove that an already-present object is identical.
func PutReplicaShard(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, cursor PushCursor, part ShardPart) (PushCursor, error) {
	return putReplicaShard(ctx, store, recipient, layout, cursor, part, nil)
}

// PutReplicaShardWithIdentities publishes or verifies one immutable Replica
// shard using a retained content identity for an identical retry.
func PutReplicaShardWithIdentities(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, cursor PushCursor, part ShardPart, identities []*ecdh.PrivateKey) (PushCursor, error) {
	if err := validateIdentities(identities); err != nil {
		return PushCursor{}, err
	}
	return putReplicaShard(ctx, store, recipient, layout, cursor, part, identities)
}

func putReplicaShard(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, cursor PushCursor, part ShardPart, identities []*ecdh.PrivateKey) (PushCursor, error) {
	if err := validateReplicaWriteArgs(ctx, store, recipient); err != nil {
		return PushCursor{}, err
	}
	if err := layout.validate(); err != nil {
		return PushCursor{}, err
	}
	next, err := cursor.Advance(part)
	if err != nil {
		return PushCursor{}, err
	}
	key, err := layout.ReplicaShardKey(part.Number)
	if err != nil {
		return PushCursor{}, err
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: publish v2 Replica shard: %w", err)
	}
	sealed, err := SealShard(recipient, key, part.Shard)
	if err != nil {
		return PushCursor{}, fmt.Errorf("syncer: encrypt v2 Replica shard: %w", err)
	}
	verify := func(existing []byte) error {
		opened, err := openShardWithIdentities(identities, key, existing)
		if err != nil {
			return err
		}
		if !sameShard(opened, part.Shard) {
			return ErrReplicaImmutableConflict
		}
		return nil
	}
	if err := putReplicaImmutable(ctx, store, key, sealed, maxReplicaShardBytes, identities, verify); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: publish v2 Replica shard %d: %w", part.Number, err)
	}
	return next, nil
}

// PushReplica publishes a complete source-native append. It plans and
// validates the local suffix before any remote write, writes the descriptor,
// appends immutable shards, and publishes the tip last.
func PushReplica(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, cursor PushCursor, records [][]byte, options ReplicaPushOptions) (ReplicaPushResult, error) {
	return pushReplica(ctx, store, recipient, layout, descriptor, cursor, records, options, nil)
}

func pushReplica(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, cursor PushCursor, records [][]byte, options ReplicaPushOptions, saveCursor func(PushCursor) error) (ReplicaPushResult, error) {
	result := ReplicaPushResult{Cursor: cursor}
	if err := validateReplicaWriteArgs(ctx, store, recipient); err != nil {
		return ReplicaPushResult{}, err
	}
	if err := validateReplicaDescriptorForLayout(layout, descriptor); err != nil {
		return ReplicaPushResult{}, err
	}
	if options.Plan.MaxRecords == 0 && options.Plan.MaxEncodedBytes == 0 {
		options.Plan = DefaultPlanOptions()
	}
	plan, err := PlanAppend(cursor, records, options.Plan)
	if err != nil {
		return ReplicaPushResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReplicaPushResult{}, fmt.Errorf("syncer: push v2 Replica: %w", err)
	}
	if saveCursor != nil {
		// A cursor-backed writer owns this Replica namespace. On a normal
		// subsequent push the descriptor is already present, and the local
		// cursor is the durable proof that this writer has initialized it. If
		// private identities are available, still authenticate and compare the
		// descriptor; without them, never overwrite it and let body shards
		// remain fail-closed if they need verification.
		err = ensureReplicaDescriptor(ctx, store, recipient, layout, descriptor, options.Identities)
	} else if len(options.Identities) == 0 {
		err = PutReplicaDescriptor(ctx, store, recipient, layout, descriptor)
	} else {
		err = PutReplicaDescriptorWithIdentities(ctx, store, recipient, layout, descriptor, options.Identities)
	}
	if err != nil {
		return result, err
	}

	durable := cursor
	for _, part := range plan.Parts {
		var next PushCursor
		if len(options.Identities) == 0 {
			next, err = PutReplicaShard(ctx, store, recipient, layout, durable, part)
		} else {
			next, err = PutReplicaShardWithIdentities(ctx, store, recipient, layout, durable, part, options.Identities)
		}
		if err != nil {
			result.Cursor = durable
			result.PublishedShards = int(durable.NextShard - cursor.NextShard)
			return result, err
		}
		durable = next
		if saveCursor != nil {
			if err := saveCursor(durable); err != nil {
				result.Cursor = durable
				result.PublishedShards = int(durable.NextShard - cursor.NextShard)
				return result, errors.Join(ErrReplicaCursorCommit, err)
			}
		}
	}

	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if saveCursor != nil && len(plan.Parts) == 0 {
		if err := saveCursor(durable); err != nil {
			return result, errors.Join(ErrReplicaCursorCommit, err)
		}
	}
	tip := replicaTipFor(descriptor.ReplicaID, durable, options.Now)
	if err := PutReplicaTip(ctx, store, recipient, layout, tip); err != nil {
		result.Cursor = durable
		result.Tip = tip
		result.PublishedShards = int(durable.NextShard - cursor.NextShard)
		return result, err
	}
	result.Cursor = durable
	result.Tip = tip
	result.PublishedShards = len(plan.Parts)
	return result, nil
}

func ensureReplicaDescriptor(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor, identities []*ecdh.PrivateKey) error {
	if len(identities) != 0 {
		return PutReplicaDescriptorWithIdentities(ctx, store, recipient, layout, descriptor, identities)
	}
	key, err := layout.ReplicaDescriptorKey()
	if err != nil {
		return err
	}
	if err := validateReplicaDescriptorForLayout(layout, descriptor); err != nil {
		return err
	}
	if _, err := store.Stat(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, remote.ErrNotFound) {
		return fmt.Errorf("check existing v2 Replica descriptor: %w", err)
	}
	return PutReplicaDescriptor(ctx, store, recipient, layout, descriptor)
}

// FetchHubDescriptor reads this device's encrypted Hub descriptor.
func FetchHubDescriptor(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (sessionhub.HubDescriptor, error) {
	key, err := layout.HubDescriptorKey()
	if err != nil {
		return sessionhub.HubDescriptor{}, err
	}
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Hub descriptor")
	if err != nil {
		return sessionhub.HubDescriptor{}, err
	}
	descriptor, err := sessionhub.ParseHubDescriptor(payload)
	if err != nil {
		return sessionhub.HubDescriptor{}, fmt.Errorf("syncer: parse v2 Hub descriptor: %w", err)
	}
	if descriptor.HubID != layout.hubKey {
		return sessionhub.HubDescriptor{}, fmt.Errorf("%w: Hub descriptor belongs to another Hub", ErrReplicaIdentityMismatch)
	}
	return descriptor, nil
}

// FetchProjectDescriptor reads this device's encrypted Project descriptor.
func FetchProjectDescriptor(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (sessionhub.ProjectDescriptor, error) {
	key, err := layout.ProjectDescriptorKey()
	if err != nil {
		return sessionhub.ProjectDescriptor{}, err
	}
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Project descriptor")
	if err != nil {
		return sessionhub.ProjectDescriptor{}, err
	}
	descriptor, err := sessionhub.ParseProjectDescriptor(payload)
	if err != nil {
		return sessionhub.ProjectDescriptor{}, fmt.Errorf("syncer: parse v2 Project descriptor: %w", err)
	}
	if descriptor.HubID != layout.hubKey || descriptor.ProjectID != layout.projectKey {
		return sessionhub.ProjectDescriptor{}, fmt.Errorf("%w: Project descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return descriptor, nil
}

// FetchSessionDescriptor reads this device's encrypted logical Session
// descriptor.
func FetchSessionDescriptor(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (sessionhub.SessionDescriptor, error) {
	sessionLayout, err := layout.SessionLayout()
	if err != nil {
		return sessionhub.SessionDescriptor{}, err
	}
	return FetchSessionDescriptorForDevice(ctx, store, sessionLayout, layout.deviceID, identities)
}

// FetchSessionDescriptorForDevice reads a logical Session descriptor without
// requiring a Replica key. This is used when project-level listing discovers
// a Session descriptor before any Replica body exists.
func FetchSessionDescriptorForDevice(ctx context.Context, store remote.Remote, layout SessionHubLayout, deviceID string, identities []*ecdh.PrivateKey) (sessionhub.SessionDescriptor, error) {
	key, err := layout.DescriptorKey(deviceID)
	if err != nil {
		return sessionhub.SessionDescriptor{}, err
	}
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Session descriptor")
	if err != nil {
		return sessionhub.SessionDescriptor{}, err
	}
	descriptor, err := sessionhub.ParseSessionDescriptor(payload)
	if err != nil {
		return sessionhub.SessionDescriptor{}, fmt.Errorf("syncer: parse v2 Session descriptor: %w", err)
	}
	if descriptor.ProjectID != layout.projectKey || descriptor.SessionID != layout.sessionKey {
		return sessionhub.SessionDescriptor{}, fmt.Errorf("%w: Session descriptor belongs to another namespace", ErrReplicaIdentityMismatch)
	}
	return descriptor, nil
}

// FetchReplicaDescriptor reads one immutable Replica descriptor.
func FetchReplicaDescriptor(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (sessionhub.NativeReplicaDescriptor, error) {
	key, err := layout.ReplicaDescriptorKey()
	if err != nil {
		return sessionhub.NativeReplicaDescriptor{}, err
	}
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Replica descriptor")
	if errors.Is(err, remote.ErrNotFound) {
		return sessionhub.NativeReplicaDescriptor{}, fmt.Errorf("%w: %w", ErrReplicaDescriptorMissing, err)
	}
	if err != nil {
		return sessionhub.NativeReplicaDescriptor{}, err
	}
	descriptor, err := sessionhub.ParseNativeReplicaDescriptor(payload)
	if err != nil {
		return sessionhub.NativeReplicaDescriptor{}, fmt.Errorf("syncer: parse v2 Replica descriptor: %w", err)
	}
	if err := validateReplicaDescriptorForLayout(layout, descriptor); err != nil {
		return sessionhub.NativeReplicaDescriptor{}, err
	}
	return descriptor, nil
}

// FetchReplicaTip reads one authenticated Replica tip.
func FetchReplicaTip(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (sessionhub.ReplicaTip, error) {
	key, err := layout.ReplicaTipKey()
	if err != nil {
		return sessionhub.ReplicaTip{}, err
	}
	payload, err := fetchReplicaPayload(ctx, store, key, identities, "Replica tip")
	if errors.Is(err, remote.ErrNotFound) {
		return sessionhub.ReplicaTip{}, fmt.Errorf("%w: %w", ErrReplicaTipMissing, err)
	}
	if err != nil {
		return sessionhub.ReplicaTip{}, err
	}
	tip, err := sessionhub.ParseReplicaTip(payload)
	if err != nil {
		return sessionhub.ReplicaTip{}, fmt.Errorf("syncer: parse v2 Replica tip: %w", err)
	}
	if tip.ReplicaID != layout.replicaKey {
		return sessionhub.ReplicaTip{}, fmt.Errorf("%w: tip belongs to another Replica", ErrReplicaIdentityMismatch)
	}
	return tip, nil
}

// FetchReplicaMetadata reads only descriptor and tip. It never lists or gets
// Replica shards. A missing tip is represented by a nil Tip so list/show can
// display an incomplete bootstrap without claiming it is restorable.
func FetchReplicaMetadata(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (ReplicaMetadata, error) {
	descriptor, err := FetchReplicaDescriptor(ctx, store, layout, identities)
	if err != nil {
		return ReplicaMetadata{}, err
	}
	metadata := ReplicaMetadata{Layout: layout, Descriptor: descriptor}
	tip, err := FetchReplicaTip(ctx, store, layout, identities)
	if errors.Is(err, ErrReplicaTipMissing) {
		return metadata, nil
	}
	if err != nil {
		return ReplicaMetadata{}, err
	}
	metadata.Tip = &tip
	return metadata, nil
}

// FetchSessionReplicaMetadata lists all v2 Replica descriptors below one
// logical Session and reads only their descriptor/tip objects. Shards are
// intentionally ignored, so this is safe for session list/show paths.
func FetchSessionReplicaMetadata(ctx context.Context, store remote.Remote, layout SessionHubLayout, identities []*ecdh.PrivateKey) ([]ReplicaMetadata, error) {
	return FetchSessionReplicaMetadataWithDevices(ctx, store, layout, identities, nil)
}

// FetchSessionReplicaMetadataWithDevices is the device-filtered form used by
// callers that have already evaluated the current membership policy.
func FetchSessionReplicaMetadataWithDevices(ctx context.Context, store remote.Remote, layout SessionHubLayout, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]ReplicaMetadata, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	prefix, err := layout.SessionPrefix()
	if err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list v2 Replica metadata: %w", err)
	}
	refs, err := collectReplicaDescriptorRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, ErrNoReplicaMetadata
	}
	keys := make([]replicaRefKey, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ReplicaKey != keys[j].ReplicaKey {
			return keys[i].ReplicaKey < keys[j].ReplicaKey
		}
		return keys[i].DeviceID < keys[j].DeviceID
	})

	metadata := make([]ReplicaMetadata, 0, len(keys))
	for _, key := range keys {
		if allowed != nil {
			if _, ok := allowed[key.DeviceID]; !ok {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("syncer: read v2 Replica metadata: %w", err)
		}
		replicaLayout, err := layout.Replica(key.ReplicaKey, key.DeviceID)
		if err != nil {
			return nil, err
		}
		ref, err := FetchReplicaMetadata(ctx, store, replicaLayout, identities)
		if err != nil {
			return nil, err
		}
		metadata = append(metadata, ref)
	}
	if len(metadata) == 0 {
		return nil, ErrNoReplicaMetadata
	}
	return metadata, nil
}

// FetchProjectReplicaMetadata lists every v2 Replica descriptor below one
// logical Project and reads only each descriptor/tip pair. The logical Session
// key is taken from the object path, while all body shards remain untouched.
func FetchProjectReplicaMetadata(ctx context.Context, store remote.Remote, layout ProjectHubLayout, identities []*ecdh.PrivateKey) ([]ProjectReplicaMetadataRef, error) {
	return FetchProjectReplicaMetadataWithDevices(ctx, store, layout, identities, nil)
}

// FetchProjectReplicaMetadataWithDevices is the device-filtered form used by
// project list/show views.
func FetchProjectReplicaMetadataWithDevices(ctx context.Context, store remote.Remote, layout ProjectHubLayout, identities []*ecdh.PrivateKey, allowed map[string]struct{}) ([]ProjectReplicaMetadataRef, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	prefix, err := layout.ProjectPrefix()
	if err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list v2 Project Replica metadata: %w", err)
	}
	refs, err := collectProjectReplicaDescriptorRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	sessionDescriptors, err := collectProjectSessionDescriptorRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 && len(sessionDescriptors) == 0 {
		return nil, ErrNoReplicaMetadata
	}

	sessionKeySet := make(map[string]struct{}, len(refs)+len(sessionDescriptors))
	for sessionKey := range refs {
		sessionKeySet[sessionKey] = struct{}{}
	}
	for sessionKey := range sessionDescriptors {
		sessionKeySet[sessionKey] = struct{}{}
	}
	sessionKeys := make([]string, 0, len(sessionKeySet))
	for sessionKey := range sessionKeySet {
		sessionKeys = append(sessionKeys, sessionKey)
	}
	sort.Strings(sessionKeys)
	result := make([]ProjectReplicaMetadataRef, 0, len(sessionKeys))
	for _, sessionKey := range sessionKeys {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("syncer: read v2 Project Replica metadata: %w", err)
		}
		sessionLayout, err := layout.Session(sessionKey)
		if err != nil {
			return nil, err
		}
		keys := make([]replicaRefKey, 0, len(refs[sessionKey]))
		for key := range refs[sessionKey] {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].ReplicaKey != keys[j].ReplicaKey {
				return keys[i].ReplicaKey < keys[j].ReplicaKey
			}
			return keys[i].DeviceID < keys[j].DeviceID
		})

		replicas := make([]ReplicaMetadata, 0, len(keys))
		for _, key := range keys {
			if allowed != nil {
				if _, ok := allowed[key.DeviceID]; !ok {
					continue
				}
			}
			replicaLayout, err := sessionLayout.Replica(key.ReplicaKey, key.DeviceID)
			if err != nil {
				return nil, err
			}
			metadata, err := FetchReplicaMetadata(ctx, store, replicaLayout, identities)
			if err != nil {
				return nil, err
			}
			replicas = append(replicas, metadata)
		}
		var sessionDescriptor *sessionhub.SessionDescriptor
		deviceIDs := make([]string, 0, len(sessionDescriptors[sessionKey]))
		for deviceID := range sessionDescriptors[sessionKey] {
			deviceIDs = append(deviceIDs, deviceID)
		}
		sort.Strings(deviceIDs)
		for _, deviceID := range deviceIDs {
			if allowed != nil {
				if _, ok := allowed[deviceID]; !ok {
					continue
				}
			}
			descriptor, descriptorErr := FetchSessionDescriptorForDevice(ctx, store, sessionLayout, deviceID, identities)
			if descriptorErr == nil {
				sessionDescriptor = &descriptor
				break
			}
			if !errors.Is(descriptorErr, remote.ErrNotFound) {
				return nil, descriptorErr
			}
		}
		if sessionDescriptor == nil && len(replicas) != 0 {
			// A pre-v2 writer may have published Replica metadata before the
			// logical descriptor. Keep the Replica visible as an incomplete
			// bootstrap candidate.
			descriptor, descriptorErr := FetchSessionDescriptor(ctx, store, replicas[0].Layout, identities)
			if descriptorErr == nil {
				sessionDescriptor = &descriptor
			} else if !errors.Is(descriptorErr, remote.ErrNotFound) {
				return nil, descriptorErr
			}
		}
		if len(replicas) != 0 || sessionDescriptor != nil {
			result = append(result, ProjectReplicaMetadataRef{SessionID: sessionKey, SessionDescriptor: sessionDescriptor, Replicas: replicas})
		}
	}
	if len(result) == 0 {
		return nil, ErrNoReplicaMetadata
	}
	return result, nil
}

// FetchCompleteReplica reads, verifies and assembles one full Replica. It is
// deliberately separate from FetchReplicaMetadata so list/pull cannot
// accidentally download session body data.
func FetchCompleteReplica(ctx context.Context, store remote.Remote, layout ReplicaLayout, identities []*ecdh.PrivateKey) (ReplicaSnapshot, error) {
	if ctx == nil {
		return ReplicaSnapshot{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return ReplicaSnapshot{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return ReplicaSnapshot{}, err
	}
	descriptor, err := FetchReplicaDescriptor(ctx, store, layout, identities)
	if err != nil {
		return ReplicaSnapshot{}, errors.Join(ErrReplicaIncomplete, err)
	}
	tip, err := FetchReplicaTip(ctx, store, layout, identities)
	if err != nil {
		return ReplicaSnapshot{}, errors.Join(ErrReplicaIncomplete, err)
	}
	prefix, err := layout.ReplicaPrefix()
	if err != nil {
		return ReplicaSnapshot{}, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return ReplicaSnapshot{}, fmt.Errorf("%w: list shards: %w", ErrReplicaIncomplete, err)
	}
	refs, err := collectReplicaShardRefs(prefix, objects)
	if err != nil {
		return ReplicaSnapshot{}, fmt.Errorf("%w: %v", ErrReplicaIncomplete, err)
	}
	if tip.RecordCount == 0 {
		if len(refs) != 0 || tip.ShardCount != 0 || tip.LastShard != 0 || tip.HeadDigest != hexDigest(EmptyDigest()) {
			return ReplicaSnapshot{}, fmt.Errorf("%w: empty tip does not match visible shards", ErrReplicaIncomplete)
		}
		return ReplicaSnapshot{Layout: layout, Descriptor: descriptor, Tip: tip, HeadDigest: EmptyDigest()}, nil
	}
	if len(refs) == 0 {
		return ReplicaSnapshot{}, fmt.Errorf("%w: tip expects shards but none are visible", ErrReplicaIncomplete)
	}
	numbers := make([]uint64, 0, len(refs))
	for number := range refs {
		numbers = append(numbers, number)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	builder := newBranchBuilder(layout.deviceID)
	for _, number := range numbers {
		if err := ctx.Err(); err != nil {
			return ReplicaSnapshot{}, fmt.Errorf("%w: read shards: %w", ErrReplicaIncomplete, err)
		}
		key := refs[number]
		sealed, err := readReplicaObject(ctx, store, key, maxReplicaShardBytes)
		if err != nil {
			return ReplicaSnapshot{}, fmt.Errorf("%w: read shard %d: %w", ErrReplicaIncomplete, number, err)
		}
		shard, err := openShardWithIdentities(identities, key, sealed)
		if err != nil {
			return ReplicaSnapshot{}, fmt.Errorf("%w: open shard %d: %w", ErrReplicaIncomplete, number, err)
		}
		if err := builder.append(number, shard); err != nil {
			return ReplicaSnapshot{}, fmt.Errorf("%w: assemble shard %d: %w", ErrReplicaIncomplete, number, err)
		}
	}
	branch, err := builder.finish()
	if err != nil {
		return ReplicaSnapshot{}, fmt.Errorf("%w: finish branch: %w", ErrReplicaIncomplete, err)
	}
	if uint64(len(refs)) != tip.ShardCount || numbers[len(numbers)-1] != tip.LastShard || uint64(len(branch.Records)) != tip.RecordCount || branch.HeadDigest != digestFromString(tip.HeadDigest) {
		return ReplicaSnapshot{}, fmt.Errorf("%w: tip does not match complete shard sequence", ErrReplicaIncomplete)
	}
	return ReplicaSnapshot{Layout: layout, Descriptor: descriptor, Tip: tip, Records: branch.Records, HeadDigest: branch.HeadDigest}, nil
}

func validateReplicaDescriptorForLayout(layout ReplicaLayout, descriptor sessionhub.NativeReplicaDescriptor) error {
	if err := layout.validate(); err != nil {
		return err
	}
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("syncer: validate v2 Replica descriptor: %w", err)
	}
	if descriptor.ReplicaID != layout.replicaKey || descriptor.SessionID != layout.sessionKey || descriptor.Source.DeviceID != layout.deviceID {
		return fmt.Errorf("%w: Replica descriptor does not match its object namespace", ErrReplicaIdentityMismatch)
	}
	return nil
}

func replicaTipFor(replicaID string, cursor PushCursor, now time.Time) sessionhub.ReplicaTip {
	shardCount := cursor.NextShard - 1
	lastShard := shardCount
	return sessionhub.ReplicaTip{
		Version:     sessionhub.ModelVersion,
		ReplicaID:   replicaID,
		RecordCount: cursor.RecordCount,
		ShardCount:  shardCount,
		LastShard:   lastShard,
		HeadDigest:  hexDigest(cursor.HeadDigest),
		UpdatedAt:   now.UTC().Round(0),
	}
}

func validateReplicaWriteArgs(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	return nil
}

func putReplicaMutable(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, key string, payload []byte, kind string) error {
	if err := validateReplicaWriteArgs(ctx, store, recipient); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish v2 %s: %w", kind, err)
	}
	sealed, err := sealReplicaPayload(recipient, key, payload)
	if err != nil {
		return fmt.Errorf("syncer: encrypt v2 %s: %w", kind, err)
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish v2 %s: %w", kind, err)
	}
	return nil
}

func putReplicaImmutable(ctx context.Context, store remote.Remote, key string, sealed []byte, maxExistingBytes int64, identities []*ecdh.PrivateKey, verify func([]byte) error) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := store.Stat(ctx, key)
	switch {
	case errors.Is(err, remote.ErrNotFound):
		if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
			return err
		}
		return nil
	case err != nil:
		return fmt.Errorf("check immutable v2 object: %w", err)
	case verify == nil || len(identities) == 0:
		return fmt.Errorf("%w: %s already exists and cannot be verified without a private identity", ErrReplicaImmutableConflict, key)
	default:
		existing, err := readReplicaObject(ctx, store, key, maxExistingBytes)
		if err != nil {
			return fmt.Errorf("read immutable v2 object for retry: %w", err)
		}
		if err := verify(existing); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrReplicaImmutableConflict, key, safeReplicaVerifyError(err))
		}
		return nil
	}
}

func sealReplicaPayload(recipient *ecdh.PublicKey, key string, payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > maxEncryptedReplicaDescriptorBytes {
		return nil, fmt.Errorf("%w: plaintext descriptor is too large", ErrReplicaObjectTooLarge)
	}
	sealed, err := crypto.Encrypt(recipient, key, payload)
	if err != nil {
		return nil, err
	}
	if len(sealed) > maxEncryptedReplicaDescriptorBytes {
		return nil, fmt.Errorf("%w: encrypted descriptor exceeds %d bytes", ErrReplicaObjectTooLarge, maxEncryptedReplicaDescriptorBytes)
	}
	return sealed, nil
}

func fetchReplicaPayload(ctx context.Context, store remote.Remote, key string, identities []*ecdh.PrivateKey, kind string) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	sealed, err := readReplicaObject(ctx, store, key, maxEncryptedReplicaDescriptorBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: read v2 %s: %w", kind, err)
	}
	payload, err := openReplicaPayloadWithIdentities(identities, key, sealed)
	if err != nil {
		return nil, fmt.Errorf("syncer: decrypt v2 %s: %w", kind, err)
	}
	return payload, nil
}

func openReplicaPayloadWithIdentities(identities []*ecdh.PrivateKey, key string, sealed []byte) ([]byte, error) {
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	var last error
	for _, identity := range identities {
		payload, err := crypto.Decrypt(identity, key, sealed)
		if err == nil {
			return payload, nil
		}
		last = err
	}
	return nil, last
}

func readReplicaObject(ctx context.Context, store remote.Remote, key string, maxBytes int64) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("read v2 object: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("read v2 object: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close v2 object: %w", closeErr)
	}
	if len(data) > int(maxBytes) {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrReplicaObjectTooLarge, maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type replicaRefKey struct {
	ReplicaKey string
	DeviceID   string
}

func collectProjectReplicaDescriptorRefs(prefix string, objects []remote.ObjectInfo) (map[string]map[replicaRefKey]string, error) {
	refs := make(map[string]map[replicaRefKey]string)
	for _, object := range objects {
		sessionKey, replicaKey, deviceID, ok := parseProjectReplicaDescriptorObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		byReplica := refs[sessionKey]
		if byReplica == nil {
			byReplica = make(map[replicaRefKey]string)
			refs[sessionKey] = byReplica
		}
		ref := replicaRefKey{ReplicaKey: replicaKey, DeviceID: deviceID}
		if _, exists := byReplica[ref]; exists {
			return nil, fmt.Errorf("%w: descriptor for Session %q, Replica %q and device %q", ErrReplicaImmutableConflict, sessionKey, replicaKey, deviceID)
		}
		byReplica[ref] = object.Key
	}
	return refs, nil
}

func collectProjectSessionDescriptorRefs(prefix string, objects []remote.ObjectInfo) (map[string]map[string]string, error) {
	refs := make(map[string]map[string]string)
	for _, object := range objects {
		sessionKey, deviceID, ok := parseProjectSessionDescriptorObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		byDevice := refs[sessionKey]
		if byDevice == nil {
			byDevice = make(map[string]string)
			refs[sessionKey] = byDevice
		}
		if _, exists := byDevice[deviceID]; exists {
			return nil, fmt.Errorf("%w: Session descriptor for Session %q and device %q", ErrReplicaImmutableConflict, sessionKey, deviceID)
		}
		byDevice[deviceID] = object.Key
	}
	return refs, nil
}

func parseProjectSessionDescriptorObjectKey(prefix, key string) (string, string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/sessions/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/sessions/"), "/")
	if len(parts) != 4 || parts[1] != "descriptors" || parts[3] != descriptorMetaName {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil {
		return "", "", false
	}
	expected := prefix + "/sessions/" + parts[0] + "/descriptors/" + parts[2] + "/" + descriptorMetaName
	if checked, err := checkedKey(expected); err != nil || checked != key {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func parseProjectReplicaDescriptorObjectKey(prefix, key string) (string, string, string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/sessions/") {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/sessions/"), "/")
	if len(parts) != 5 || parts[1] != "replicas" || parts[4] != replicaDescriptorName {
		return "", "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[2]) != nil || validateIdentifier(parts[3]) != nil {
		return "", "", "", false
	}
	expected := prefix + "/sessions/" + parts[0] + "/replicas/" + parts[2] + "/" + parts[3] + "/" + replicaDescriptorName
	if checked, err := checkedKey(expected); err != nil || checked != key {
		return "", "", "", false
	}
	return parts[0], parts[2], parts[3], true
}

func collectReplicaDescriptorRefs(prefix string, objects []remote.ObjectInfo) (map[replicaRefKey]string, error) {
	refs := make(map[replicaRefKey]string)
	for _, object := range objects {
		replicaKey, deviceID, ok := parseReplicaDescriptorObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		ref := replicaRefKey{ReplicaKey: replicaKey, DeviceID: deviceID}
		if _, exists := refs[ref]; exists {
			return nil, fmt.Errorf("%w: descriptor for Replica %q and device %q", ErrReplicaImmutableConflict, replicaKey, deviceID)
		}
		refs[ref] = object.Key
	}
	return refs, nil
}

func parseReplicaDescriptorObjectKey(prefix, key string) (string, string, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/replicas/") {
		return "", "", false
	}
	remainder := strings.TrimPrefix(key, prefix+"/replicas/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 3 || parts[2] != replicaDescriptorName {
		return "", "", false
	}
	if validateIdentifier(parts[0]) != nil || validateIdentifier(parts[1]) != nil {
		return "", "", false
	}
	expected := prefix + "/replicas/" + parts[0] + "/" + parts[1] + "/" + replicaDescriptorName
	if checked, err := checkedKey(expected); err != nil || checked != key {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type replicaShardRefs map[uint64]string

func collectReplicaShardRefs(prefix string, objects []remote.ObjectInfo) (replicaShardRefs, error) {
	refs := make(replicaShardRefs)
	for _, object := range objects {
		number, ok := parseReplicaShardObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		if _, exists := refs[number]; exists {
			return nil, fmt.Errorf("%w: sequence %d", ErrDuplicateReplicaShard, number)
		}
		refs[number] = object.Key
	}
	return refs, nil
}

func parseReplicaShardObjectKey(prefix, key string) (uint64, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return 0, false
	}
	name := strings.TrimPrefix(key, prefix+"/")
	number, err := ParseShardNumber(name)
	if err != nil {
		return 0, false
	}
	expected, err := checkedKey(prefix + "/" + fmt.Sprintf("%0*d", replicaShardNameWidth, number))
	if err != nil || expected != key {
		return 0, false
	}
	return number, true
}

func sameShard(left, right Shard) bool {
	if left.Base != right.Base || left.PrefixDigest != right.PrefixDigest || len(left.Records) != len(right.Records) {
		return false
	}
	for index := range left.Records {
		if !bytes.Equal(left.Records[index], right.Records[index]) {
			return false
		}
	}
	return true
}

func digestFromString(value string) [32]byte {
	var digest [32]byte
	parsed, err := parseDigest(strings.TrimPrefix(value, "sha256:"))
	if err == nil {
		return parsed
	}
	return digest
}

func safeReplicaVerifyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, crypto.ErrCorrupt) || errors.Is(err, crypto.ErrUnsupportedVersion) {
		return errors.New("existing object could not be authenticated")
	}
	return errors.New("existing object did not match the requested immutable value")
}
