package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

func TestReplicaLayoutUsesIndependentV2Namespace(t *testing.T) {
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	key, err := layout.ReplicaShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	want := "v2/hubs/hub/projects/project/sessions/session/replicas/replica/device/000001"
	if key != want {
		t.Fatalf("ReplicaShardKey = %q, want %q", key, want)
	}
	if strings.HasPrefix(key, "v1/") {
		t.Fatalf("v2 Replica key used the v1 namespace: %q", key)
	}

	descriptor, err := layout.ReplicaDescriptorKey()
	if err != nil {
		t.Fatal(err)
	}
	if descriptor != "v2/hubs/hub/projects/project/sessions/session/replicas/replica/device/meta" {
		t.Fatalf("ReplicaDescriptorKey = %q", descriptor)
	}
	hubDescriptor, err := layout.HubDescriptorKey()
	if err != nil {
		t.Fatal(err)
	}
	if hubDescriptor != "v2/hubs/hub/descriptors/device/meta" {
		t.Fatalf("HubDescriptorKey = %q", hubDescriptor)
	}
	projectDescriptor, err := layout.ProjectDescriptorKey()
	if err != nil {
		t.Fatal(err)
	}
	if projectDescriptor != "v2/hubs/hub/projects/project/descriptors/device/meta" {
		t.Fatalf("ProjectDescriptorKey = %q", projectDescriptor)
	}
	sessionDescriptor, err := layout.SessionDescriptorKey()
	if err != nil {
		t.Fatal(err)
	}
	if sessionDescriptor != "v2/hubs/hub/projects/project/sessions/session/descriptors/device/meta" {
		t.Fatalf("SessionDescriptorKey = %q", sessionDescriptor)
	}
}

func TestPushAndFetchCompleteReplicaIsEncryptedAndIdempotent(t *testing.T) {
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
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	records := [][]byte{
		[]byte(`{"n":1,"text":"first"}`),
		[]byte(`{"n":2,"text":"second"}`),
		[]byte(`{"n":3,"text":"third"}`),
	}
	now := time.Date(2026, 8, 27, 8, 10, 0, 0, time.UTC)
	options := ReplicaPushOptions{
		Plan:       PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes},
		Identities: []*ecdh.PrivateKey{private},
		Now:        now,
	}
	result, err := PushReplica(context.Background(), store, public, layout, descriptor, NewPushCursor(), records, options)
	if err != nil {
		t.Fatalf("PushReplica: %v", err)
	}
	if result.Cursor.RecordCount != uint64(len(records)) || result.PublishedShards != 3 {
		t.Fatalf("push result = %+v", result)
	}
	if result.Tip.RecordCount != 3 || result.Tip.ShardCount != 3 || result.Tip.LastShard != 3 {
		t.Fatalf("push tip = %+v", result.Tip)
	}

	objects, err := store.List(context.Background(), "v2/hubs/hub/projects/project/sessions/session")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 5 {
		t.Fatalf("v2 Replica object count = %d, want 5", len(objects))
	}
	for _, object := range objects {
		reader, err := store.Get(context.Background(), object.Key)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s: read=%v close=%v", object.Key, readErr, closeErr)
		}
		for _, record := range records {
			if bytes.Contains(body, record) {
				t.Fatalf("remote object %q contains plaintext record", object.Key)
			}
		}
	}

	snapshot, err := FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica: %v", err)
	}
	encodedSnapshotDescriptor, err := snapshot.Descriptor.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	encodedDescriptor, err := descriptor.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedSnapshotDescriptor, encodedDescriptor) || snapshot.Tip != result.Tip || snapshot.HeadDigest != result.Cursor.HeadDigest {
		t.Fatalf("snapshot metadata = %+v, want descriptor=%+v tip=%+v digest=%x", snapshot, descriptor, result.Tip, result.Cursor.HeadDigest)
	}
	if len(snapshot.Records) != len(records) {
		t.Fatalf("snapshot record count = %d, want %d", len(snapshot.Records), len(records))
	}
	for index := range records {
		if !bytes.Equal(snapshot.Records[index], records[index]) {
			t.Fatalf("snapshot record %d = %s, want %s", index, snapshot.Records[index], records[index])
		}
	}

	// A retry with the stale initial cursor verifies all existing immutable
	// objects and does not replace them. The mutable tip may be republished.
	retry, err := PushReplica(context.Background(), store, public, layout, descriptor, NewPushCursor(), records, options)
	if err != nil {
		t.Fatalf("idempotent PushReplica retry: %v", err)
	}
	if retry.Cursor != result.Cursor || retry.Tip != result.Tip {
		t.Fatalf("retry result = %+v, want %+v", retry, result)
	}

	metadata, err := FetchReplicaMetadata(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchReplicaMetadata: %v", err)
	}
	if metadata.Tip == nil || *metadata.Tip != result.Tip {
		t.Fatalf("metadata tip = %+v, want %+v", metadata.Tip, result.Tip)
	}

	sessionLayout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := FetchSessionReplicaMetadata(context.Background(), store, sessionLayout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchSessionReplicaMetadata: %v", err)
	}
	if len(refs) != 1 || refs[0].Descriptor.ReplicaID != "replica" {
		t.Fatalf("Replica metadata refs = %+v", refs)
	}
}

