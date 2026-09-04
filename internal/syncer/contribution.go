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

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

var (
	// ErrNoContributions reports a logical Session without any visible
	// immutable Contribution objects.
	ErrNoContributions = errors.New("syncer: v2 session has no contributions")

	// ErrContributionMissing reports a direct read of a Contribution object
	// that is not present. A list followed by this error is classified as an
	// incomplete snapshot by FetchSessionContributions.
	ErrContributionMissing = errors.New("syncer: v2 contribution is missing")

	// ErrContributionImmutableConflict prevents an existing Contribution from
	// being replaced. An identical retry is accepted only when a retained
	// private content identity can authenticate the existing object.
	ErrContributionImmutableConflict = errors.New("syncer: immutable contribution conflicts")

	// ErrContributionIdentityMismatch reports a valid envelope placed under a
	// different Session or Contribution object key.
	ErrContributionIdentityMismatch = errors.New("syncer: v2 contribution identity mismatch")

	// ErrContributionObjectTooLarge reports an encrypted Contribution object
	// that exceeds the bounded envelope accepted by this layer.
	ErrContributionObjectTooLarge = errors.New("syncer: v2 contribution object is too large")

	// ErrContributionSnapshotIncomplete reports a list/object/graph snapshot
	// that cannot be proven complete. Callers must not materialize it.
	ErrContributionSnapshotIncomplete = errors.New("syncer: v2 contribution snapshot is incomplete")

	// ErrDuplicateContributionObject reports duplicate list entries for one
	// immutable Contribution identity.
	ErrDuplicateContributionObject = errors.New("syncer: duplicate v2 contribution object")
)

// PutContribution publishes one immutable Contribution envelope. The object
// key is derived only from the Contribution ID, while the encrypted envelope
// authenticates the full source/parent/range identity. An existing object is
// never overwritten.
func PutContribution(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, contribution sessionhub.Contribution) error {
	return putContribution(ctx, store, recipient, layout, contribution, nil)
}

// PutContributionWithIdentities publishes or verifies one immutable
// Contribution using one of the retained content-key generations. This makes
// retries idempotent without allowing a conflicting envelope to overwrite the
// original object.
func PutContributionWithIdentities(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, contribution sessionhub.Contribution, identities []*ecdh.PrivateKey) error {
	if err := validateIdentities(identities); err != nil {
		return err
	}
	return putContribution(ctx, store, recipient, layout, contribution, identities)
}

func putContribution(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout SessionHubLayout, contribution sessionhub.Contribution, identities []*ecdh.PrivateKey) error {
	if err := validateReplicaWriteArgs(ctx, store, recipient); err != nil {
		return err
	}
	if err := layout.validate(); err != nil {
		return err
	}
	if contribution.SessionID != layout.sessionKey {
		return fmt.Errorf("%w: contribution belongs to Session %q, layout is %q", ErrContributionIdentityMismatch, contribution.SessionID, layout.sessionKey)
	}
	key, err := layout.ContributionKey(contribution.ContributionID, contribution.Source.DeviceID)
	if err != nil {
		return err
	}
	payload, err := contribution.MarshalBinary()
	if err != nil {
		return fmt.Errorf("syncer: encode v2 Contribution: %w", err)
	}
	sealed, err := sealContributionPayload(recipient, key, payload)
	if err != nil {
		return fmt.Errorf("syncer: encrypt v2 Contribution: %w", err)
	}
	verify := func(existing []byte) error {
		opened, err := openContributionPayloadWithIdentities(identities, key, existing)
		if err != nil {
			return err
		}
		parsed, err := sessionhub.ParseContribution(opened)
		if err != nil {
			return err
		}
		if parsed.SessionID != layout.sessionKey || parsed.ContributionID != contribution.ContributionID {
			return ErrContributionIdentityMismatch
		}
		canonical, err := parsed.MarshalBinary()
		if err != nil {
			return err
		}
		if !bytes.Equal(canonical, payload) {
			return ErrContributionImmutableConflict
		}
		return nil
	}
	return putContributionImmutable(ctx, store, key, sealed, identities, verify)
}

// FetchContribution reads and authenticates one immutable Contribution from a
// logical Session. It never reads a Replica body.
func FetchContribution(ctx context.Context, store remote.Remote, layout SessionHubLayout, contributionID, deviceID string, identities []*ecdh.PrivateKey) (sessionhub.Contribution, error) {
	if ctx == nil {
		return sessionhub.Contribution{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return sessionhub.Contribution{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return sessionhub.Contribution{}, err
	}
	key, err := layout.ContributionKey(contributionID, deviceID)
	if err != nil {
		return sessionhub.Contribution{}, err
	}
	sealed, err := readContributionObject(ctx, store, key)
	if errors.Is(err, remote.ErrNotFound) {
		return sessionhub.Contribution{}, fmt.Errorf("%w: %w", ErrContributionMissing, err)
	}
	if err != nil {
		return sessionhub.Contribution{}, fmt.Errorf("syncer: read v2 Contribution: %w", err)
	}
	payload, err := openContributionPayloadWithIdentities(identities, key, sealed)
	if err != nil {
		return sessionhub.Contribution{}, fmt.Errorf("syncer: decrypt v2 Contribution: %w", err)
	}
	contribution, err := sessionhub.ParseContribution(payload)
	if err != nil {
		return sessionhub.Contribution{}, fmt.Errorf("syncer: parse v2 Contribution: %w", err)
	}
	if contribution.SessionID != layout.sessionKey || contribution.ContributionID != contributionID || contribution.Source.DeviceID != deviceID {
		return sessionhub.Contribution{}, fmt.Errorf("%w: envelope does not match Session or object key", ErrContributionIdentityMismatch)
	}
	return contribution, nil
}

// FetchSessionContributions lists and authenticates every visible immutable
// Contribution under one logical Session. A missing object after listing, a
// malformed graph member, or a missing parent is returned as an incomplete
// snapshot rather than silently omitted.
func FetchSessionContributions(ctx context.Context, store remote.Remote, layout SessionHubLayout, identities []*ecdh.PrivateKey) ([]sessionhub.Contribution, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return nil, err
	}
	prefix, err := layout.ContributionPrefix()
	if err != nil {
		return nil, err
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("syncer: list v2 Contributions: %w", err)
	}
	refs, err := collectContributionRefs(prefix, objects)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, ErrNoContributions
	}
	contributions := make([]sessionhub.Contribution, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrContributionSnapshotIncomplete, err)
		}
		contribution, err := FetchContribution(ctx, store, layout, ref.ContributionID, ref.DeviceID, identities)
		if err != nil {
			return nil, fmt.Errorf("%w: Contribution %q from device %q: %w", ErrContributionSnapshotIncomplete, ref.ContributionID, ref.DeviceID, err)
		}
		contributions = append(contributions, contribution)
	}
	return contributions, nil
}

