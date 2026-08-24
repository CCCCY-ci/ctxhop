package syncer

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalFileLockSerializesIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "v1", "push.lock")
	first, err := AcquireLocalFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		second, err := AcquireLocalFileLock(context.Background(), path)
		if err != nil {
			done <- err
			return
		}
		done <- second.Close()
	}()
	<-started

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("second lock acquired while first was held: %v", err)
	case <-timer.C:
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second lock did not acquire after first was released")
	}
}

func TestLocalFileLockHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "v1", "push.lock")
	first, err := AcquireLocalFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AcquireLocalFileLock(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled lock error = %v, want context deadline exceeded", err)
	}
}
