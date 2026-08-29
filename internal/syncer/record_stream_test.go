package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestLegacyReplicaReaderStreamsOneShardAtATime(t *testing.T) {
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
	store := &remoteReadFake{objects: map[string][]byte{}}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`), []byte(`{"n":3}`)}
	firstDigest, err := DigestRecords(records[:2])
	if err != nil {
		t.Fatal(err)
	}
	addRemoteShard(t, store, layout, public, 1, EmptyDigest(), records[:2])
	secondKey, err := layout.ShardKey(2)
	if err != nil {
		t.Fatal(err)
	}
	secondShard, err := NewShard(2, firstDigest, records[2:])
	if err != nil {
		t.Fatal(err)
	}
	secondSealed, err := SealShard(public, secondKey, secondShard)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[secondKey] = secondSealed
	store.list = append(store.list, remote.ObjectInfo{Key: secondKey, Size: int64(len(secondSealed))})
	digest, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := NewMetadata(uint64(len(records)), digest, []byte(`{"agent":"codex","nativeId":"native-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	addRemoteMetadata(t, store, layout, public, metadata)

	reader, err := OpenLegacyReplicaReader(context.Background(), store, "project", "session", "device", []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("OpenLegacyReplicaReader: %v", err)
	}
	if got := reader.Metadata(); got.RecordCount != metadata.RecordCount || got.HeadDigest != metadata.HeadDigest || !bytes.Equal(got.Payload, metadata.Payload) {
		t.Fatalf("reader metadata = %+v, want %+v", got, metadata)
	}
	first, err := reader.Next(context.Background())
	if err != nil {
		t.Fatalf("first reader.Next: %v", err)
	}
	if !bytes.Equal(first, records[0]) || reader.shardIndex != 1 || len(reader.current.Records) != 2 {
		t.Fatalf("reader loaded more or less than one shard: first=%q shardIndex=%d currentRecords=%d", first, reader.shardIndex, len(reader.current.Records))
	}

	var got [][]byte
	got = append(got, first)
	for {
		record, nextErr := reader.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("reader.Next: %v", nextErr)
		}
		got = append(got, record)
	}
	if len(got) != len(records) {
		t.Fatalf("streamed record count = %d, want %d", len(got), len(records))
	}
	for i := range records {
		if !bytes.Equal(got[i], records[i]) {
			t.Fatalf("record %d = %q, want %q", i, got[i], records[i])
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(reader.current.Records) != 0 || reader.shards != nil || !reader.closed {
		t.Fatalf("reader retained state after Close: %+v", reader)
	}
}

func TestLegacyReplicaReaderRefusesGapsAndMetadataDisagreement(t *testing.T) {
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

	t.Run("gap", func(t *testing.T) {
		store := &remoteReadFake{objects: map[string][]byte{}}
		layout, err := NewObjectLayout("project", "gap", "device")
		if err != nil {
			t.Fatal(err)
		}
		record := []byte(`{"n":1}`)
		digest, err := DigestRecords([][]byte{record})
		if err != nil {
			t.Fatal(err)
		}
		addRemoteShard(t, store, layout, public, 2, digest, [][]byte{record})
		metadata, err := NewMetadata(1, digest, []byte(`{"agent":"codex","nativeId":"native-gap"}`))
		if err != nil {
			t.Fatal(err)
		}
		addRemoteMetadata(t, store, layout, public, metadata)
		if _, err := OpenLegacyReplicaReader(context.Background(), store, "project", "gap", "device", []*ecdh.PrivateKey{private}); err == nil || !errors.Is(err, ErrIncompleteRemoteSession) {
			t.Fatalf("gap reader error = %v, want ErrIncompleteRemoteSession", err)
		}
	})

	t.Run("metadata disagreement", func(t *testing.T) {
		store := &remoteReadFake{objects: map[string][]byte{}}
		layout, err := NewObjectLayout("project", "short", "device")
		if err != nil {
			t.Fatal(err)
		}
		record := []byte(`{"n":1}`)
		addRemoteShard(t, store, layout, public, 1, EmptyDigest(), [][]byte{record})
		metadata, err := NewMetadata(2, [32]byte{4}, []byte(`{"agent":"codex","nativeId":"native-short"}`))
		if err != nil {
			t.Fatal(err)
		}
		addRemoteMetadata(t, store, layout, public, metadata)
		reader, err := OpenLegacyReplicaReader(context.Background(), store, "project", "short", "device", []*ecdh.PrivateKey{private})
		if err != nil {
			t.Fatalf("OpenLegacyReplicaReader: %v", err)
		}
		defer reader.Close()
		if _, err := reader.Next(context.Background()); err != nil {
			t.Fatalf("read existing record: %v", err)
		}
		if _, err := reader.Next(context.Background()); err == nil || !errors.Is(err, ErrIncompleteRemoteSession) {
			t.Fatalf("metadata disagreement error = %v, want ErrIncompleteRemoteSession", err)
		}
	})
}

func TestPushReplicaStreamPublishesGeneratedRecordsWithExpectedDigest(t *testing.T) {
	store := newConcurrentReplicaRemote()
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
	layout, err := NewReplicaLayout("hub", "project", "session", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "session", "device")
	const count = 700
	expected := EmptyDigest()
	for index := 0; index < count; index++ {
		expected = nextDigest(expected, generatedStreamRecord(index))
	}
	reader := &generatedRecordReader{count: count}
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	result, err := PushReplicaStreamWithCursorStore(context.Background(), store, public, layout, descriptor, state, reader, ReplicaStreamOptions{
		ReplicaPushOptions:  ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 64, MaxEncodedBytes: maxShardBytes}, Identities: []*ecdh.PrivateKey{private}},
		VerifyExpected:      true,
		ExpectedRecordCount: count,
		ExpectedHeadDigest:  expected,
	})
	if err != nil {
		t.Fatalf("PushReplicaStreamWithCursorStore: %v", err)
	}
	if result.Tip.ReplicaID == "" || result.Cursor.RecordCount != count || result.PublishedShards != (count+63)/64 || !reader.closed {
		t.Fatalf("stream result = %+v, reader closed=%t", result, reader.closed)
	}
	snapshot, err := FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica: %v", err)
	}
	if len(snapshot.Records) != count || snapshot.HeadDigest != expected || !bytes.Equal(snapshot.Records[count-1], generatedStreamRecord(count-1)) {
		t.Fatalf("snapshot = count:%d digest:%x", len(snapshot.Records), snapshot.HeadDigest)
	}
}