// FetchContributionGraph reads one authenticated Contribution snapshot and
// builds the immutable causal DAG. Graph construction is intentionally kept in
// sessionhub; this function only bridges encrypted object storage to it.
func FetchContributionGraph(ctx context.Context, store remote.Remote, layout SessionHubLayout, identities []*ecdh.PrivateKey) (*sessionhub.Graph, error) {
	contributions, err := FetchSessionContributions(ctx, store, layout, identities)
	if err != nil {
		return nil, err
	}
	graph, err := sessionhub.NewContributionGraph(layout.sessionKey, contributions)
	if err != nil {
		return nil, fmt.Errorf("%w: build contribution graph: %w", ErrContributionSnapshotIncomplete, err)
	}
	return graph, nil
}

func sealContributionPayload(recipient *ecdh.PublicKey, key string, payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > maxEncryptedReplicaDescriptorBytes {
		return nil, fmt.Errorf("%w: plaintext envelope is too large", ErrContributionObjectTooLarge)
	}
	sealed, err := crypto.Encrypt(recipient, key, payload)
	if err != nil {
		return nil, err
	}
	if len(sealed) > maxEncryptedReplicaDescriptorBytes {
		return nil, fmt.Errorf("%w: encrypted envelope exceeds %d bytes", ErrContributionObjectTooLarge, maxEncryptedReplicaDescriptorBytes)
	}
	return sealed, nil
}

func openContributionPayloadWithIdentities(identities []*ecdh.PrivateKey, key string, sealed []byte) ([]byte, error) {
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

func putContributionImmutable(ctx context.Context, store remote.Remote, key string, sealed []byte, identities []*ecdh.PrivateKey, verify func([]byte) error) error {
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
		return fmt.Errorf("check immutable v2 Contribution: %w", err)
	case verify == nil || len(identities) == 0:
		return fmt.Errorf("%w: %s already exists and cannot be verified without a private identity", ErrContributionImmutableConflict, key)
	default:
		existing, err := readContributionObject(ctx, store, key)
		if err != nil {
			return fmt.Errorf("read immutable v2 Contribution for retry: %w", err)
		}
		if err := verify(existing); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrContributionImmutableConflict, key, err)
		}
		return nil
	}
}

func readContributionObject(ctx context.Context, store remote.Remote, key string) ([]byte, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedReplicaDescriptorBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("read v2 Contribution: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("read v2 Contribution: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close v2 Contribution: %w", closeErr)
	}
	if len(data) > maxEncryptedReplicaDescriptorBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrContributionObjectTooLarge, maxEncryptedReplicaDescriptorBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type contributionRef struct {
	ContributionID string
	DeviceID       string
}

func collectContributionRefs(prefix string, objects []remote.ObjectInfo) ([]contributionRef, error) {
	refs := make(map[contributionRef]struct{})
	for _, object := range objects {
		ref, ok := parseContributionObjectKey(prefix, object.Key)
		if !ok {
			continue
		}
		if _, exists := refs[ref]; exists {
			return nil, fmt.Errorf("%w: Contribution %q from device %q", ErrDuplicateContributionObject, ref.ContributionID, ref.DeviceID)
		}
		refs[ref] = struct{}{}
	}
	result := make([]contributionRef, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ContributionID != result[j].ContributionID {
			return result[i].ContributionID < result[j].ContributionID
		}
		return result[i].DeviceID < result[j].DeviceID
	})
	return result, nil
}

func parseContributionObjectKey(prefix, key string) (contributionRef, bool) {
	if key == "" || !strings.HasPrefix(key, prefix+"/") {
		return contributionRef{}, false
	}
	parts := strings.Split(strings.TrimPrefix(key, prefix+"/"), "/")
	if len(parts) != 2 || validateIdentifier(parts[0]) != nil || validateIdentifier(parts[1]) != nil {
		return contributionRef{}, false
	}
	expected, err := checkedKey(prefix + "/" + parts[0] + "/" + parts[1])
	if err != nil || expected != key {
		return contributionRef{}, false
	}
	return contributionRef{DeviceID: parts[0], ContributionID: parts[1]}, true
}
