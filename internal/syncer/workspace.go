package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/remote"
	workspacepkg "github.com/CCCCY-ci/agentsync/internal/workspace"
)

const maxEncryptedWorkspaceBytes = workspacepkg.MaxSnapshotBytes + 1024

var (
	ErrRemoteWorkspaceTooLarge = errors.New("syncer: remote workspace snapshot is too large")
	ErrInvalidWorkspaceObject  = errors.New("syncer: invalid workspace object")
)

// SealWorkspace serializes, compresses and encrypts a workspace snapshot for
// its exact device branch object key.
func SealWorkspace(recipient *ecdh.PublicKey, objectKey string, snapshot workspacepkg.Snapshot) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal workspace: %w", err)
	}
	plaintext, err := snapshot.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkspaceObject, err)
	}
	compressed, err := compressPayload(plaintext, workspacepkg.MaxSnapshotBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress workspace: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt workspace: %w", err)
	}
	return sealed, nil
}

// OpenWorkspace decrypts, decompresses and validates a remote workspace
// snapshot.
func OpenWorkspace(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (workspacepkg.Snapshot, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: open workspace: %w", err)
	}
	if len(sealed) > maxEncryptedWorkspaceBytes {
		return workspacepkg.Snapshot{}, ErrRemoteWorkspaceTooLarge
	}
	compressed, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: decrypt workspace: %w", err)
	}
	plaintext, err := decompressPayload(compressed, workspacepkg.MaxSnapshotBytes)
	if err != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: decompress workspace: %w", err)
	}
	snapshot, err := workspacepkg.ParseSnapshot(plaintext)
	if err != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: parse workspace: %w", err)
	}
	return snapshot, nil
}

// PutWorkspaceSnapshot publishes the current device's workspace snapshot.
func PutWorkspaceSnapshot(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, snapshot workspacepkg.Snapshot) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	key, err := layout.WorkspaceKey()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish workspace: %w", err)
	}
	sealed, err := SealWorkspace(recipient, key, snapshot)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish workspace: %w", err)
	}
	return nil
}

// ReadWorkspaceSnapshot reads the current workspace snapshot for one device
// branch. It is intentionally not part of metadata-only listing.
func ReadWorkspaceSnapshot(ctx context.Context, store remote.Remote, layout ObjectLayout, identities []*ecdh.PrivateKey) (workspacepkg.Snapshot, error) {
	key, err := layout.WorkspaceKey()
	if err != nil {
		return workspacepkg.Snapshot{}, err
	}
	if ctx == nil {
		return workspacepkg.Snapshot{}, errors.New("syncer: context is required")
	}
	if store == nil {
		return workspacepkg.Snapshot{}, errors.New("syncer: remote store is required")
	}
	if err := validateIdentities(identities); err != nil {
		return workspacepkg.Snapshot{}, err
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return workspacepkg.Snapshot{}, err
	}
	sealed, readErr := io.ReadAll(io.LimitReader(reader, maxEncryptedWorkspaceBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: read workspace: %w", readErr)
	}
	if closeErr != nil {
		return workspacepkg.Snapshot{}, fmt.Errorf("syncer: close workspace: %w", closeErr)
	}
	if len(sealed) > maxEncryptedWorkspaceBytes {
		return workspacepkg.Snapshot{}, ErrRemoteWorkspaceTooLarge
	}
	var lastErr error
	for _, identity := range identities {
		snapshot, openErr := OpenWorkspace(identity, key, sealed)
		if openErr == nil {
			return snapshot, nil
		}
		lastErr = openErr
	}
	return workspacepkg.Snapshot{}, fmt.Errorf("syncer: open workspace: %w", lastErr)
}
