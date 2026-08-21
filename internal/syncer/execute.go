package syncer

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"

	"github.com/CCCCY-ci/agentsync/internal/remote"
)

var (
	// ErrCursorCommit reports a remote shard that succeeded but whose local
	// cursor could not be persisted.
	ErrCursorCommit = errors.New("syncer: cursor commit failed")

	// ErrExecutorLayoutMismatch reports a cursor store for a different remote
	// identity tuple than the executor's object layout.
	ErrExecutorLayoutMismatch = errors.New("syncer: executor layout does not match cursor store")
)

// AppendExecutor publishes planned append shards and commits their cursor one
// step at a time. Its fields are private so a constructed executor cannot be
// changed into one that writes a different namespace without validation.
type AppendExecutor struct {
	store     remote.Remote
	recipient *ecdh.PublicKey
	layout    ObjectLayout
	state     CursorStore
	options   PlanOptions
}

// NewAppendExecutor validates the dependencies for durable append execution.
func NewAppendExecutor(store remote.Remote, recipient *ecdh.PublicKey, layout ObjectLayout, state CursorStore, options PlanOptions) (AppendExecutor, error) {
	if store == nil {
		return AppendExecutor{}, errors.New("syncer: remote store is required")
	}
	if recipient == nil {
		return AppendExecutor{}, errors.New("syncer: recipient key is required")
	}
	if err := layout.validate(); err != nil {
		return AppendExecutor{}, err
	}
	if err := options.validate(); err != nil {
		return AppendExecutor{}, err
	}
	if state.layout != layout {
		return AppendExecutor{}, ErrExecutorLayoutMismatch
	}
	if _, err := state.filePath(); err != nil {
		return AppendExecutor{}, err
	}

	return AppendExecutor{
		store:     store,
		recipient: recipient,
		layout:    layout,
		state:     state,
		options:   options,
	}, nil
}

// Execute plans and publishes the suffix after cursor.
//
// The cursor returned after an execution error is the last cursor known to be
// durable in the local state file once remote execution has started. If the
// error occurs during validation or planning, the returned cursor is zero.
func (e AppendExecutor) Execute(ctx context.Context, cursor PushCursor, records [][]byte) (PushCursor, error) {
	if ctx == nil {
		return PushCursor{}, errors.New("syncer: context is required")
	}
	if err := ctx.Err(); err != nil {
		return PushCursor{}, fmt.Errorf("syncer: execute append: %w", err)
	}

	prefixEnd, err := prepareAppend(cursor, records, e.options)
	if err != nil {
		return PushCursor{}, err
	}

	// Preflight every boundary before the first remote write. This preserves
	// the previous all-or-nothing planning guarantee without retaining cloned
	// records for every shard at once.
	sequence := cursor.NextShard
	base := cursor.RecordCount
	for start := prefixEnd; start < len(records); {
		if err := ctx.Err(); err != nil {
			return PushCursor{}, fmt.Errorf("syncer: execute append: %w", err)
		}
		if sequence == 0 || sequence > maxShardNumber {
			return PushCursor{}, ErrShardSequenceExhausted
		}
		end, err := planShardEnd(base, records, start, e.options)
		if err != nil {
			return PushCursor{}, err
		}
		base += uint64(end - start)
		sequence++
		start = end
	}

	durable := cursor
	for start := prefixEnd; start < len(records); {
		if err := ctx.Err(); err != nil {
			return durable, fmt.Errorf("syncer: execute append: %w", err)
		}
		end, shard, err := planShard(durable.RecordCount, durable.HeadDigest, records, start, e.options)
		if err != nil {
			return durable, err
		}
		part := ShardPart{Number: durable.NextShard, Shard: shard}

		next, err := PutShard(ctx, e.store, e.recipient, e.layout, durable, part)
		if err != nil {
			return durable, fmt.Errorf("syncer: publish shard %d: %w", part.Number, err)
		}

		if err := e.state.Save(ctx, next); err != nil {
			commitErr := errors.Join(ErrCursorCommit, err)
			return durable, fmt.Errorf("syncer: save cursor after shard %d: %w", part.Number, commitErr)
		}
		durable = next
		start = end
	}

	return durable, nil
}
