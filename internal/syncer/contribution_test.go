package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

func TestContributionLayoutUsesSharedSessionNamespace(t *testing.T) {
	layout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := layout.ContributionPrefix()
	if err != nil {
		t.Fatal(err)
	}
	if want := "v2/hubs/hub/projects/project/sessions/session/contributions"; prefix != want {
		t.Fatalf("ContributionPrefix() = %q, want %q", prefix, want)
	}
	key, err := layout.ContributionKey("contribution", "device")
	if err != nil {
		t.Fatal(err)
	}
	if want := prefix + "/device/contribution"; key != want {
		t.Fatalf("ContributionKey() = %q, want %q", key, want)
	}
	if _, err := layout.ContributionKey("unsafe-id", "device"); err == nil {
		t.Fatal("ContributionKey accepted an unsafe object name")
	}
}

func TestPutAndFetchContributionIsEncryptedIdempotentAndImmutable(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	contribution := testRemoteContribution("contribution", "session", "replica", "device")
	if err := PutContribution(context.Background(), store, public, layout, contribution); err != nil {
		t.Fatalf("PutContribution: %v", err)
	}
	prefix, err := layout.ContributionPrefix()
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 {
		t.Fatalf("Contribution object count = %d, want 1", len(objects))
	}
	reader, err := store.Get(context.Background(), objects[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read contribution object: read=%v close=%v", readErr, closeErr)
	}
	if bytes.Contains(ciphertext, []byte(`"contributionId"`)) || bytes.Contains(ciphertext, []byte(`"replicaId"`)) {
		t.Fatal("Contribution object contains plaintext envelope fields")
	}

	fetched, err := FetchContribution(context.Background(), store, layout, contribution.ContributionID, contribution.Source.DeviceID, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchContribution: %v", err)
	}
	encodedFetched, err := fetched.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	encodedOriginal, err := contribution.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedFetched, encodedOriginal) {
		t.Fatalf("fetched Contribution differs from original: %s != %s", encodedFetched, encodedOriginal)
	}

	if err := PutContributionWithIdentities(context.Background(), store, public, layout, contribution, []*ecdh.PrivateKey{private}); err != nil {
		t.Fatalf("idempotent Contribution retry: %v", err)
	}
	if err := PutContribution(context.Background(), store, public, layout, contribution); !errors.Is(err, ErrContributionImmutableConflict) {
		t.Fatalf("unverifiable retry error = %v, want ErrContributionImmutableConflict", err)
	}
	conflicting := contribution
	conflicting.Ranges = append([]sessionhub.RangeRef(nil), contribution.Ranges...)
	conflicting.Ranges[0].RangeDigest = strings.Repeat("2", 64)
	if err := PutContributionWithIdentities(context.Background(), store, public, layout, conflicting, []*ecdh.PrivateKey{private}); !errors.Is(err, ErrContributionImmutableConflict) {
		t.Fatalf("conflicting retry error = %v, want ErrContributionImmutableConflict", err)
	}
}

func TestFetchContributionGraphRejectsIncompleteSnapshot(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	child := testRemoteContribution("child", "session", "replica", "device", "missing")
	if err := PutContribution(context.Background(), store, public, layout, child); err != nil {
		t.Fatalf("PutContribution(child): %v", err)
	}
	if _, err := FetchSessionContributions(context.Background(), store, layout, []*ecdh.PrivateKey{private}); err != nil {
		t.Fatalf("FetchSessionContributions: %v", err)
	}
	_, err = FetchContributionGraph(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if !errors.Is(err, ErrContributionSnapshotIncomplete) || !errors.Is(err, sessionhub.ErrUnknownParent) {
		t.Fatalf("incomplete graph error = %v, want snapshot incomplete and unknown parent", err)
	}
}

func TestFetchContributionGraphReturnsDeterministicHeads(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	parent := testRemoteContribution("parent", "session", "replica", "device")
	child := testRemoteContribution("child", "session", "replica", "device", parent.ContributionID)
	for _, contribution := range []sessionhub.Contribution{child, parent} {
		if err := PutContribution(context.Background(), store, public, layout, contribution); err != nil {
			t.Fatalf("PutContribution(%s): %v", contribution.ContributionID, err)
		}
	}
	graph, err := FetchContributionGraph(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchContributionGraph: %v", err)
	}
	if got := graph.Heads(); len(got) != 1 || got[0] != child.ContributionID {
		t.Fatalf("graph heads = %v, want [%s]", got, child.ContributionID)
	}
	if got := graph.ContributionIDs(); len(got) != 2 || got[0] != child.ContributionID || got[1] != parent.ContributionID {
		t.Fatalf("graph IDs = %v, want lexical order [%s %s]", got, child.ContributionID, parent.ContributionID)
	}
}

func testRemoteContribution(id, sessionID, replicaID, deviceID string, parents ...string) sessionhub.Contribution {
	return sessionhub.Contribution{
		Version:        sessionhub.ModelVersion,
		ContributionID: id,
		SessionID:      sessionID,
		Source: sessionhub.ContributionSource{
			Agent:      "claude-code",
			ReplicaID:  replicaID,
			DeviceID:   deviceID,
			Generation: 1,
		},
		Parents: parents,
		Ranges: []sessionhub.RangeRef{{
			ReplicaID:    replicaID,
			StartRecord:  0,
			EndRecord:    1,
			PrefixDigest: strings.Repeat("0", 64),
			RangeDigest:  strings.Repeat("1", 64),
		}},
		CreatedAt: time.Date(2026, 8, 27, 8, 10, 0, 0, time.UTC),
	}
}
