package main

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func loadMaterializedReplicaBinding(configDir, hubID, projectID, sessionID, replicaID, agent, nativeSessionID string, generation uint64) (*sessionhub.LocalBinding, error) {
	binding, err := sessionhub.LoadLocalBinding(configDir, hubID, projectID, sessionID, replicaID, agent)
	if errors.Is(err, sessionhub.ErrLocalBindingNotFound) {
		transactionID, ok := materializeTransactionIDFromNativeSession(nativeSessionID)
		if !ok {
			return nil, nil
		}
		transaction, transactionErr := sessionhub.LoadMaterializeTransaction(configDir, hubID, projectID, sessionID, transactionID)
		if errors.Is(transactionErr, sessionhub.ErrMaterializeTransactionNotFound) {
			return nil, nil
		}
		if transactionErr != nil {
			return nil, fmt.Errorf("inspect materialize transaction for missing binding: %w", transactionErr)
		}
		if transaction.TargetAgent != agent || transaction.TargetNativeID != nativeSessionID || transaction.ReplicaID != replicaID {
			return nil, fmt.Errorf("%w: materialize transaction target differs from discovered Replica", syncflow.ErrMaterializeBoundaryUnknown)
		}
		return nil, fmt.Errorf("%w: materialized target binding is missing", syncflow.ErrMaterializeBoundaryUnknown)
	}
	if err != nil {
		return nil, fmt.Errorf("load local Replica binding: %w", err)
	}
	if binding.NativeSessionID != nativeSessionID || binding.Generation != generation {
		return nil, errors.New("local Replica binding does not match the discovered NativeSession")
	}
	if binding.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize {
		return nil, nil
	}
	return &binding, nil
}

func materializeTransactionIDFromNativeSession(nativeSessionID string) (string, bool) {
	const prefix = "ctxhop-"
	if !strings.HasPrefix(nativeSessionID, prefix) {
		return "", false
	}
	value := strings.TrimPrefix(nativeSessionID, prefix)
	if len(value) != 64 {
		return "", false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'f') || (r >= '0' && r <= '9') {
			continue
		}
		return "", false
	}
	return value, true
}

func ensureMaterializedReplicaOrigin(ctx context.Context, store remote.Remote, layout syncer.ReplicaLayout, identities []*ecdh.PrivateKey, binding sessionhub.LocalBinding) error {
	descriptor, err := syncer.FetchReplicaDescriptor(ctx, store, layout, identities)
	if errors.Is(err, syncer.ErrReplicaDescriptorMissing) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing materialized Replica descriptor: %w", err)
	}
	if descriptor.Origin.Kind != sessionhub.ReplicaOriginLocalMaterialize || !sameMaterializeStringList(descriptor.Origin.BaseHeads, binding.Origin.BaseHeads) {
		return fmt.Errorf("%w: existing Replica origin predates boundary-aware publication; a new generation is required", syncflow.ErrMaterializeContributionConflict)
	}
	return nil
}

func publishMaterializedReplicaSuffix(ctx context.Context, configDir string, store remote.Remote, public *ecdh.PublicKey, identities []*ecdh.PrivateKey, identifierKey []byte, layout syncer.ReplicaLayout, binding sessionhub.LocalBinding) error {
	snapshot, err := syncer.FetchCompleteReplica(ctx, store, layout, identities)
	if err != nil {
		return fmt.Errorf("verify complete target Replica: %w", err)
	}
	sessionLayout, err := layout.SessionLayout()
	if err != nil {
		return fmt.Errorf("prepare target Session Contribution layout: %w", err)
	}
	existing, err := syncer.FetchSessionContributions(ctx, store, sessionLayout, identities)
	if errors.Is(err, syncer.ErrNoContributions) {
		existing = nil
	} else if err != nil {
		return fmt.Errorf("read target Session Contributions: %w", err)
	}
	existing, err = includeMaterializedCursorContribution(ctx, store, sessionLayout, layout.DeviceID(), identities, binding, existing)
	if err != nil {
		return err
	}
	plan, err := syncflow.PlanMaterializedSuffix(syncflow.MaterializedSuffixRequest{
		Binding:               binding,
		Snapshot:              snapshot,
		ExistingContributions: existing,
		IdentifierKey:         identifierKey,
	})
	if err != nil {
		return err
	}
	if plan.Contribution != nil {
		if err := syncer.PutContributionWithIdentities(ctx, store, public, sessionLayout, *plan.Contribution, identities); err != nil {
			return fmt.Errorf("publish target Contribution: %w", err)
		}
	}
	if err := sessionhub.SaveLocalBinding(configDir, plan.Binding); err != nil {
		return fmt.Errorf("commit materialized target cursors: %w", err)
	}
	return nil
}

func includeMaterializedCursorContribution(ctx context.Context, store remote.Remote, layout syncer.SessionHubLayout, deviceID string, identities []*ecdh.PrivateKey, binding sessionhub.LocalBinding, existing []sessionhub.Contribution) ([]sessionhub.Contribution, error) {
	if binding.ContributionCursor.LastContributionID == "" {
		return existing, nil
	}
	for _, contribution := range existing {
		if contribution.ContributionID == binding.ContributionCursor.LastContributionID {
			return existing, nil
		}
	}
	contribution, err := syncer.FetchContribution(ctx, store, layout, binding.ContributionCursor.LastContributionID, deviceID, identities)
	if err != nil {
		return nil, fmt.Errorf("read last materialized Contribution directly: %w", err)
	}
	return append(existing, contribution), nil
}
