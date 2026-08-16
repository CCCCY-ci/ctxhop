package remoteexample

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

// Store is a complete in-memory Remote implementation for adapter experiments
// and contract tests. It is deliberately not a production backend: process
// restarts discard its objects.
type Store struct {
	mu      sync.RWMutex
	objects map[string]object
}

type object struct {
	data    []byte
	modTime time.Time
}

var _ remote.Remote = (*Store)(nil)

// New returns an empty example store.
func New() *Store {
	return &Store{objects: make(map[string]object)}
}

// Name implements remote.Remote.
func (s *Store) Name() string {
	return "example-memory"
}

// List implements remote.Remote. Object order is made deterministic for tests;
// callers must not depend on that ordering.
func (s *Store) List(ctx context.Context, prefix string) ([]remote.ObjectInfo, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]remote.ObjectInfo, 0)
	for key, value := range s.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		result = append(result, remote.ObjectInfo{
			Key:     key,
			Size:    int64(len(value.data)),
			ModTime: value.modTime,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})
	return result, nil
}

// Get implements remote.Remote.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	value, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return nil, remote.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), value.data...))), nil
}

// Put implements remote.Remote. The example verifies the declared size and
// rejects a short or long body instead of silently storing a truncated object.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if body == nil {
		return errors.New("remote example: nil object body")
	}
	if size < 0 {
		return errors.New("remote example: negative object size")
	}
	if err := contextError(ctx); err != nil {
		return err
	}

	data, err := readSized(ctx, body, size)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	s.mu.Lock()
	s.objects[key] = object{
		data:    append([]byte(nil), data...),
		modTime: now,
	}
	s.mu.Unlock()
	return nil
}

// Delete implements remote.Remote. Missing objects are already successful.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

// Stat implements remote.Remote.
func (s *Store) Stat(ctx context.Context, key string) (remote.ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return remote.ObjectInfo{}, err
	}
	if err := contextError(ctx); err != nil {
		return remote.ObjectInfo{}, err
	}

	s.mu.RLock()
	value, ok := s.objects[key]
	s.mu.RUnlock()
	if !ok {
		return remote.ObjectInfo{}, remote.ErrNotFound
	}
	return remote.ObjectInfo{
		Key:     key,
		Size:    int64(len(value.data)),
		ModTime: value.modTime,
	}, nil
}

func validateKey(key string) error {
	if key == "" || strings.ContainsRune(key, 0) {
		return errors.New("remote example: invalid object key")
	}
	clean := path.Clean(key)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(key, "/") {
		return fmt.Errorf("remote example: unsafe object key %q", key)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("remote example: nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := contextError(r.ctx); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func readSized(ctx context.Context, body io.Reader, size int64) ([]byte, error) {
	reader := contextReader{ctx: ctx, r: body}
	data, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil {
		return nil, fmt.Errorf("remote example: read object: %w", err)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("remote example: object size mismatch: declared %d, read %d", size, len(data))
	}
	if size == math.MaxInt64 {
		return data, nil
	}

	var extra [1]byte
	for {
		n, err := reader.Read(extra[:])
		if n > 0 {
			return nil, fmt.Errorf("remote example: object is larger than declared size %d", size)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return data, nil
			}
			return nil, fmt.Errorf("remote example: read object trailer: %w", err)
		}
	}
}
