package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreStatsStoreRoundTripAndLocalNoOp(t *testing.T) {
	store, err := NewRestoreStatsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	empty, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty != (RestoreStats{}) {
		t.Fatalf("absent stats = %+v, want zero", empty)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent stats path error = %v, want not-exist", err)
	}

	now := time.Date(2026, 8, 15, 2, 5, 0, 123000000, time.FixedZone("test", 8*60*60))
	got, err := store.RecordRestore(ctx, "localdevice", []string{"foreign", "localdevice", "foreign"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.CrossDeviceRestores != 1 || !got.LastRestoredAt.Equal(now.UTC()) {
		t.Fatalf("recorded stats = %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantWire := fmt.Sprintf("{\"version\":1,\"crossDeviceRestores\":1,\"lastRestoredAt\":%q}\n", now.UTC().Format(time.RFC3339Nano))
	if string(data) != wantWire {
		t.Fatalf("stats wire = %q, want %q", data, wantWire)
	}
	before := append([]byte(nil), data...)

	if _, err := store.RecordRestore(ctx, "localdevice", []string{"localdevice", "localdevice"}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("same-device restore changed stats: before=%q after=%q", before, after)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CrossDeviceRestores != 1 || !loaded.LastRestoredAt.Equal(now.UTC()) {
		t.Fatalf("round-trip stats = %+v", loaded)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary stats file survived: %s", entry.Name())
		}
	}
}

func TestRestoreStatsStoreRejectsMalformedState(t *testing.T) {
	store, err := NewRestoreStatsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	timestamp := "2026-08-15T02:05:00Z"
	valid := fmt.Sprintf("{\"version\":1,\"crossDeviceRestores\":1,\"lastRestoredAt\":%q}", timestamp)
	for name, wire := range map[string]string{
		"unknown field":  strings.TrimSuffix(valid, "}") + ",\"extra\":1}",
		"trailing json":  valid + "{}",
		"trailing text":  valid + "x",
		"newer version":  strings.Replace(valid, "\"version\":1", "\"version\":99", 1),
		"older version":  strings.Replace(valid, "\"version\":1", "\"version\":0", 1),
		"bad timestamp":  strings.Replace(valid, timestamp, "not-a-time", 1),
		"zero timestamp": strings.Replace(valid, timestamp, "0001-01-01T00:00:00Z", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("malformed statistics unexpectedly loaded")
			}
			if name == "newer version" {
				if !errors.Is(err, ErrUnsupportedRestoreStats) {
					t.Fatalf("error = %v, want ErrUnsupportedRestoreStats", err)
				}
			} else if !errors.Is(err, ErrInvalidRestoreStats) {
				t.Fatalf("error = %v, want ErrInvalidRestoreStats", err)
			}
		})
	}
}

func TestRestoreStatsStoreRejectsInvalidInputsAndCancellation(t *testing.T) {
	store, err := NewRestoreStatsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 15, 2, 5, 0, 0, time.UTC)
	for name, input := range map[string]struct {
		local   string
		sources []string
		stamp   time.Time
	}{
		"empty local":   {local: "", sources: []string{"foreign"}, stamp: now},
		"bad local":     {local: "Local", sources: []string{"foreign"}, stamp: now},
		"empty sources": {local: "localdevice", sources: nil, stamp: now},
		"bad source":    {local: "localdevice", sources: []string{"Foreign"}, stamp: now},
		"zero time":     {local: "localdevice", sources: []string{"foreign"}, stamp: time.Time{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.RecordRestore(context.Background(), input.local, input.sources, input.stamp); !errors.Is(err, ErrInvalidRestoreStats) {
				t.Fatalf("error = %v, want ErrInvalidRestoreStats", err)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load error = %v, want context.Canceled", err)
	}
	if _, err := store.RecordRestore(cancelled, "localdevice", []string{"foreign"}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled record error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(nil); err == nil {
		t.Fatal("nil context load unexpectedly succeeded")
	}
	if _, err := store.RecordRestore(nil, "localdevice", []string{"foreign"}, now); err == nil {
		t.Fatal("nil context record unexpectedly succeeded")
	}
	if _, err := NewRestoreStatsStore(""); err == nil {
		t.Fatal("empty statistics root unexpectedly succeeded")
	}
}

func TestRestoreStatsStoreRejectsCountOverflow(t *testing.T) {
	store, err := NewRestoreStatsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.filePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	wire := fmt.Sprintf("{\"version\":1,\"crossDeviceRestores\":%d,\"lastRestoredAt\":\"2026-08-15T02:05:00Z\"}", ^uint64(0))
	if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordRestore(context.Background(), "localdevice", []string{"foreign"}, time.Now().UTC())
	if !errors.Is(err, ErrInvalidRestoreStats) {
		t.Fatalf("overflow error = %v, want ErrInvalidRestoreStats", err)
	}
}
