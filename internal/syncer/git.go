package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"

	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/gitstate"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const (
	maxEncryptedGitStateBytes    = gitstate.MaxStateBytes + 1024
	maxGitTransferPlainBytes     = gitstate.MaxTransferBytes + gitstate.MaxTransferBytes/2
	maxEncryptedGitTransferBytes = maxGitTransferPlainBytes + 1024
)

var (
	ErrRemoteGitStateTooLarge    = errors.New("syncer: remote Git state is too large")
	ErrRemoteGitTransferTooLarge = errors.New("syncer: remote Git transfer is too large")
)

func SealGitState(recipient *ecdh.PublicKey, objectKey string, state gitstate.State) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal Git state: %w", err)
	}
	plaintext, err := state.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("syncer: encode Git state: %w", err)
	}
	compressed, err := compressPayload(plaintext, gitstate.MaxStateBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress Git state: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt Git state: %w", err)
	}
	return sealed, nil
}

func OpenGitState(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (gitstate.State, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return gitstate.State{}, fmt.Errorf("syncer: open Git state: %w", err)
	}
	if len(sealed) > maxEncryptedGitStateBytes {
		return gitstate.State{}, ErrRemoteGitStateTooLarge
	}
	compressed, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return gitstate.State{}, fmt.Errorf("syncer: decrypt Git state: %w", err)
	}
	plaintext, err := decompressPayload(compressed, gitstate.MaxStateBytes)
	if err != nil {
		return gitstate.State{}, fmt.Errorf("syncer: decompress Git state: %w", err)
	}
	state, err := gitstate.ParseState(plaintext)
	if err != nil {
		return gitstate.State{}, fmt.Errorf("syncer: parse Git state: %w", err)
	}
	return state, nil
}

func SealGitTransfer(recipient *ecdh.PublicKey, objectKey string, transfer gitstate.Transfer) ([]byte, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return nil, fmt.Errorf("syncer: seal Git transfer: %w", err)
	}
	plaintext, err := transfer.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("syncer: encode Git transfer: %w", err)
	}
	compressed, err := compressPayload(plaintext, maxGitTransferPlainBytes)
	if err != nil {
		return nil, fmt.Errorf("syncer: compress Git transfer: %w", err)
	}
	sealed, err := crypto.Encrypt(recipient, objectKey, compressed)
	if err != nil {
		return nil, fmt.Errorf("syncer: encrypt Git transfer: %w", err)
	}
	return sealed, nil
}

func OpenGitTransfer(identity *ecdh.PrivateKey, objectKey string, sealed []byte) (gitstate.Transfer, error) {
	if err := remote.ValidateKey(objectKey); err != nil {
		return gitstate.Transfer{}, fmt.Errorf("syncer: open Git transfer: %w", err)
	}
	if len(sealed) > maxEncryptedGitTransferBytes {
		return gitstate.Transfer{}, ErrRemoteGitTransferTooLarge
	}
	compressed, err := crypto.Decrypt(identity, objectKey, sealed)
	if err != nil {
		return gitstate.Transfer{}, fmt.Errorf("syncer: decrypt Git transfer: %w", err)
	}
	plaintext, err := decompressPayload(compressed, maxGitTransferPlainBytes)
	if err != nil {
		return gitstate.Transfer{}, fmt.Errorf("syncer: decompress Git transfer: %w", err)
	}
	transfer, err := gitstate.ParseTransfer(plaintext)
	if err != nil {
		return gitstate.Transfer{}, fmt.Errorf("syncer: parse Git transfer: %w", err)
	}
	return transfer, nil
}

func PutGitState(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, state gitstate.State) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	key, err := layout.GitStateKey()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish Git state: %w", err)
	}
	sealed, err := SealGitState(recipient, key, state)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish Git state: %w", err)
	}
	return nil
}

func PutGitTransfer(ctx context.Context, store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, transfer gitstate.Transfer) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if store == nil {
		return errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return errors.New("syncer: recipient key is required")
	}
	key, err := layout.GitTransferKey()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("syncer: publish Git transfer: %w", err)
	}
	sealed, err := SealGitTransfer(recipient, key, transfer)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("syncer: publish Git transfer: %w", err)
	}
	return nil
}

func ReadGitState(ctx context.Context, store remote.Remote, layout ObjectLayout, identities []*ecdh.PrivateKey) (gitstate.State, error) {
	key, err := layout.GitStateKey()
	if err != nil {
		return gitstate.State{}, err
	}
	sealed, err := readGitObject(ctx, store, key, maxEncryptedGitStateBytes)
	if err != nil {
		return gitstate.State{}, err
	}
	var lastErr error
	for _, identity := range identities {
		state, openErr := OpenGitState(identity, key, sealed)
		if openErr == nil {
			return state, nil
		}
		lastErr = openErr
	}
	if lastErr == nil {
		return gitstate.State{}, errors.New("syncer: no encryption identity was provided")
	}
	return gitstate.State{}, fmt.Errorf("syncer: open Git state: %w", lastErr)
}

func ReadGitTransfer(ctx context.Context, store remote.Remote, layout ObjectLayout, identities []*ecdh.PrivateKey) (gitstate.Transfer, error) {
	key, err := layout.GitTransferKey()
	if err != nil {
		return gitstate.Transfer{}, err
	}
	sealed, err := readGitObject(ctx, store, key, maxEncryptedGitTransferBytes)
	if err != nil {
		return gitstate.Transfer{}, err
	}
	var lastErr error
	for _, identity := range identities {
		transfer, openErr := OpenGitTransfer(identity, key, sealed)
		if openErr == nil {
			return transfer, nil
		}
		lastErr = openErr
	}
	if lastErr == nil {
		return gitstate.Transfer{}, errors.New("syncer: no encryption identity was provided")
	}
	return gitstate.Transfer{}, fmt.Errorf("syncer: open Git transfer: %w", lastErr)
}

func readGitObject(ctx context.Context, store remote.Remote, key string, maxBytes int64) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("syncer: context is required")
	}
	if store == nil {
		return nil, errors.New("syncer: remote store is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("syncer: read Git object: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("syncer: close Git object: %w", closeErr)
	}
	if int64(len(data)) > maxBytes {
		if maxBytes == maxEncryptedGitStateBytes {
			return nil, ErrRemoteGitStateTooLarge
		}
		return nil, ErrRemoteGitTransferTooLarge
	}
	return data, nil
}
