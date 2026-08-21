package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

func TestPushCursorAdvancesOnlyAcrossTheExpectedShard(t *testing.T) {
	cursor := NewPushCursor()
	if err := cursor.Validate(); err != nil {
		t.Fatalf("initial cursor: %v", err)
	}
	if cursor.NextShard != 1 || cursor.RecordCount != 0 || cursor.HeadDigest != EmptyDigest() {
		t.Fatalf("initial cursor = %+v", cursor)
	}

	records := [][]byte{[]byte(`{"n":1}`)}
	part, err := NewShard(0, EmptyDigest(), records)
	if err != nil {
		t.Fatal(err)
	}
	next, err := cursor.Advance(ShardPart{Number: 1, Shard: part})
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	wantDigest, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	if next.NextShard != 2 || next.RecordCount != 1 || next.HeadDigest != wantDigest {
		t.Fatalf("advanced cursor = %+v", next)
	}

	for name, candidate := range map[string]ShardPart{
		"wrong sequence": {Number: 2, Shard: part},
		"wrong base":     {Number: 1, Shard: mustShard(t, 1, EmptyDigest(), records)},
		"wrong digest":   {Number: 1, Shard: mustShard(t, 0, wantDigest, records)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := cursor.Advance(candidate); !errors.Is(err, ErrInvalidPushPart) {
				t.Fatalf("Advance error = %v, want ErrInvalidPushPart", err)
			}
		})
	}
	invalid, err := NewShard(0, EmptyDigest(), [][]byte{[]byte(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	invalid.Records = nil
	if _, err := cursor.Advance(ShardPart{Number: 1, Shard: invalid}); !errors.Is(err, ErrInvalidPushPart) {
		t.Fatalf("invalid shard error = %v, want ErrInvalidPushPart", err)
	}

	for name, candidate := range map[string]PushCursor{
		"zero":          {},
		"sequence high": {NextShard: maxShardNumber + 2},
		"wrong digest":  {NextShard: 1, HeadDigest: [32]byte{1}},
	} {
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidPushCursor) {
			t.Errorf("%s cursor error = %v, want ErrInvalidPushCursor", name, err)
		}
	}
	maxCursor := PushCursor{NextShard: maxShardNumber + 1, RecordCount: 1, HeadDigest: wantDigest}
	if _, err := maxCursor.Advance(ShardPart{Number: maxShardNumber + 1, Shard: part}); !errors.Is(err, ErrInvalidPushPart) {
		t.Fatalf("exhausted cursor error = %v, want ErrInvalidPushPart", err)
	}
	overflowCursor := PushCursor{NextShard: 1, RecordCount: ^uint64(0), HeadDigest: wantDigest}
	overflowPart := mustShard(t, ^uint64(0), wantDigest, records)
	if _, err := overflowCursor.Advance(ShardPart{Number: 1, Shard: overflowPart}); !errors.Is(err, ErrInvalidPushPart) {
		t.Fatalf("overflow cursor error = %v, want ErrInvalidPushPart", err)
	}
}

func TestPlanAppendBuildsIndependentOrderedParts(t *testing.T) {
	records := [][]byte{
		[]byte(`{"n":1}`),
		[]byte(`{"n":2}`),
		[]byte(`{"n":3}`),
		[]byte(`{"n":4}`),
	}
	plan, err := PlanAppend(NewPushCursor(), records, PlanOptions{MaxRecords: 2, MaxEncodedBytes: maxShardBytes})
	if err != nil {
		t.Fatalf("PlanAppend: %v", err)
	}
	if len(plan.Parts) != 2 || plan.Parts[0].Number != 1 || plan.Parts[1].Number != 2 {
		t.Fatalf("parts = %+v", plan.Parts)
	}
	if plan.Parts[0].Shard.Base != 0 || plan.Parts[1].Shard.Base != 2 {
		t.Fatalf("part bases = %d, %d", plan.Parts[0].Shard.Base, plan.Parts[1].Shard.Base)
	}
	if plan.Next.NextShard != 3 || plan.Next.RecordCount != 4 {
		t.Fatalf("next cursor = %+v", plan.Next)
	}
	wantHead, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Next.HeadDigest != wantHead {
		t.Fatalf("head digest = %x, want %x", plan.Next.HeadDigest, wantHead)
	}

	records[2][5] = '9'
	if bytes.Equal(plan.Parts[1].Shard.Records[0], records[2]) {
		t.Fatal("plan retained a caller-owned record buffer")
	}

	_, err = PlanAppend(plan.Next, records, DefaultPlanOptions())
	if err == nil || !errors.Is(err, ErrLocalHistoryChanged) {
		// The input was deliberately changed after planning, so the cursor must
		// refuse the changed prefix rather than silently republishing it.
		t.Fatalf("changed history error = %v, want ErrLocalHistoryChanged", err)
	}

	unchanged := append([][]byte(nil), records...)
	unchanged[2] = []byte(`{"n":3}`)
	empty, err := PlanAppend(plan.Next, unchanged, DefaultPlanOptions())
	if err != nil {
		t.Fatalf("empty PlanAppend: %v", err)
	}
	if len(empty.Parts) != 0 || empty.Next != plan.Next {
		t.Fatalf("empty plan = %+v", empty)
	}

	firstCursor, err := NewPushCursor().Advance(plan.Parts[0])
	if err != nil {
		t.Fatal(err)
	}
	suffix, err := PlanAppend(firstCursor, unchanged, PlanOptions{MaxRecords: 2, MaxEncodedBytes: maxShardBytes})
	if err != nil {
		t.Fatalf("suffix PlanAppend: %v", err)
	}
	if len(suffix.Parts) != 1 || suffix.Parts[0].Number != 2 {
		t.Fatalf("suffix parts = %+v", suffix.Parts)
	}
}

func TestPlanAppendRefusesChangedInputAndEnforcesLimits(t *testing.T) {
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}
	initial := NewPushCursor()
	firstPlan, err := PlanAppend(initial, records, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	cursor := firstPlan.Next

	if _, err := PlanAppend(cursor, records[:1], DefaultPlanOptions()); !errors.Is(err, ErrLocalHistoryChanged) {
		t.Fatalf("truncated history error = %v, want ErrLocalHistoryChanged", err)
	}
	changed := [][]byte{[]byte(`{"n":9}`), records[1]}
	if _, err := PlanAppend(cursor, changed, DefaultPlanOptions()); !errors.Is(err, ErrLocalHistoryChanged) {
		t.Fatalf("diverged history error = %v, want ErrLocalHistoryChanged", err)
	}
	if _, err := PlanAppend(initial, [][]byte{[]byte("not json")}, DefaultPlanOptions()); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid record error = %v, want ErrInvalidRecord", err)
	}

	for name, options := range map[string]PlanOptions{
		"zero records": {MaxEncodedBytes: maxShardBytes},
		"zero bytes":   {MaxRecords: 1},
		"too large":    {MaxRecords: 1, MaxEncodedBytes: maxShardBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PlanAppend(initial, records, options); err == nil {
				t.Fatal("PlanAppend unexpectedly accepted invalid options")
			}
		})
	}

	one, err := NewShard(0, EmptyDigest(), [][]byte{records[0]})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := one.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanAppend(initial, records[:1], PlanOptions{MaxRecords: 1, MaxEncodedBytes: len(encoded) - 1}); !errors.Is(err, ErrShardTooLarge) {
		t.Fatalf("oversized single record error = %v, want ErrShardTooLarge", err)
	}
	if _, err := PlanAppend(PushCursor{NextShard: maxShardNumber, HeadDigest: EmptyDigest()}, [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`)}, PlanOptions{MaxRecords: 1, MaxEncodedBytes: maxShardBytes}); !errors.Is(err, ErrShardSequenceExhausted) {
		t.Fatalf("sequence exhaustion error = %v, want ErrShardSequenceExhausted", err)
	}
}

func TestShardEncodedSizeMatchesMarshalBinary(t *testing.T) {
	tests := []struct {
		base    uint64
		records [][]byte
	}{
		{base: 0, records: [][]byte{[]byte("{\"n\":1}")}},
		{base: 9, records: [][]byte{[]byte("{\"text\":\"hello\"}"), []byte("{\"n\":2}")}},
		{base: 999999, records: [][]byte{[]byte("{\"escaped\":\"<>&\"}"), []byte("{\"array\":[1,2,3]}"), []byte("{\"unicode\":\"路径\"}")}},
	}
	for _, test := range tests {
		shard, err := NewShard(test.base, EmptyDigest(), test.records)
		if err != nil {
			t.Fatalf("NewShard(base=%d): %v", test.base, err)
		}
		encoded, err := shard.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary(base=%d): %v", test.base, err)
		}
		recordBytes := 0
		for _, record := range test.records {
			recordBytes += len(record)
		}
		if got := shardEncodedSize(test.base, uint64(len(test.records)), recordBytes); got != len(encoded) {
			t.Fatalf("shardEncodedSize(base=%d) = %d, want %d", test.base, got, len(encoded))
		}
	}
}

func TestPutShardPublishesOnlyCiphertextAndReturnsProgress(t *testing.T) {
	store := &pushRemoteFake{objects: make(map[string][]byte)}
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
	records := [][]byte{[]byte(`{"message":"synthetic"}`)}
	part := ShardPart{Number: 1, Shard: mustShard(t, 0, EmptyDigest(), records)}
	next, err := PutShard(context.Background(), store, public, layout, NewPushCursor(), part)
	if err != nil {
		t.Fatalf("PutShard: %v", err)
	}
	if next.RecordCount != 1 || next.NextShard != 2 {
		t.Fatalf("next cursor = %+v", next)
	}
	key, err := layout.ShardKey(1)
	if err != nil {
		t.Fatal(err)
	}
	sealed := store.objects[key]
	if len(sealed) == 0 || bytes.Contains(sealed, records[0]) {
		t.Fatal("remote fake received empty or plaintext shard")
	}
	opened, err := OpenShard(private, key, sealed)
	if err != nil {
		t.Fatalf("OpenShard: %v", err)
	}
	if !recordsEqual(opened.Records, records) {
		t.Fatalf("opened records = %q, want %q", opened.Records, records)
	}
	if store.puts[0].size != int64(len(sealed)) {
		t.Fatalf("Put size = %d, want %d", store.puts[0].size, len(sealed))
	}

	// A retry uses the same cursor and logical part. It may produce different
	// randomized ciphertext, but it must not create another sequence number.
	if _, err := PutShard(context.Background(), store, public, layout, NewPushCursor(), part); err != nil {
		t.Fatalf("retry PutShard: %v", err)
	}
	if len(store.puts) != 2 || len(store.objects) != 1 {
		t.Fatalf("retry writes = %d, objects = %d", len(store.puts), len(store.objects))
	}
	if _, err := PutShard(context.Background(), store, public, layout, next, part); !errors.Is(err, ErrInvalidPushPart) {
		t.Fatalf("reusing advanced cursor error = %v, want ErrInvalidPushPart", err)
	}
}

func TestPutShardStopsBeforeRemoteWriteOnArgumentsCancellationOrFailure(t *testing.T) {
	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	part := ShardPart{Number: 1, Shard: mustShard(t, 0, EmptyDigest(), [][]byte{[]byte(`{"ok":true}`)})}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() error{
		"cancelled": func() error {
			_, err := PutShard(cancelled, &pushRemoteFake{}, public, layout, NewPushCursor(), part)
			return err
		},
		"nil context": func() error {
			_, err := PutShard(nil, &pushRemoteFake{}, public, layout, NewPushCursor(), part)
			return err
		},
		"nil remote": func() error {
			_, err := PutShard(context.Background(), nil, public, layout, NewPushCursor(), part)
			return err
		},
		"nil recipient": func() error {
			_, err := PutShard(context.Background(), &pushRemoteFake{}, nil, layout, NewPushCursor(), part)
			return err
		},
		"invalid layout": func() error {
			_, err := PutShard(context.Background(), &pushRemoteFake{}, public, ObjectLayout{}, NewPushCursor(), part)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("PutShard unexpectedly succeeded")
			}
		})
	}

	putErr := errors.New("synthetic put failure")
	store := &pushRemoteFake{putErr: putErr}
	if next, err := PutShard(context.Background(), store, public, layout, NewPushCursor(), part); err == nil || !errors.Is(err, putErr) || next != (PushCursor{}) {
		t.Fatalf("failed PutShard = %+v, %v", next, err)
	}
	if len(store.puts) != 1 {
		t.Fatalf("failed write calls = %d, want 1", len(store.puts))
	}
}

type pushWrite struct {
	key  string
	body []byte
	size int64
}

type pushRemoteFake struct {
	objects map[string][]byte
	puts    []pushWrite
	putErr  error
}

func (f *pushRemoteFake) Name() string { return "push-fake" }

func (f *pushRemoteFake) List(context.Context, string) ([]remote.ObjectInfo, error) {
	return nil, nil
}

func (f *pushRemoteFake) Get(_ context.Context, key string) (io.ReadCloser, error) {
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", remote.ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *pushRemoteFake) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.puts = append(f.puts, pushWrite{key: key, body: append([]byte(nil), body...), size: size})
	if f.putErr != nil {
		return f.putErr
	}
	if size >= 0 && int64(len(body)) != size {
		return fmt.Errorf("size = %d, want %d", len(body), size)
	}
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	f.objects[key] = append([]byte(nil), body...)
	return nil
}

func (f *pushRemoteFake) Delete(context.Context, string) error { return nil }

func (f *pushRemoteFake) Stat(context.Context, string) (remote.ObjectInfo, error) {
	return remote.ObjectInfo{}, remote.ErrNotFound
}

var _ remote.Remote = (*pushRemoteFake)(nil)
