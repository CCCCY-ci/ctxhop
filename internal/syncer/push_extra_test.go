package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
)

func TestPushRejectsInvalidCursorBeforeInspectingThePart(t *testing.T) {
	part := ShardPart{Number: 1, Shard: mustShard(t, 0, EmptyDigest(), [][]byte{[]byte(`{"ok":true}`)})}
	if _, err := (PushCursor{}).Advance(part); !errors.Is(err, ErrInvalidPushCursor) {
		t.Fatalf("Advance error = %v, want ErrInvalidPushCursor", err)
	}
	if _, err := PlanAppend(PushCursor{}, part.Shard.Records, DefaultPlanOptions()); !errors.Is(err, ErrInvalidPushCursor) {
		t.Fatalf("PlanAppend error = %v, want ErrInvalidPushCursor", err)
	}
}

func TestPlanAppendHandlesHardAndConfiguredEnvelopeLimits(t *testing.T) {
	records := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":1}`)}
	one := mustShard(t, 0, EmptyDigest(), records[:1])
	encoded, err := one.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanAppend(NewPushCursor(), records, PlanOptions{MaxRecords: 2, MaxEncodedBytes: len(encoded) + 1}); err != nil {
		t.Fatalf("byte-limited PlanAppend: %v", err)
	}

	large := make([]byte, maxShardBytes+2)
	large[0] = '"'
	large[len(large)-1] = '"'
	for i := 1; i < len(large)-1; i++ {
		large[i] = 'x'
	}
	if _, err := PlanAppend(NewPushCursor(), [][]byte{large}, DefaultPlanOptions()); !errors.Is(err, ErrShardTooLarge) {
		t.Fatalf("hard-limit error = %v, want ErrShardTooLarge", err)
	}

	tooSmall := bytes.Repeat([]byte{'x'}, maxShardBytes)
	if _, err := PlanAppend(NewPushCursor(), [][]byte{tooSmall}, DefaultPlanOptions()); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid oversized record error = %v, want ErrInvalidRecord", err)
	}
}

func TestPutShardReportsSealAndPostSealCancellation(t *testing.T) {
	dataKey := newTestDataKey(t)
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

	if _, err := PutShard(context.Background(), &pushRemoteFake{}, &ecdh.PublicKey{}, layout, NewPushCursor(), part); err == nil {
		t.Fatal("PutShard accepted a zero public key")
	}

	ctx := &cancelAfterFirstCheckContext{}
	store := &pushRemoteFake{}
	if _, err := PutShard(ctx, store, public, layout, NewPushCursor(), part); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-seal cancellation error = %v, want context.Canceled", err)
	}
	if len(store.puts) != 0 {
		t.Fatalf("post-seal cancellation wrote %d objects", len(store.puts))
	}
}

func newTestDataKey(t *testing.T) *crypto.DataKey {
	t.Helper()
	return crypto.NewDataKey()
}

type cancelAfterFirstCheckContext struct {
	checks int
}

func (c *cancelAfterFirstCheckContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterFirstCheckContext) Done() <-chan struct{}       { return nil }
func (c *cancelAfterFirstCheckContext) Err() error {
	c.checks++
	if c.checks > 1 {
		return context.Canceled
	}
	return nil
}
func (c *cancelAfterFirstCheckContext) Value(any) any { return nil }
