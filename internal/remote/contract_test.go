package remote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// runContract exercises every behaviour the sync layer relies on, against any
// driver.
//
// One suite for all drivers is the point: the layout's safety rests on every
// backend behaving identically, and a difference that only shows up on the one
// nobody tests locally is exactly the kind of bug that reaches a user.
func runContract(t *testing.T, newStore func(t *testing.T) Remote) {
	t.Helper()

	put := func(t *testing.T, s Remote, key, body string) {
		t.Helper()
		if err := s.Put(context.Background(), key, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	read := func(t *testing.T, s Remote, key string) string {
		t.Helper()
		r, err := s.Get(context.Background(), key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read %q: %v", key, err)
		}
		return string(data)
	}

	t.Run("round trip", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/projects/p/sessions/s/dev/000001", "shard one")
		if got := read(t, s, "v1/projects/p/sessions/s/dev/000001"); got != "shard one" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("binary content survives", func(t *testing.T) {
		// Shards are ciphertext. Any transformation - newline translation,
		// encoding, trimming - would corrupt them.
		s := newStore(t)
		body := string([]byte{0x00, 0x0d, 0x0a, 0x1a, 0xff, 0xfe, 0x80})
		put(t, s, "v1/blob", body)
		if got := read(t, s, "v1/blob"); got != body {
			t.Errorf("got % x, want % x", got, body)
		}
	})

	t.Run("empty object", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/empty", "")
		if got := read(t, s, "v1/empty"); got != "" {
			t.Errorf("got %q", got)
		}
		info, err := s.Stat(context.Background(), "v1/empty")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != 0 {
			t.Errorf("Size = %d", info.Size)
		}
	})

	t.Run("missing object reports ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(context.Background(), "v1/absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get: got %v, want ErrNotFound", err)
		}
		if _, err := s.Stat(context.Background(), "v1/absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat: got %v, want ErrNotFound", err)
		}
	})

	t.Run("get and stat agree about what exists", func(t *testing.T) {
		// A key that both report as present must be readable, and one that
		// either reports as absent must not be readable by the other. A
		// disagreement here would let the sync layer believe a shard exists
		// when nothing can be read from it.
		s := newStore(t)
		put(t, s, "v1/projects/p/sessions/s/dev/000001", "x")

		for _, key := range []string{
			"v1/projects/p/sessions/s/dev",
			"v1/projects/p",
			"v1/projects/p/sessions/s/dev/000002",
		} {
			_, statErr := s.Stat(context.Background(), key)
			r, getErr := s.Get(context.Background(), key)
			if getErr == nil {
				r.Close()
			}
			if errors.Is(statErr, ErrNotFound) != errors.Is(getErr, ErrNotFound) {
				t.Errorf("%q: Stat says %v but Get says %v", key, statErr, getErr)
			}
		}
	})

	t.Run("stat does not transfer contents", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/sized", "0123456789")
		info, err := s.Stat(context.Background(), "v1/sized")
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.Size != 10 {
			t.Errorf("Size = %d, want 10", info.Size)
		}
		if info.Key != "v1/sized" {
			t.Errorf("Key = %q", info.Key)
		}
	})

	t.Run("list by prefix", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/projects/a/sessions/s/dev1/000001", "1")
		put(t, s, "v1/projects/a/sessions/s/dev1/000002", "2")
		put(t, s, "v1/projects/a/sessions/s/dev2/000001", "3")
		put(t, s, "v1/projects/b/sessions/s/dev1/000001", "4")

		got := keysOf(t, s, "v1/projects/a/")
		want := []string{
			"v1/projects/a/sessions/s/dev1/000001",
			"v1/projects/a/sessions/s/dev1/000002",
			"v1/projects/a/sessions/s/dev2/000001",
		}
		if !equalKeys(got, want) {
			t.Errorf("got %v\nwant %v", got, want)
		}

		// A prefix that stops mid-segment still matches, as it does in S3.
		if got := keysOf(t, s, "v1/projects/a/sessions/s/dev1/0000"); len(got) != 2 {
			t.Errorf("partial-segment prefix returned %v", got)
		}
	})

	t.Run("list of an empty prefix is empty, not an error", func(t *testing.T) {
		s := newStore(t)
		got, err := s.List(context.Background(), "v1/nothing/here/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("list everything", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/a", "1")
		put(t, s, "v1/b/c", "2")
		if got := keysOf(t, s, ""); len(got) != 2 {
			t.Errorf("got %v", got)
		}
	})

	t.Run("overwrite replaces the whole object", func(t *testing.T) {
		// Shards are immutable by convention, but metadata objects are
		// rewritten, and a partial overwrite would leave a mixture.
		s := newStore(t)
		put(t, s, "v1/meta", "a longer original value")
		put(t, s, "v1/meta", "short")
		if got := read(t, s, "v1/meta"); got != "short" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/gone", "x")
		for i := 0; i < 2; i++ {
			if err := s.Delete(context.Background(), "v1/gone"); err != nil {
				t.Fatalf("Delete %d: %v", i, err)
			}
		}
		if _, err := s.Get(context.Background(), "v1/gone"); !errors.Is(err, ErrNotFound) {
			t.Errorf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("unsafe keys are refused by every operation", func(t *testing.T) {
		s := newStore(t)
		for _, key := range []string{
			"", "/absolute", "..", "v1/../escape", "v1//empty", `v1\backslash`,
			"C:/drive", "v1/trailing/", "v1/space ", "v1/dot.",
		} {
			ctx := context.Background()
			if err := s.Put(ctx, key, strings.NewReader("x"), 1); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q): got %v, want ErrInvalidKey", key, err)
			}
			if _, err := s.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q): got %v, want ErrInvalidKey", key, err)
			}
			if _, err := s.Stat(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Stat(%q): got %v, want ErrInvalidKey", key, err)
			}
			if err := s.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete(%q): got %v, want ErrInvalidKey", key, err)
			}
		}
	})

	t.Run("a cancelled context stops the operation", func(t *testing.T) {
		s := newStore(t)
		put(t, s, "v1/x", "x")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Every operation, not just the slow-looking ones: a caller that gives
		// up mid-sync expects the whole store to stop, and a Delete that
		// carries on regardless would act after the decision to abort.
		if _, err := s.Get(ctx, "v1/x"); err == nil {
			t.Error("Get ignored a cancelled context")
		}
		if err := s.Put(ctx, "v1/y", strings.NewReader("y"), 1); err == nil {
			t.Error("Put ignored a cancelled context")
		}
		if _, err := s.List(ctx, ""); err == nil {
			t.Error("List ignored a cancelled context")
		}
		if _, err := s.Stat(ctx, "v1/x"); err == nil {
			t.Error("Stat ignored a cancelled context")
		}
		if err := s.Delete(ctx, "v1/x"); err == nil {
			t.Error("Delete ignored a cancelled context")
		}

		// And the object it was told not to delete is still there.
		if _, err := s.Stat(context.Background(), "v1/x"); err != nil {
			t.Errorf("a cancelled Delete removed the object anyway: %v", err)
		}
	})

	t.Run("large object", func(t *testing.T) {
		s := newStore(t)
		body := strings.Repeat("abcdefgh", 200_000) // 1.6 MB
		put(t, s, "v1/large", body)
		if got := read(t, s, "v1/large"); got != body {
			t.Errorf("large object came back wrong: %d bytes, want %d", len(got), len(body))
		}
	})

	t.Run("name is reported", func(t *testing.T) {
		if newStore(t).Name() == "" {
			t.Error("a backend must identify itself for configuration and diagnostics")
		}
	})
}

func keysOf(t *testing.T, s Remote, prefix string) []string {
	t.Helper()
	infos, err := s.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("List(%q): %v", prefix, err)
	}
	keys := make([]string, len(infos))
	for i, info := range infos {
		keys[i] = info.Key
	}
	sort.Strings(keys)
	return keys
}

func equalKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	sort.Strings(want)
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// readAllString is used by driver-specific tests that need the body directly.
func readAllString(t *testing.T, r io.Reader) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf.String()
}
