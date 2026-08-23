package syncflow

import (
	"context"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// LoadObservedTips adapts the syncer pull-tip state to PullOptions. The state
// contains only opaque remote progress and is safe to keep in local state.
func LoadObservedTips(ctx context.Context, store syncer.PullTipStore) ([]RemoteTip, error) {
	tips, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	observed := make([]RemoteTip, len(tips))
	for i, tip := range tips {
		observed[i] = RemoteTip{
			DeviceID:    tip.DeviceID,
			RecordCount: tip.RecordCount,
			HeadDigest:  tip.HeadDigest,
		}
	}
	return observed, nil
}

// SaveObservedTips persists the opaque tips returned by an explicit metadata
// check. It does not write any session records or remote objects.
func SaveObservedTips(ctx context.Context, store syncer.PullTipStore, tips []RemoteTip) error {
	converted := make([]syncer.PullTip, len(tips))
	for i, tip := range tips {
		validated, err := syncer.NewPullTip(tip.DeviceID, tip.RecordCount, tip.HeadDigest)
		if err != nil {
			return err
		}
		converted[i] = validated
	}
	return store.Save(ctx, converted)
}
