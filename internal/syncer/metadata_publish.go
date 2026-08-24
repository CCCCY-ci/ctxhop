package syncer

import (
	"context"
	"errors"
	"fmt"
)

// PublishMetadata publishes the metadata tip represented by a durable cursor.
// It is intentionally separate from Execute: shard and cursor durability are
// established first, so a metadata retry never republishes immutable shards.
func (e AppendExecutor) PublishMetadata(ctx context.Context, cursor PushCursor, payload []byte) error {
	if ctx == nil {
		return errors.New("syncer: context is required")
	}
	if err := cursor.Validate(); err != nil {
		return err
	}
	metadata, err := NewMetadata(cursor.RecordCount, cursor.HeadDigest, payload)
	if err != nil {
		return fmt.Errorf("syncer: prepare session metadata: %w", err)
	}
	if err := PutMetadata(ctx, e.store, e.recipient, e.layout, metadata); err != nil {
		return fmt.Errorf("syncer: publish session metadata: %w", err)
	}
	return nil
}