func TestReplicaMetadataListingDoesNotReadShards(t *testing.T) {
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
	base, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &countingReplicaRemote{Remote: base}
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	if err := PutReplicaDescriptor(context.Background(), store, public, layout, descriptor); err != nil {
		t.Fatal(err)
	}
	if err := PutReplicaTip(context.Background(), store, public, layout, sessionhub.ReplicaTip{
		Version:     sessionhub.ModelVersion,
		ReplicaID:   "replica",
		RecordCount: 0,
		HeadDigest:  hexDigest(EmptyDigest()),
		UpdatedAt:   time.Date(2026, 8, 27, 8, 10, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	part, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutReplicaShard(context.Background(), store, public, layout, NewPushCursor(), ShardPart{Number: 1, Shard: part}); err != nil {
		t.Fatal(err)
	}

	sessionLayout, err := NewSessionHubLayout("hub", "project", "session")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := FetchSessionReplicaMetadata(context.Background(), store, sessionLayout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Tip == nil {
		t.Fatalf("metadata refs = %+v", refs)
	}
	if store.shardGets != 0 {
		t.Fatalf("metadata listing read %d shard bodies", store.shardGets)
	}
}

func TestFetchProjectReplicaMetadataGroupsSessionsAndFiltersDevices(t *testing.T) {
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
	base, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &countingReplicaRemote{Remote: base}
	for _, item := range []struct {
		session string
		replica string
		device  string
	}{
		{session: "sessionone", replica: "replicaone", device: "deviceone"},
		{session: "sessiontwo", replica: "replicatwo", device: "devicetwo"},
	} {
		layout, err := NewReplicaLayout("hub", "project", item.session, item.replica, item.device)
		if err != nil {
			t.Fatal(err)
		}
		if err := PutSessionDescriptor(context.Background(), store, public, layout, sessionhub.SessionDescriptor{
			Version:   sessionhub.ModelVersion,
			SessionID: item.session,
			ProjectID: "project",
			Title:     item.session + " title",
			CreatedAt: time.Date(2026, 8, 27, 8, 10, 0, 0, time.UTC),
			CreatedBy: sessionhub.SessionCreator{Agent: "codex", DeviceID: item.device},
			Lifecycle: sessionhub.SessionActive,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := PushReplica(context.Background(), store, public, layout, testReplicaDescriptor(item.replica, item.session, item.device), NewPushCursor(), [][]byte{[]byte(`{"session":"` + item.session + `"}`)}, ReplicaPushOptions{Identities: []*ecdh.PrivateKey{private}}); err != nil {
			t.Fatal(err)
		}
	}

	projectLayout, err := NewProjectHubLayout("hub", "project")
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := FetchProjectReplicaMetadataWithDevices(context.Background(), store, projectLayout, []*ecdh.PrivateKey{private}, map[string]struct{}{"deviceone": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].SessionID != "sessionone" || len(filtered[0].Replicas) != 1 {
		t.Fatalf("filtered project metadata = %+v", filtered)
	}
	if filtered[0].SessionDescriptor == nil || filtered[0].SessionDescriptor.Title != "sessionone title" {
		t.Fatalf("session descriptor = %+v", filtered[0].SessionDescriptor)
	}

	all, err := FetchProjectReplicaMetadata(context.Background(), store, projectLayout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].SessionID != "sessionone" || all[1].SessionID != "sessiontwo" {
		t.Fatalf("all project metadata = %+v", all)
	}
	if store.shardGets != 0 {
		t.Fatalf("project metadata listing read %d shard bodies", store.shardGets)
	}
}

func TestPushReplicaWithCursorStoreAllowsUnverifiedSubsequentPush(t *testing.T) {
	dataKey := newTestDataKey(t)
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	options := ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes}}
	if _, err := PushReplicaWithCursorStore(context.Background(), store, public, layout, descriptor, state, [][]byte{[]byte(`{"n":1}`)}, options); err != nil {
		t.Fatalf("first cursor-backed push: %v", err)
	}
	if _, err := PushReplicaWithCursorStore(context.Background(), store, public, layout, descriptor, state, [][]byte{[]byte(`{"n":1}`)}, options); err != nil {
		t.Fatalf("subsequent cursor-backed push without private identity: %v", err)
	}
}

func TestReplicaImmutableRetryReadsLargeShardBound(t *testing.T) {
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
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	record := []byte(`{"type":"large","payload":"`)
	record = append(record, bytes.Repeat([]byte{'x'}, 2<<20)...)
	record = append(record, []byte(`"}`)...)
	options := ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes}, Identities: []*ecdh.PrivateKey{private}}
	if _, err := PushReplica(context.Background(), store, public, layout, testReplicaDescriptor("replica", "session", "device"), NewPushCursor(), [][]byte{record}, options); err != nil {
		t.Fatal(err)
	}
	if _, err := PushReplica(context.Background(), store, public, layout, testReplicaDescriptor("replica", "session", "device"), NewPushCursor(), [][]byte{record}, options); err != nil {
		t.Fatalf("large immutable retry: %v", err)
	}
}

func TestFetchCompleteReplicaRejectsMissingShardAndImmutableConflict(t *testing.T) {
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
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	options := ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes}, Identities: []*ecdh.PrivateKey{private}}
	if _, err := PushReplica(context.Background(), store, public, layout, descriptor, NewPushCursor(), records, options); err != nil {
		t.Fatal(err)
	}
	secondKey, err := layout.ReplicaShardKey(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), secondKey); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private}); !errors.Is(err, ErrReplicaIncomplete) {
		t.Fatalf("missing shard error = %v, want ErrReplicaIncomplete", err)
	}
	if err := store.Put(context.Background(), secondKey, bytes.NewReader([]byte("tampered")), int64(len("tampered"))); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private}); !errors.Is(err, ErrReplicaIncomplete) {
		t.Fatalf("tampered shard error = %v, want ErrReplicaIncomplete", err)
	}

	// The same immutable key cannot be reused with a different shard, even
	// though the generic Remote interface itself permits replacement.
	first, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"different":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutReplicaShardWithIdentities(context.Background(), store, public, layout, NewPushCursor(), ShardPart{Number: 1, Shard: first}, []*ecdh.PrivateKey{private}); !errors.Is(err, ErrReplicaImmutableConflict) {
		t.Fatalf("immutable conflict error = %v, want ErrReplicaImmutableConflict", err)
	}
}

