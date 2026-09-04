package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

type remoteReadFake struct {
	objects map[string][]byte
	list    []remote.ObjectInfo
	listErr error
	getErr  error
}

func (f *remoteReadFake) Name() string { return "fake" }

func (f *remoteReadFake) List(ctx context.Context, _ string) ([]remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]remote.ObjectInfo(nil), f.list...), nil
}

func (f *remoteReadFake) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, fmtRemoteNotFound(key)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *remoteReadFake) Put(context.Context, string, io.Reader, int64) error { return nil }
func (f *remoteReadFake) Delete(context.Context, string) error                { return nil }
func (f *remoteReadFake) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, remote.ErrNotFound
}

func fmtRemoteNotFound(key string) error {
	return errors.Join(remote.ErrNotFound, errors.New("missing "+key))
}

func TestFetchBranchesAssemblesAllVisibleDevices(t *testing.T) {
	store := &remoteReadFake{objects: map[string][]byte{}}
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	projectID, sessionID := "project", "session"
	deviceA, err := NewObjectLayout(projectID, sessionID, "devicea")
	if err != nil {
		t.Fatal(err)
	}
	deviceB, err := NewObjectLayout(projectID, sessionID, "deviceb")
	if err != nil {
		t.Fatal(err)
	}
	addRemoteShard(t, store, deviceA, public, 1, EmptyDigest(), [][]byte{[]byte(`{"n":1}`)})
	prefixA, err := DigestRecords([][]byte{[]byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	addRemoteShard(t, store, deviceA, public, 2, prefixA, [][]byte{[]byte(`{"n":2}`)})
	addRemoteShard(t, store, deviceB, public, 1, EmptyDigest(), [][]byte{[]byte(`{"n":1}`), []byte(`{"n":3}`)})

	sessionPrefix, err := NewSessionLayout(projectID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := sessionPrefix.Prefix()
	if err != nil {
		t.Fatal(err)
	}
	store.list = append(store.list,
		remote.ObjectInfo{Key: prefix + "/devicea/meta"},
		remote.ObjectInfo{Key: prefix + "/Foreign/000001"},
	)
	sort.Slice(store.list, func(i, j int) bool { return store.list[i].Key > store.list[j].Key })

	branches, err := FetchBranches(context.Background(), store, projectID, sessionID, private)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 || branches[0].DeviceID != "devicea" || branches[1].DeviceID != "deviceb" {
		t.Fatalf("branches = %+v", branches)
	}
	if len(branches[0].Records) != 2 || len(branches[1].Records) != 2 {
		t.Fatalf("branch records = %+v", branches)
	}
	if !bytes.Equal(branches[0].Records[0], []byte(`{"n":1}`)) || !bytes.Equal(branches[0].Records[1], []byte(`{"n":2}`)) {
		t.Fatalf("device A records = %q", branches[0].Records)
	}
}

func TestFetchBranchesRefusesIncompleteOrDuplicatedStreams(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	baseRecord := []byte(`{"n":1}`)
	prefix, err := DigestRecords([][]byte{baseRecord})
	if err != nil {
		t.Fatal(err)
	}

	for name, setup := range map[string]func(*testing.T, *remoteReadFake, ObjectLayout){
		"missing first": func(t *testing.T, store *remoteReadFake, layout ObjectLayout) {
			addRemoteShard(t, store, layout, public, 2, prefix, [][]byte{[]byte(`{"n":2}`)})
		},
		"missing middle": func(t *testing.T, store *remoteReadFake, layout ObjectLayout) {
			addRemoteShard(t, store, layout, public, 1, EmptyDigest(), [][]byte{baseRecord})
			addRemoteShard(t, store, layout, public, 3, prefix, [][]byte{[]byte(`{"n":3}`)})
		},
		"duplicate": func(t *testing.T, store *remoteReadFake, layout ObjectLayout) {
			addRemoteShard(t, store, layout, public, 1, EmptyDigest(), [][]byte{baseRecord})
			key, err := layout.ShardKey(1)
			if err != nil {
				t.Fatal(err)
			}
			store.list = append(store.list, remote.ObjectInfo{Key: key})
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &remoteReadFake{objects: map[string][]byte{}}
			layout, err := NewObjectLayout("project", "session", "device")
			if err != nil {
				t.Fatal(err)
			}
			setup(t, store, layout)
			_, err = FetchBranches(context.Background(), store, "project", "session", private)
			if err == nil {
				t.Fatal("FetchBranches unexpectedly succeeded")
			}
			if name == "duplicate" {
				if !errors.Is(err, ErrDuplicateShard) {
					t.Fatalf("error = %v, want ErrDuplicateShard", err)
				}
			} else if !errors.Is(err, ErrIncompleteBranch) {
				t.Fatalf("error = %v, want ErrIncompleteBranch", err)
			}
		})
	}
}

func TestFetchBranchesStopsOnRemoteDisagreementDamageAndSize(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		present bool
		body    []byte
		want    error
	}{
		"listed then missing": {want: remote.ErrNotFound},
		"corrupt":             {present: true, body: []byte("not ciphertext")},
		"too large":           {present: true, body: bytes.Repeat([]byte{'x'}, maxEncryptedShardBytes+1), want: ErrRemoteObjectTooLarge},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := &remoteReadFake{objects: map[string][]byte{}, list: []remote.ObjectInfo{{Key: key}}}
			if test.present {
				store.objects[key] = test.body
			}
			_, err := FetchBranches(context.Background(), store, "project", "session", private)
			if err == nil {
				t.Fatal("FetchBranches unexpectedly succeeded")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestFetchBranchesHandlesEmptyInputCancellationAndArguments(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	store := &remoteReadFake{objects: map[string][]byte{}}
	if _, err := FetchBranches(context.Background(), store, "project", "session", private); !errors.Is(err, ErrNoRemoteBranches) {
		t.Fatalf("empty result error = %v, want ErrNoRemoteBranches", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchBranches(cancelled, store, "project", "session", private); err == nil {
		t.Fatal("cancelled fetch unexpectedly succeeded")
	}
	if _, err := FetchBranches(context.Background(), nil, "project", "session", private); err == nil {
		t.Fatal("nil remote unexpectedly succeeded")
	}
	if _, err := FetchBranches(context.Background(), store, "project", "session", nil); err == nil {
		t.Fatal("nil identity unexpectedly succeeded")
	}
	if _, err := FetchBranches(nil, store, "project", "session", private); err == nil {
		t.Fatal("nil context unexpectedly succeeded")
	}
}

func addRemoteShard(t *testing.T, store *remoteReadFake, layout ObjectLayout, public *ecdh.PublicKey, number uint64, prefix [32]byte, records [][]byte) {
	t.Helper()
	key, err := layout.ShardKey(number)
	if err != nil {
		t.Fatal(err)
	}
	shard, err := NewShard(number-1, prefix, records)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealShard(public, key, shard)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[key] = sealed
	store.list = append(store.list, remote.ObjectInfo{Key: key, Size: int64(len(sealed))})
}

func TestFetchCompleteBranchesRejectsAStaleShardListing(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	store := &remoteReadFake{objects: map[string][]byte{}}
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	addRemoteShard(t, store, layout, public, 1, EmptyDigest(), records[:1])
	firstDigest, err := DigestRecords(records[:1])
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := layout.ShardKey(2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewShard(1, firstDigest, records[1:])
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealShard(public, secondKey, second)
	if err != nil {
		t.Fatal(err)
	}
	// The object exists and can be read, but the eventual-consistency snapshot
	// returned by List does not expose it yet.
	store.objects[secondKey] = sealed
	digest, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(uint64(len(records)), digest, []byte(`{"test":true}`))
	if err != nil {
		t.Fatal(err)
	}
	addRemoteMetadata(t, store, layout, public, metadata)

	_, err = FetchCompleteBranches(context.Background(), store, "project", "session", private)
	if err == nil || !errors.Is(err, ErrIncompleteRemoteSession) {
		t.Fatalf("FetchCompleteBranches error = %v, want ErrIncompleteRemoteSession", err)
	}
}

func TestFetchCompleteBranchesAcceptsMatchingMetadataAndShards(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	store := &remoteReadFake{objects: map[string][]byte{}}
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	addRemoteShard(t, store, layout, public, 1, EmptyDigest(), records[:1])
	firstDigest, err := DigestRecords(records[:1])
	if err != nil {
		t.Fatal(err)
	}
	addRemoteShard(t, store, layout, public, 2, firstDigest, records[1:])
	digest, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(uint64(len(records)), digest, []byte(`{"test":true}`))
	if err != nil {
		t.Fatal(err)
	}
	addRemoteMetadata(t, store, layout, public, metadata)

	branches, err := FetchCompleteBranches(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatalf("FetchCompleteBranches: %v", err)
	}
	if len(branches) != 1 || len(branches[0].Records) != len(records) {
		t.Fatalf("branches = %+v", branches)
	}

	replicas, err := FetchCompleteLegacyReplicas(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatalf("FetchCompleteLegacyReplicas: %v", err)
	}
	if len(replicas) != 1 || replicas[0].LegacySessionID != "session" || replicas[0].DeviceID != "device" {
		t.Fatalf("legacy replicas = %+v", replicas)
	}
	if replicas[0].Metadata.RecordCount != metadata.RecordCount || replicas[0].Metadata.HeadDigest != metadata.HeadDigest || !bytes.Equal(replicas[0].Metadata.Payload, metadata.Payload) {
		t.Fatalf("legacy metadata = %+v, want %+v", replicas[0].Metadata, metadata)
	}
	if len(replicas[0].Branch.Records) != len(records) || replicas[0].Branch.HeadDigest != digest {
		t.Fatalf("legacy branch = %+v", replicas[0].Branch)
	}
}
