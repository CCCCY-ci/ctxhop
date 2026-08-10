// Package remote defines the storage backend abstraction.
//
// A Remote is deliberately dumb: it is an object store that can list, read,
// write, delete and stat opaque blobs, and nothing more. It is never asked to
// provide locking, transactions, conditional writes or read-after-write
// consistency. All correctness guarantees live in the layers above.
//
// This constraint is what allows a Remote to be an S3-compatible bucket, a
// plain directory synchronised by a third-party tool, or a USB drive. See the
// "dumb pipe" design principle and the storage layout in the PRD (§4 P3, §8.3).
//
// The layout guarantees that a given object key is only ever written by a
// single device, so two devices can never race on the same key. Implementations
// therefore do not need to handle write conflicts.
package remote

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get and Stat when the key does not exist.
//
// Implementations must map their backend-specific "no such key" error to this
// sentinel so callers can distinguish a missing object from a transport
// failure. Treating a transport failure as "missing" would let the sync layer
// conclude that another device has no data, which is never safe.
var ErrNotFound = errors.New("remote: object not found")

// ObjectInfo describes a stored object without exposing its contents.
type ObjectInfo struct {
	// Key is the full object key, relative to the configured root prefix.
	Key string

	// Size is the encrypted object size in bytes.
	Size int64

	// ModTime is the backend's last-modified timestamp. It is advisory only:
	// clock skew between devices means it must never be used to order versions.
	// Ordering is derived from the session content itself (§9.6).
	ModTime time.Time
}

// Remote is a content-agnostic object store.
//
// Every value written through this interface is already encrypted by the layer
// above (§4 P6). Implementations must not inspect, decompress or transform
// object contents.
//
// All methods must honour context cancellation and must fail fast rather than
// block indefinitely when the backend is unreachable (§11.2).
type Remote interface {
	// Name returns a short, stable identifier for this backend type, such as
	// "s3" or "dir". It is used in diagnostics and configuration.
	Name() string

	// List returns every object whose key starts with prefix.
	//
	// The result is not required to be sorted. Callers must tolerate eventually
	// consistent backends where a recently written object is not yet listed;
	// they must never interpret an absent key as proof that a device wrote
	// nothing.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Get opens the object for reading. The caller closes the returned reader.
	// It returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put writes size bytes from r to key, replacing any existing object.
	//
	// Callers only ever Put to keys owned by the local device, and shard objects
	// are immutable once written (BR-03, BR-04), so a Put is either a first
	// write or an idempotent retry of an identical payload.
	Put(ctx context.Context, key string, r io.Reader, size int64) error

	// Delete removes the object. Deleting a key that does not exist must
	// succeed, so that cleanup is idempotent.
	Delete(ctx context.Context, key string) error

	// Stat returns metadata for key without transferring its contents.
	// It returns ErrNotFound if the key does not exist.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}

// Prober is implemented by backends that can verify their configuration before
// any real work happens.
//
// `agentsync init` refuses to save a configuration that fails this check, so
// that credential and permission problems surface during setup rather than
// during the first sync (§9.1).
type Prober interface {
	// Probe verifies connectivity and read, write and delete permissions,
	// cleaning up anything it creates. The returned error must state which
	// operation failed and which permission is missing (§11.2).
	Probe(ctx context.Context) error
}