func TestConcurrentReplicaWritersUseIndependentDeviceNamespaces(t *testing.T) {
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
	store := newConcurrentReplicaRemote()

	type writeResult struct {
		layout ReplicaLayout
		err    error
	}
	results := make(chan writeResult, 2)
	var writers sync.WaitGroup
	for _, item := range []struct {
		replica string
		device  string
		record  string
	}{
		{replica: "replicaa", device: "devicea", record: `{"source":"a"}`},
		{replica: "replicab", device: "deviceb", record: `{"source":"b"}`},
	} {
		item := item
		writers.Add(1)
		go func() {
			defer writers.Done()
			layout, layoutErr := NewReplicaLayout("hub", "project", "session", item.replica, item.device)
			if layoutErr != nil {
				results <- writeResult{err: layoutErr}
				return
			}
			_, pushErr := PushReplica(context.Background(), store, public, layout, testReplicaDescriptor(item.replica, "session", item.device), NewPushCursor(), [][]byte{[]byte(item.record)}, ReplicaPushOptions{Identities: []*ecdh.PrivateKey{private}})
			results <- writeResult{layout: layout, err: pushErr}
		}()
	}
	writers.Wait()
	close(results)

	layouts := make([]ReplicaLayout, 0, 2)
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent Replica push: %v", result.err)
		}
		layouts = append(layouts, result.layout)
	}
	if len(layouts) != 2 {
		t.Fatalf("concurrent layouts = %d, want 2", len(layouts))
	}
	objects, err := store.List(context.Background(), "v2/hubs/hub/projects/project/sessions/session")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 6 {
		t.Fatalf("concurrent Replica object count = %d, want 6", len(objects))
	}
	for _, layout := range layouts {
		if _, err := FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private}); err != nil {
			t.Fatalf("FetchCompleteReplica for %s/%s: %v", layout.ReplicaKey(), layout.DeviceID(), err)
		}
	}
}

