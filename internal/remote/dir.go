package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

// Dir stores objects as files under a directory.
//
// It serves two purposes: it is the infrastructure the integration tests run
// against, and it lets someone start with a folder their existing sync tool
// already carries between machines instead of registering for object storage.
//
// The second use is why writes are atomic here even though a local filesystem
// would not require it: a third-party sync tool watching the directory must
// never observe half a shard.
type Dir struct {
	// Root is the directory objects live under.
	Root string
}

// NewDir returns a Dir rooted at an absolute path.
func NewDir(root string) (*Dir, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("remote: no directory configured")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve directory: %w", err)
	}
	return &Dir{Root: abs}, nil
}

// Name identifies this backend in configuration and diagnostics.
func (d *Dir) Name() string { return "dir" }

// pathFor turns a key into a filesystem path under Root.
//
// The key is validated first, every time. This is the point where an
// externally supplied string becomes a path, so it is where a `..` would
// escape the root if anything let it through.
func (d *Dir) pathFor(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(d.Root, filepath.FromSlash(key)), nil
}

// List returns every object whose key starts with prefix.
//
// The walk starts at the deepest directory the prefix implies rather than at
// the root, so listing one session does not traverse every project.
func (d *Dir) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}

	// The root must exist. Without this check an unplugged drive, an unmounted
	// share or a mistyped path reports an empty store, and "the other device
	// pushed nothing" is how a fast-forward turns into a fork. A directory
	// missing *below* the root is different: it only means nothing has been
	// written under that prefix yet.
	if info, err := os.Stat(d.Root); err != nil {
		return nil, fmt.Errorf("storage directory is unavailable: check that %s is present and readable: %w",
			filepath.Base(d.Root), err)
	} else if !info.IsDir() {
		return nil, errors.New("storage path is not a directory: check the configured location")
	}

	start := d.Root
	if dir := path.Dir(prefix); dir != "." && dir != "/" {
		start = filepath.Join(d.Root, filepath.FromSlash(dir))
	}

	var out []ObjectInfo
	err := filepath.WalkDir(start, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory that disappeared under us is not an error for a
			// listing; anything else is.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		// Only regular files are objects. A symlink is skipped rather than
		// followed: following one could read outside the configured root.
		if !entry.Type().IsRegular() {
			return nil
		}

		// p is always under Root because the walk started there, so the prefix
		// can simply be trimmed. filepath.Rel would only add an error branch
		// that cannot be reached.
		key := filepath.ToSlash(strings.TrimPrefix(p, d.Root+string(filepath.Separator)))
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		// Temporary files belong to a write in progress, not to the store.
		if strings.HasSuffix(key, ".tmp") {
			return nil
		}
		// A listing is another place an external string becomes a key. Ours
		// are always valid, so anything else was put there by something other
		// than us and is not ours to hand upwards.
		if ValidateKey(key) != nil {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size(), ModTime: info.ModTime()})
		return nil
	})

	if err != nil {
		// A missing directory was already turned into "nothing here" by the
		// callback, so anything reaching this point is a real failure and must
		// not be reported as an empty store - that would tell the sync layer
		// another device pushed nothing.
		return nil, fmt.Errorf("list %q: %w", prefix, err)
	}
	return out, nil
}

// Get opens the object for reading.
func (d *Dir) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := d.pathFor(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}

	// Opening a directory succeeds on both platforms, so without this the
	// caller would get a usable handle whose first read fails with something
	// unrecognisable instead of the absence Stat already reports here.
	info, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // already failing; the close cannot help
		return nil, fmt.Errorf("read object: %w", err)
	}
	if info.IsDir() {
		f.Close() //nolint:errcheck // nothing was read, so nothing can be lost
		return nil, fmt.Errorf("%w: %s is a directory", ErrNotFound, key)
	}
	return f, nil
}

// Put writes size bytes from r to key.
//
// The write is atomic, so a reader - including a third-party sync tool - never
// sees a partially written object.
func (d *Dir) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	p, err := d.pathFor(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create object directory: %w", err)
	}

	return atomicfile.Write(p, func(w io.Writer) error {
		written, err := io.Copy(w, r)
		if err != nil {
			return fmt.Errorf("write object: %w", err)
		}
		// A short read means the caller handed us less than it promised. The
		// object would look complete on disk, so refuse rather than publish a
		// shard that silently lost its tail.
		if size >= 0 && written != size {
			return fmt.Errorf("write object: got %d bytes, expected %d", written, size)
		}
		return nil
	})
}

// Delete removes the object. Deleting a key that is not there succeeds, so
// cleanup is idempotent.
func (d *Dir) Delete(ctx context.Context, key string) error {
	p, err := d.pathFor(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// Stat returns metadata without transferring the object.
func (d *Dir) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := d.pathFor(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, err
	}

	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("stat object: %w", err)
	}
	if info.IsDir() {
		// A directory occupying an object's key is not an object. Reporting it
		// as one would let a caller believe a shard exists.
		return ObjectInfo{}, fmt.Errorf("%w: %s is a directory", ErrNotFound, key)
	}
	return ObjectInfo{Key: key, Size: info.Size(), ModTime: info.ModTime()}, nil
}

// Probe verifies the directory can be created, written, read and cleaned up,
// so a misconfiguration surfaces during setup rather than during the first
// sync (§9.1).
// A single segment, so probing never creates a directory it would then have to
// remove.
const probeKey = ".agentsync-probe"

func (d *Dir) Probe(ctx context.Context) (err error) {
	const body = "probe"

	if mkErr := os.MkdirAll(d.Root, 0o755); mkErr != nil {
		return fmt.Errorf("cannot create the storage directory: check the path and its permissions: %w", mkErr)
	}
	if putErr := d.Put(ctx, probeKey, strings.NewReader(body), int64(len(body))); putErr != nil {
		return fmt.Errorf("cannot write to the storage directory: check its permissions and free space: %w", putErr)
	}

	// Once the object exists it must be removed whatever happens next: a probe
	// that leaves its own litter behind has not verified cleanliness, and
	// Prober's contract says it cleans up after itself.
	defer func() {
		if delErr := d.Delete(ctx, probeKey); delErr != nil && err == nil {
			err = fmt.Errorf("cannot delete from the storage directory: check its permissions: %w", delErr)
		}
	}()

	r, getErr := d.Get(ctx, probeKey)
	if getErr != nil {
		return fmt.Errorf("cannot read back from the storage directory: check its permissions: %w", getErr)
	}
	if closeErr := r.Close(); closeErr != nil {
		return fmt.Errorf("cannot read back from the storage directory: %w", closeErr)
	}
	return nil
}