func TestPushReplicaStreamResumesAfterShardFailureWithoutRepublishingPrefix(t *testing.T) {
	base := newConcurrentReplicaRemote()
	failing := &streamFailRemote{Remote: base, failAt: 3}
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
	layout, err := NewReplicaLayout("hub", "project", "resume", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "resume", "device")
	const count = 300
	expected := EmptyDigest()
	for index := 0; index < count; index++ {
		expected = nextDigest(expected, generatedStreamRecord(index))
	}
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	first, err := PushReplicaStreamWithCursorStore(context.Background(), failing, public, layout, descriptor, state, &generatedRecordReader{count: count}, ReplicaStreamOptions{
		ReplicaPushOptions:  ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 256, MaxEncodedBytes: maxShardBytes}, Identities: []*ecdh.PrivateKey{private}},
		VerifyExpected:      true,
		ExpectedRecordCount: count,
		ExpectedHeadDigest:  expected,
	})
	if err == nil {
		t.Fatal("interrupted stream unexpectedly succeeded")
	}
	if first.Cursor.RecordCount != 256 || first.Cursor.NextShard != 2 || first.PublishedShards != 1 {
		t.Fatalf("interrupted stream result = %+v, err=%v", first, err)
	}
	cursor, err := state.Load(context.Background())
	if err != nil {
		t.Fatalf("load durable cursor: %v", err)
	}
	if cursor != first.Cursor {
		t.Fatalf("durable cursor = %+v, result cursor = %+v", cursor, first.Cursor)
	}

	failing.failAt = 0
	second, err := PushReplicaStreamWithCursorStore(context.Background(), failing, public, layout, descriptor, state, &generatedRecordReader{count: count}, ReplicaStreamOptions{
		ReplicaPushOptions:  ReplicaPushOptions{Plan: PlanOptions{MaxRecords: 256, MaxEncodedBytes: maxShardBytes}, Identities: []*ecdh.PrivateKey{private}},
		VerifyExpected:      true,
		ExpectedRecordCount: count,
		ExpectedHeadDigest:  expected,
	})
	if err != nil {
		t.Fatalf("resume stream: %v", err)
	}
	if second.Tip.ReplicaID == "" || second.Cursor.RecordCount != count || second.PublishedShards != 1 {
		t.Fatalf("resume result = %+v", second)
	}
	snapshot, err := FetchCompleteReplica(context.Background(), base, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica after resume: %v", err)
	}
	if len(snapshot.Records) != count || snapshot.HeadDigest != expected {
		t.Fatalf("resumed snapshot = count:%d digest:%x", len(snapshot.Records), snapshot.HeadDigest)
	}
}

func TestPushReplicaStreamRejectsUnexpectedEndWithoutPublishingTip(t *testing.T) {
	store := newConcurrentReplicaRemote()
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
	layout, err := NewReplicaLayout("hub", "project", "short", "replica", "device")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := testReplicaDescriptor("replica", "short", "device")
	result, err := PushReplicaStreamWithCursorStore(context.Background(), store, public, layout, descriptor, mustReplicaCursorStore(t, layout), &generatedRecordReader{count: 2}, ReplicaStreamOptions{
		ReplicaPushOptions:  ReplicaPushOptions{Plan: DefaultPlanOptions(), Identities: []*ecdh.PrivateKey{private}},
		VerifyExpected:      true,
		ExpectedRecordCount: 3,
		ExpectedHeadDigest:  [32]byte{9},
	})
	if err == nil || !errors.Is(err, ErrRecordStreamMismatch) {
		t.Fatalf("short stream result = %+v, err=%v", result, err)
	}
	if _, err := FetchReplicaTip(context.Background(), store, layout, []*ecdh.PrivateKey{private}); err == nil || !errors.Is(err, ErrReplicaTipMissing) {
		t.Fatalf("short stream unexpectedly published a tip: %v", err)
	}
}

type generatedRecordReader struct {
	count  int
	index  int
	closed bool
}

func (r *generatedRecordReader) Next(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, ErrRecordStreamClosed
	}
	if r.index == r.count {
		return nil, io.EOF
	}
	record := generatedStreamRecord(r.index)
	r.index++
	return record, nil
}

func (r *generatedRecordReader) Close() error {
	r.closed = true
	return nil
}

func generatedStreamRecord(index int) []byte {
	return []byte(fmt.Sprintf(`{"n":%d}`, index))
}

type streamFailRemote struct {
	remote.Remote
	failAt int
	puts   int
}

func (r *streamFailRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	r.puts++
	if r.failAt > 0 && r.puts == r.failAt {
		return errors.New("injected streamed Replica interruption")
	}
	return r.Remote.Put(ctx, key, body, size)
}

func mustReplicaCursorStore(t *testing.T, layout ReplicaLayout) ReplicaCursorStore {
	t.Helper()
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

var _ RecordReader = (*generatedRecordReader)(nil)
var _ remote.Remote = (*streamFailRemote)(nil)