func TestReplicaPushWithoutIdentityRefusesExistingImmutableObjects(t *testing.T) {
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
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	if _, err := PushReplica(context.Background(), store, public, layout, descriptor, NewPushCursor(), [][]byte{[]byte(`{"n":1}`)}, ReplicaPushOptions{Identities: []*ecdh.PrivateKey{private}}); err != nil {
		t.Fatal(err)
	}
	_, err = PushReplica(context.Background(), store, public, layout, descriptor, NewPushCursor(), [][]byte{[]byte(`{"n":1}`)}, ReplicaPushOptions{})
	if !errors.Is(err, ErrReplicaImmutableConflict) {
		t.Fatalf("unverified retry error = %v, want ErrReplicaImmutableConflict", err)
	}
}

func TestReplicaCursorStoreIsolatedAndAtomic(t *testing.T) {
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Load(context.Background()); !errors.Is(err, ErrNoReplicaCursor) {
		t.Fatalf("missing cursor error = %v, want ErrNoReplicaCursor", err)
	}
	want := PushCursor{NextShard: 3, RecordCount: 2, HeadDigest: [32]byte{2}}
	if err := state.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := state.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("loaded cursor = %+v, want %+v", got, want)
	}

	path, err := state.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"replicaId":"other","nextShard":3,"recordCount":2,"headDigest":"`+hexDigest(want.HeadDigest)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Load(context.Background()); !errors.Is(err, ErrInvalidReplicaCursorState) {
		t.Fatalf("foreign cursor error = %v, want ErrInvalidReplicaCursorState", err)
	}
}

func testReplicaDescriptor(replicaID, sessionID, deviceID string) sessionhub.NativeReplicaDescriptor {
	return sessionhub.NativeReplicaDescriptor{
		Version:   sessionhub.ModelVersion,
		ReplicaID: replicaID,
		SessionID: sessionID,
		Source: sessionhub.NativeSource{
			Agent:            "codex",
			NativeSessionKey: "nativekey",
			DeviceID:         deviceID,
			Generation:       1,
			NativeFormat:     "codex-jsonl",
			AgentVersion:     "0.42.0",
		},
		Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative, BaseHeads: []string{}},
		CreatedAt: time.Date(2026, 8, 27, 8, 10, 0, 0, time.UTC),
	}
}

type countingReplicaRemote struct {
	remote.Remote
	shardGets int
}

func (r *countingReplicaRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if isReplicaShardKey(key) {
		r.shardGets++
	}
	return r.Remote.Get(ctx, key)
}

func isReplicaShardKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) == 0 {
		return false
	}
	_, err := ParseShardNumber(parts[len(parts)-1])
	return err == nil
}

var _ remote.Remote = (*countingReplicaRemote)(nil)

type concurrentReplicaRemote struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func newConcurrentReplicaRemote() *concurrentReplicaRemote {
	return &concurrentReplicaRemote{objects: make(map[string][]byte)}
}

func (r *concurrentReplicaRemote) Name() string { return "concurrent-replica-fake" }

func (r *concurrentReplicaRemote) List(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	objects := make([]remote.ObjectInfo, 0)
	for key, body := range r.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, remote.ObjectInfo{Key: key, Size: int64(len(body))})
		}
	}
	return objects, nil
}

func (r *concurrentReplicaRemote) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	body, ok := r.objects[key]
	body = append([]byte(nil), body...)
	r.mu.RUnlock()
	if !ok {
		return nil, fmtRemoteNotFound(key)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (r *concurrentReplicaRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(data)) != size {
		return errors.New("concurrent fake received an unexpected object size")
	}
	r.mu.Lock()
	r.objects[key] = append([]byte(nil), data...)
	r.mu.Unlock()
	return nil
}

func (r *concurrentReplicaRemote) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.objects, key)
	r.mu.Unlock()
	return nil
}

func (r *concurrentReplicaRemote) Stat(ctx context.Context, key string) (remote.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return remote.ObjectInfo{}, err
	}
	r.mu.RLock()
	body, ok := r.objects[key]
	r.mu.RUnlock()
	if !ok {
		return remote.ObjectInfo{}, fmtRemoteNotFound(key)
	}
	return remote.ObjectInfo{Key: key, Size: int64(len(body))}, nil
}

var _ remote.Remote = (*concurrentReplicaRemote)(nil)
