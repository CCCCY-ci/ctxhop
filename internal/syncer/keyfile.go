package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
)

const maxKeyfileBytes = 1 << 20

var (
	// ErrNoRemoteKeyfile reports a backend that has not been initialised for
	// CtxHop yet.
	ErrNoRemoteKeyfile = errors.New("syncer: remote keyfile is not present")

	// ErrRemoteKeyfileExists prevents a later init from replacing the envelope
	// that protects every session already stored in the backend.
	ErrRemoteKeyfileExists = errors.New("syncer: remote keyfile already exists")

	// ErrRemoteKeyfileTooLarge reports an object that is not a plausible
	// keyfile before it is parsed or allocated without a bound.
	ErrRemoteKeyfileTooLarge = errors.New("syncer: remote keyfile is too large")
)

// PublishKeyfile creates the remote keyfile without replacing an existing
// envelope. The keyfile is the only object whose replacement can make all
// existing encrypted sessions unreadable, so a caller must explicitly choose
// a future key-rotation operation rather than getting overwrite semantics from
// the generic Remote.Put method.
func PublishKeyfile(ctx context.Context, store remote.Remote, keyfile *crypto.Keyfile) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	data, err := crypto.MarshalKeyfile(keyfile)
	if err != nil {
		return fmt.Errorf("syncer: encode keyfile: %w", err)
	}
	if len(data) > maxKeyfileBytes {
		return fmt.Errorf("%w: maximum is %d bytes", ErrRemoteKeyfileTooLarge, maxKeyfileBytes)
	}

	_, err = store.Stat(ctx, crypto.KeyfilePath())
	switch {
	case err == nil:
		return ErrRemoteKeyfileExists
	case errors.Is(err, remote.ErrNotFound):
		// The object is absent. Put below is the one-way publication step.
	default:
		return fmt.Errorf("syncer: check remote keyfile: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish keyfile: %w", err)
	}
	if err := store.Put(ctx, crypto.KeyfilePath(), bytesReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("syncer: publish keyfile: %w", err)
	}
	return nil
}

// ReplaceKeyfile updates an already initialised remote keyfile.
//
// Replacing the envelope is an explicit lifecycle operation: the caller must
// fetch and validate the current keyfile first. Refusing to create a missing
// keyfile prevents a mistyped backend from looking like a successful password
// rotation that created a second, empty storage.
func ReplaceKeyfile(ctx context.Context, store remote.Remote, keyfile *crypto.Keyfile) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	data, err := crypto.MarshalKeyfile(keyfile)
	if err != nil {
		return fmt.Errorf("syncer: encode keyfile: %w", err)
	}
	if len(data) > maxKeyfileBytes {
		return fmt.Errorf("%w: maximum is %d bytes", ErrRemoteKeyfileTooLarge, maxKeyfileBytes)
	}
	if _, err := store.Stat(ctx, crypto.KeyfilePath()); err != nil {
		if errors.Is(err, remote.ErrNotFound) {
			return ErrNoRemoteKeyfile
		}
		return fmt.Errorf("syncer: check remote keyfile before replacement: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: replace keyfile: %w", err)
	}
	if err := store.Put(ctx, crypto.KeyfilePath(), bytesReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("syncer: replace keyfile: %w", err)
	}
	return nil
}

// FetchKeyfile reads and parses the bounded remote envelope. It never unlocks
// the keyfile: passphrases and recovery keys belong to the CLI interaction
// layer, not to remote storage plumbing.
func FetchKeyfile(ctx context.Context, store remote.Remote) (*crypto.Keyfile, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	reader, err := store.Get(ctx, crypto.KeyfilePath())
	if err != nil {
		if errors.Is(err, remote.ErrNotFound) {
			return nil, ErrNoRemoteKeyfile
		}
		return nil, fmt.Errorf("syncer: read remote keyfile: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxKeyfileBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		if closeErr != nil {
			return nil, fmt.Errorf("syncer: read remote keyfile: %w (also close: %v)", readErr, closeErr)
		}
		return nil, fmt.Errorf("syncer: read remote keyfile: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("syncer: close remote keyfile: %w", closeErr)
	}
	if len(data) > maxKeyfileBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", ErrRemoteKeyfileTooLarge, maxKeyfileBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("syncer: read remote keyfile: %w", err)
	}
	keyfile, err := crypto.ParseKeyfile(data)
	if err != nil {
		return nil, fmt.Errorf("syncer: parse remote keyfile: %w", err)
	}
	return keyfile, nil
}

// bytesReader is kept local so callers cannot accidentally pass a mutable
// buffer to a backend after publication begins.
func bytesReader(data []byte) io.Reader {
	return &immutableReader{data: append([]byte(nil), data...)}
}

type immutableReader struct {
	data []byte
	read int
}

func (r *immutableReader) Read(p []byte) (int, error) {
	if r.read == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.read:])
	r.read += n
	return n, nil
}
