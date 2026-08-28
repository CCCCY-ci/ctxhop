package syncflow

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sort"
	"strings"

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

	// ErrMaterializeHeadSelection reports a context policy that cannot be
	// resolved against the authenticated Contribution snapshot.
	ErrMaterializeHeadSelection = errors.New("syncflow: materialize head selection is invalid")
)

// MaterializeContextPolicy identifies how a CLI or UI chooses the source
// Contribution heads for a read-only materialization preview.
type MaterializeContextPolicy string

const (
	MaterializeContextCausalHead MaterializeContextPolicy = "causal-head"
	MaterializeContextAllHeads   MaterializeContextPolicy = "all-heads"
	MaterializeContextAgentOnly  MaterializeContextPolicy = "agent-only"
)

// RemoteMaterializePreviewRequest describes a read-only cross-Agent preview
// sourced from one logical Session Hub Session. The request embeds the pure
// MaterializePreviewOptions so source/target adapter capabilities remain local
// and are never sent to the Remote.
type RemoteMaterializePreviewRequest struct {
	Store      remote.Remote
	Identities []*ecdh.PrivateKey
	Layout     syncer.SessionHubLayout
	// ContextPolicy is optional for backwards compatibility. An empty policy
	// uses causal-head semantics, with an explicit head preferred and a single
	// current head selected automatically.
	ContextPolicy MaterializeContextPolicy
	// SourceAgent is required by the agent-only policy and is otherwise empty.
	SourceAgent string
	Heads       []string
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
	heads, err := ResolveMaterializeHeads(graph, request.ContextPolicy, request.Heads, request.SourceAgent)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializeRemoteRead, err)
	}
	coverage, err := graph.Select(heads...)
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
	selection, err := PlanMaterializeSelection(graph, heads, replicas)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: source selection: %w", ErrMaterializeRemoteRead, err)
	}
	preview, err := PlanMaterializePreview(ctx, selection, request.MaterializePreviewOptions)
	if err != nil {
		return MaterializePreview{}, err
	}
	return preview, nil
}

// ResolveMaterializeHeads turns a user-facing context policy into the
// explicit head set consumed by the pure graph planner. It is deterministic
// and read-only; it never infers a head from timestamps or from record
// contents.
func ResolveMaterializeHeads(graph *sessionhub.Graph, policy MaterializeContextPolicy, requested []string, sourceAgent string) ([]string, error) {
	if graph == nil {
		return nil, fmt.Errorf("%w: graph is nil", ErrMaterializeHeadSelection)
	}
	if policy == "" {
		policy = MaterializeContextCausalHead
	}
	switch policy {
	case MaterializeContextCausalHead:
		if strings.TrimSpace(sourceAgent) != "" {
			return nil, fmt.Errorf("%w: source Agent is only valid with agent-only", ErrMaterializeHeadSelection)
		}
		if len(requested) == 0 {
			current := graph.Heads()
			if len(current) != 1 {
				return nil, fmt.Errorf("%w: causal-head requires one --head when the Session has %d current heads", ErrMaterializeHeadSelection, len(current))
			}
			return current, nil
		}
		if len(requested) != 1 {
			return nil, fmt.Errorf("%w: causal-head accepts exactly one --head", ErrMaterializeHeadSelection)
		}
		return append([]string(nil), requested...), nil

	case MaterializeContextAllHeads:
		if len(requested) != 0 {
			return nil, fmt.Errorf("%w: all-heads does not accept --head", ErrMaterializeHeadSelection)
		}
		if strings.TrimSpace(sourceAgent) != "" {
			return nil, fmt.Errorf("%w: source Agent is only valid with agent-only", ErrMaterializeHeadSelection)
		}
		heads := graph.Heads()
		if len(heads) == 0 {
			return nil, fmt.Errorf("%w: Session has no current heads", ErrMaterializeHeadSelection)
		}
		return heads, nil

	case MaterializeContextAgentOnly:
		if len(requested) != 0 {
			return nil, fmt.Errorf("%w: agent-only does not accept --head", ErrMaterializeHeadSelection)
		}
		if err := validateMaterializeAgent(sourceAgent, "source"); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMaterializeHeadSelection, err)
		}
		selected := make([]string, 0)
		for _, id := range graph.ContributionIDs() {
			contribution, ok := graph.Contribution(id)
			if ok && contribution.Source.Agent == sourceAgent {
				selected = append(selected, id)
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("%w: no Contribution from source Agent %q", ErrMaterializeHeadSelection, sourceAgent)
		}
		return selected, nil

	default:
		return nil, fmt.Errorf("%w: unsupported context policy %q", ErrMaterializeHeadSelection, policy)
	}
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
