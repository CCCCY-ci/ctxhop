package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

func TestAppendExecutorPublishesMetadataForItsDurableCursor(t *testing.T) {
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
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewAppendExecutor(store, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	cursor := PushCursor{NextShard: 2, RecordCount: 1, HeadDigest: [32]byte{1}}
	payload := []byte(`{"fingerprint":"opaque"}`)
	if err := executor.PublishMetadata(context.Background(), cursor, payload); err != nil {
		t.Fatal(err)
	}
	refs, err := FetchMetadata(context.Background(), store, "project", "session", private)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].DeviceID != "device" || refs[0].Metadata.RecordCount != cursor.RecordCount || refs[0].Metadata.HeadDigest != cursor.HeadDigest {
		t.Fatalf("metadata refs = %+v", refs)
	}
	if string(refs[0].Metadata.Payload) != string(payload) {
		t.Fatalf("metadata payload = %s, want %s", refs[0].Metadata.Payload, payload)
	}
}

func TestAppendExecutorPublishMetadataValidatesArguments(t *testing.T) {
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
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewAppendExecutor(store, public, layout, state, DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	validCursor := NewPushCursor()
	for name, test := range map[string]struct {
		ctx     context.Context
		cursor  PushCursor
		payload []byte
	}{
		"nil context":     {ctx: nil, cursor: validCursor, payload: []byte(`{"ok":true}`)},
		"invalid cursor":  {ctx: context.Background(), cursor: PushCursor{}, payload: []byte(`{"ok":true}`)},
		"invalid payload": {ctx: context.Background(), cursor: validCursor, payload: []byte(`not json`)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := executor.PublishMetadata(test.ctx, test.cursor, test.payload); err == nil {
				t.Fatal("PublishMetadata unexpectedly succeeded")
			}
		})
	}
	if err := (AppendExecutor{}).PublishMetadata(context.Background(), validCursor, []byte(`{"ok":true}`)); err == nil {
		t.Fatal("zero executor unexpectedly published metadata")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := executor.PublishMetadata(cancelled, validCursor, []byte(`{"ok":true}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PublishMetadata error = %v, want context.Canceled", err)
	}
}
