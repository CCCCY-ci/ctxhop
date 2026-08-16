package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestPullTipStoreRoundTripSortsAndReplacesAtomically(t *testing.T) {
	layout, err := NewObjectLayout("project", "session", "devicea")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPullTipStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	tips := []PullTip{
		{DeviceID: "devicec", RecordCount: 3, HeadDigest: [32]byte{3}},
		{DeviceID: "deviceb", RecordCount: 2, HeadDigest: [32]byte{2}},
	}
	if err := store.Save(context.Background(), tips); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].DeviceID != "deviceb" || loaded[1].DeviceID != "devicec" {
		t.Fatalf("loaded tips = %+v", loaded)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), tips); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("saving the same pull tips produced different bytes")
	}
	if strings.Index(string(second), `"deviceb"`) > strings.Index(string(second), `"devicec"`) {
		t.Fatal("pull tips were not sorted by device ID")
	}
	entries, err := os.ReadDir(path[:strings.LastIndexAny(path, `\\/`)])
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary pull state file survived: %s", entry.Name())
		}
	}
}

func TestPullTipStoreHandlesAbsentStateAndMalformedWire(t *testing.T) {
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPullTipStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil || len(loaded) != 0 {
		t.Fatalf("absent state = %+v, error = %v", loaded, err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path[:strings.LastIndexAny(path, `\\/`)], 0o700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("0", 64)
	valid := `{"version":1,"tips":[{"deviceId":"deviceb","recordCount":1,"headDigest":"` + digest + `"}]}`
	for name, wire := range map[string]string{
		"unknown field": strings.TrimSuffix(valid, `}`) + `,"extra":1}`,
		"trailing json": valid + `{}`,
		"trailing text": valid + `x`,
		"newer version": strings.Replace(valid, `"version":1`, `"version":99`, 1),
		"older version": strings.Replace(valid, `"version":1`, `"version":0`, 1),
		"bad digest":    strings.Replace(valid, digest, strings.Repeat("0", 63), 1),
		"duplicate":     strings.TrimSuffix(valid, `]}`) + `,{"deviceId":"deviceb","recordCount":1,"headDigest":"` + digest + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("malformed pull tip state unexpectedly loaded")
			}
			if name == "newer version" {
				if !errors.Is(err, ErrUnsupportedPullTipState) {
					t.Fatalf("error = %v, want ErrUnsupportedPullTipState", err)
				}
			} else if name == "duplicate" {
				if !errors.Is(err, ErrDuplicatePullTip) {
					t.Fatalf("error = %v, want ErrDuplicatePullTip", err)
				}
			} else if !errors.Is(err, ErrInvalidPullTipState) {
				t.Fatalf("error = %v, want ErrInvalidPullTipState", err)
			}
		})
	}
}

func TestPullTipStoreRejectsInvalidTipsAndCancellation(t *testing.T) {
	for _, tip := range []PullTip{
		{DeviceID: "Device", RecordCount: 1, HeadDigest: [32]byte{1}},
		{DeviceID: "device", RecordCount: 0, HeadDigest: [32]byte{1}},
	} {
		if err := tip.Validate(); !errors.Is(err, ErrInvalidPullTipState) {
			t.Errorf("tip %+v error = %v, want ErrInvalidPullTipState", tip, err)
		}
	}
	layout, err := NewObjectLayout("project", "session", "device")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPullTipStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), []PullTip{{DeviceID: "deviceb", RecordCount: 1, HeadDigest: [32]byte{1}}, {DeviceID: "deviceb", RecordCount: 2, HeadDigest: [32]byte{2}}}); !errors.Is(err, ErrDuplicatePullTip) {
		t.Fatalf("duplicate save error = %v, want ErrDuplicatePullTip", err)
	}
	tooMany := make([]PullTip, maxPullTips+1)
	for i := range tooMany {
		tooMany[i] = PullTip{DeviceID: fmt.Sprintf("device%04d", i), RecordCount: 1, HeadDigest: [32]byte{1}}
	}
	if err := store.Save(context.Background(), tooMany); !errors.Is(err, ErrInvalidPullTipState) {
		t.Fatalf("oversized save error = %v, want ErrInvalidPullTipState", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Save(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(nil); err == nil {
		t.Fatal("nil context load unexpectedly succeeded")
	}
	if err := store.Save(nil, nil); err == nil {
		t.Fatal("nil context save unexpectedly succeeded")
	}
	if _, err := NewPullTipStore("", layout); err == nil {
		t.Fatal("empty pull tip root unexpectedly succeeded")
	}
	if _, err := NewPullTip("device", 0, [32]byte{1}); !errors.Is(err, ErrInvalidPullTipState) {
		t.Fatalf("invalid empty tip error = %v, want ErrInvalidPullTipState", err)
	}
}
