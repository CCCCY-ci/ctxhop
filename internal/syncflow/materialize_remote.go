package syncflow

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

var (
	// ErrMaterializeRemoteRead classifies failures while reading the
	// encrypted Contribution/Replica source snapshot. It is never returned for
	// target encoding failures, which remain in the pure planner's errors.
	ErrMaterializeRemoteRead = errors.New("syncflow: materialize remote read failed")

	// ErrMaterializeRemoteSourceConflict reports a selected graph that assigns
	// one Replica ID to more than one source identity. Fetching such a map by
	// Replica ID would be ambiguous and is therefore rejected before body read.
	ErrMaterializeRemoteSourceConflict = errors.New("syncflow: materialize source Replica identity conflicts")
)

// RemoteMaterializePreviewRequest describes a read-only cross-Agent preview
// sourced from one logical Session Hub Session. The request embeds the pure
// MaterializePreviewOptions so source/target adapter capabilities remain local
// and are never sent to the Remote.
type RemoteMaterializePreviewRequest struct {
	Store      remote.Remote
	Identities []*ecdh.PrivateKey
	Layout     syncer.SessionHubLayout
	Heads      []string
	MaterializePreviewOptions
}

// FetchMaterializePreview reads an authenticated Contribution graph, fetches
// only the complete Replicas referenced by the selected ancestry, and then
// delegates to the pure local materialization planner. It performs no local
// file writes, no Agent invocation, no environment mutation, and no Remote
// writes. Source records and Contribution envelopes remain unchanged.
func FetchMaterializePreview(ctx context.Context, request RemoteMaterializePreviewRequest) (MaterializePreview, error) {
	if ctx == nil {
		return MaterializePreview{}, fmt.Errorf("%w: context is required", ErrMaterializeRemoteRead)
	}
	if request.Store == nil {
		return MaterializePreview{}, fmt.Errorf("%w: remote store is required", ErrMaterializeRemoteRead)
	}
	if err := ctx.Err(); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializeRemoteRead, err)
	}
	if _, err := request.Layout.SessionPrefix(); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: Session layout: %w", ErrMaterializeRemoteRead, err)
	}

	graph, err := syncer.FetchContributionGraph(ctx, request.Store, request.Layout, request.Identities)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: Contribution graph: %w", ErrMaterializeRemoteRead, err)
	}
	coverage, err := graph.Select(request.Heads...)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: select heads: %w", ErrMaterializeRemoteRead, err)
	}
	if err := validateRemoteSourceCapabilities(graph, coverage.SelectedIDs, request.SourceCapabilities); err != nil {
		return MaterializePreview{}, err
	}

	replicaLayouts, err := selectedReplicaLayouts(request.Layout, graph, coverage.SelectedIDs)
	if err != nil {
		return MaterializePreview{}, err
	}
	replicas, err := fetchSelectedReplicas(ctx, request.Store, request.Identities, replicaLayouts)
	if err != nil {
		return MaterializePreview{}, err
	}
	selection, err := PlanMaterializeSelection(graph, request.Heads, replicas)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: source selection: %w", ErrMaterializeRemoteRead, err)
	}
	preview, err := PlanMaterializePreview(ctx, selection, request.MaterializePreviewOptions)
	if err != nil {
		return MaterializePreview{}, err
	}
	return preview, nil
}

func validateRemoteSourceCapabilities(graph *sessionhub.Graph, contributionIDs []string, capabilities map[string]adapter.MaterializeCapability) error {
	for _, contributionID := range contributionIDs {
		contribution, ok := graph.Contribution(contributionID)
		if !ok {
			return fmt.Errorf("%w: selected Contribution %q is unavailable", ErrMaterializeRemoteRead, contributionID)
		}
		if capability, exists := capabilities[contribution.Source.Agent]; !exists || capability == nil {
			return fmt.Errorf("%w: Agent %q", ErrMaterializeSourceCapabilityMissing, contribution.Source.Agent)
		}
	}
	return nil
}

func selectedReplicaLayouts(layout syncer.SessionHubLayout, graph *sessionhub.Graph, contributionIDs []string) (map[string]syncer.ReplicaLayout, error) {
	layouts := make(map[string]syncer.ReplicaLayout)
	sources := make(map[string]sessionhub.ContributionSource)
	for _, contributionID := range contributionIDs {
		contribution, ok := graph.Contribution(contributionID)
		if !ok {
			return nil, fmt.Errorf("%w: selected Contribution %q is unavailable", ErrMaterializeRemoteRead, contributionID)
		}
		source := contribution.Source
		if previous, exists := sources[source.ReplicaID]; exists && previous != source {
			return nil, fmt.Errorf("%w: Replica %q is referenced by multiple source identities", ErrMaterializeRemoteSourceConflict, source.ReplicaID)
		}
		sources[source.ReplicaID] = source
		if _, exists := layouts[source.ReplicaID]; exists {
			continue
		}
		replicaLayout, err := layout.Replica(source.ReplicaID, source.DeviceID)
		if err != nil {
			return nil, fmt.Errorf("%w: Replica %q: %w", ErrMaterializeRemoteRead, source.ReplicaID, err)
		}
		layouts[source.ReplicaID] = replicaLayout
	}
	return layouts, nil
}

func fetchSelectedReplicas(ctx context.Context, store remote.Remote, identities []*ecdh.PrivateKey, layouts map[string]syncer.ReplicaLayout) (map[string]syncer.ReplicaSnapshot, error) {
	ids := make([]string, 0, len(layouts))
	for replicaID := range layouts {
		ids = append(ids, replicaID)
	}
	sort.Strings(ids)
	replicas := make(map[string]syncer.ReplicaSnapshot, len(ids))
	for _, replicaID := range ids {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMaterializeRemoteRead, err)
		}
		snapshot, err := syncer.FetchCompleteReplica(ctx, store, layouts[replicaID], identities)
		if err != nil {
			return nil, fmt.Errorf("%w: Replica %q: %w", ErrMaterializeRemoteRead, replicaID, err)
		}
		replicas[replicaID] = snapshot
	}
	return replicas, nil
}
