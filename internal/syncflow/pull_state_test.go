package syncflow

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestObservedTipStateRoundTripsThroughSyncflow(t *testing.T) {
	layout, err := syncer.NewObjectLayout("project", "session", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	store, err := syncer.NewPullTipStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	want := []RemoteTip{
		{DeviceID: "devicec", RecordCount: 3, HeadDigest: [32]byte{3}},
		{DeviceID: "deviceb", RecordCount: 2, HeadDigest: [32]byte{2}},
	}
	if err := SaveObservedTips(context.Background(), store, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadObservedTips(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DeviceID != "deviceb" || got[1].DeviceID != "devicec" || got[0].HeadDigest != want[1].HeadDigest {
		t.Fatalf("observed tips = %+v", got)
	}
	if err := SaveObservedTips(context.Background(), store, []RemoteTip{{DeviceID: "Device", RecordCount: 1, HeadDigest: [32]byte{1}}}); !errors.Is(err, syncer.ErrInvalidPullTipState) {
		t.Fatalf("invalid observed tip error = %v, want ErrInvalidPullTipState", err)
	}
	if _, err := LoadObservedTips(nil, store); err == nil {
		t.Fatal("nil context LoadObservedTips unexpectedly succeeded")
	}
	if err := SaveObservedTips(nil, store, nil); err == nil {
		t.Fatal("nil context SaveObservedTips unexpectedly succeeded")
	}
}
