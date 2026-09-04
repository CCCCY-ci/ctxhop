package syncflow

import (
	"context"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/environment"
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
	// IdentifierKey is required only when IncludeEnvironment is true. It is
	// used to derive the opaque Hub-scoped component keys and never leaves the
	// process.
	IdentifierKey []byte
	Layout        syncer.SessionHubLayout
	// ContextPolicy is optional for backwards compatibility. An empty policy
	// uses causal-head semantics, with an explicit head preferred and a single
	// current head selected automatically.
	ContextPolicy MaterializeContextPolicy
	// SourceAgent is required by the agent-only policy and is otherwise empty.
	SourceAgent string
	Heads       []string
	// IncludeEnvironment asks the read phase to fetch complete filtered
	// environment attachments and component bodies for the selected ancestry.
	// The default remains false so a normal context preview does not download
	// environment data it cannot apply.
	IncludeEnvironment bool
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

	first, err := readMaterializeRemoteSnapshot(ctx, request)
	if err != nil {
		return MaterializePreview{}, err
	}
	selection, err := PlanMaterializeSelection(first.graph, first.heads, first.replicas)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: source selection: %w", ErrMaterializeRemoteRead, err)
	}
	preview, err := PlanMaterializePreview(ctx, selection, request.MaterializePreviewOptions)
	if err != nil {
		return MaterializePreview{}, err
	}
	if first.environmentContents != nil {
		preview.EnvironmentContents = cloneEnvironmentContents(first.environmentContents)
		preview.EnvironmentComponents = environment.ComponentSummaries(first.environmentContents)
	}

	// A Remote listing is not a transaction. Re-read the authenticated graph
	// and every selected Replica after planning so an append that lands while
	// adapters are decoding records cannot be mistaken for a complete plan.
	second, err := readMaterializeRemoteSnapshot(ctx, request)
	if err != nil {
		return MaterializePreview{}, err
	}
	if first.digest != second.digest {
		return MaterializePreview{}, fmt.Errorf("%w: source graph or Replica body changed while planning", ErrMaterializeSnapshotChanged)
	}
	preview.SourceSnapshotDigest = hex.EncodeToString(first.digest[:])
	if err := preview.Validate(); err != nil {
		return MaterializePreview{}, err
	}
	return preview, nil
}

type materializeRemoteSnapshot struct {
	graph               *sessionhub.Graph
	heads               []string
	coverage            sessionhub.Coverage
	replicas            map[string]syncer.ReplicaSnapshot
	environmentContents []environment.ComponentContent
	digest              [32]byte
}

func readMaterializeRemoteSnapshot(ctx context.Context, request RemoteMaterializePreviewRequest) (materializeRemoteSnapshot, error) {
	graph, err := syncer.FetchContributionGraph(ctx, request.Store, request.Layout, request.Identities)
	if err != nil {
		return materializeRemoteSnapshot{}, fmt.Errorf("%w: Contribution graph: %w", ErrMaterializeRemoteRead, err)
	}
	heads, err := ResolveMaterializeHeads(graph, request.ContextPolicy, request.Heads, request.SourceAgent)
	if err != nil {
		return materializeRemoteSnapshot{}, fmt.Errorf("%w: %w", ErrMaterializeRemoteRead, err)
	}
	coverage, err := graph.Select(heads...)
	if err != nil {
		return materializeRemoteSnapshot{}, fmt.Errorf("%w: select heads: %w", ErrMaterializeRemoteRead, err)
	}
	if err := validateRemoteSourceCapabilities(graph, coverage.SelectedIDs, request.SourceCapabilities); err != nil {
		return materializeRemoteSnapshot{}, err
	}

	replicaLayouts, err := selectedReplicaLayouts(request.Layout, graph, coverage.SelectedIDs)
	if err != nil {
		return materializeRemoteSnapshot{}, err
	}
	replicas, err := fetchSelectedReplicas(ctx, request.Store, request.Identities, replicaLayouts)
	if err != nil {
		return materializeRemoteSnapshot{}, err
	}
	var environmentContents []environment.ComponentContent
	if request.IncludeEnvironment {
		environmentContents, err = fetchMaterializeEnvironment(ctx, request, graph, coverage)
		if err != nil {
			return materializeRemoteSnapshot{}, err
		}
	}
	digest, err := digestMaterializeRemoteSnapshot(graph, heads, replicas, environmentContents)
	if err != nil {
		return materializeRemoteSnapshot{}, fmt.Errorf("%w: digest source snapshot: %w", ErrMaterializeRemoteRead, err)
	}
	return materializeRemoteSnapshot{
		graph:               graph,
		heads:               append([]string(nil), heads...),
		coverage:            coverage,
		replicas:            replicas,
		environmentContents: cloneEnvironmentContents(environmentContents),
		digest:              digest,
	}, nil
}

func fetchMaterializeEnvironment(ctx context.Context, request RemoteMaterializePreviewRequest, graph *sessionhub.Graph, coverage sessionhub.Coverage) ([]environment.ComponentContent, error) {
	if len(request.IdentifierKey) == 0 {
		return nil, fmt.Errorf("%w: environment identity key is required", ErrMaterializeRemoteRead)
	}
	environmentLayout, err := syncer.NewEnvironmentHubLayout(request.Layout.HubKey())
	if err != nil {
		return nil, fmt.Errorf("%w: environment layout: %w", ErrMaterializeRemoteRead, err)
	}
	contents := make([]environment.ComponentContent, 0)
	seen := make(map[string]string)
	for _, contributionID := range coverage.SelectedIDs {
		contribution, ok := graph.Contribution(contributionID)
		if !ok {
			return nil, fmt.Errorf("%w: selected Contribution %q is unavailable", ErrMaterializeRemoteRead, contributionID)
		}
		for _, environmentID := range contribution.EnvironmentRefs {
			attachment, err := syncer.FetchEnvironmentAttachment(ctx, request.Store, request.Layout, environmentID, contribution.Source.DeviceID, request.Identities)
			if err != nil {
				return nil, fmt.Errorf("%w: environment attachment %q: %w", ErrMaterializeRemoteRead, environmentID, err)
			}
			if attachment.EnvironmentID != environmentID || attachment.SessionID != graph.SessionID() || attachment.SourceAgent != contribution.Source.Agent || attachment.ObservedAtContribution != contributionID {
				return nil, fmt.Errorf("%w: environment attachment %q does not match its Contribution", ErrMaterializeRemoteRead, environmentID)
			}
			for _, componentRef := range attachment.Components {
				componentKey, err := sessionhub.DeriveEnvironmentKey(request.IdentifierKey, request.Layout.HubKey(), componentRef.Fingerprint)
				if err != nil {
					return nil, fmt.Errorf("%w: derive environment component %q: %w", ErrMaterializeRemoteRead, componentRef.Name, err)
				}
				content, err := syncer.FetchEnvironmentComponent(ctx, request.Store, environmentLayout, componentKey, contribution.Source.DeviceID, request.Identities)
				if err != nil {
					return nil, fmt.Errorf("%w: environment component %q: %w", ErrMaterializeRemoteRead, componentRef.Name, err)
				}
				if !sameEnvironmentComponentRef(content.Component, componentRef) {
					return nil, fmt.Errorf("%w: environment component %q does not match its attachment", ErrMaterializeRemoteRead, componentRef.Name)
				}
				key := environmentComponentIdentity(content.Component)
				if previous, exists := seen[key]; exists {
					if previous != content.Component.Fingerprint {
						return nil, fmt.Errorf("%w: environment component %q has conflicting bodies", ErrMaterializeRemoteRead, content.Component.Name)
					}
					continue
				}
				seen[key] = content.Component.Fingerprint
				contents = append(contents, content)
			}
		}
	}
	sort.Slice(contents, func(i, j int) bool {
		return environmentComponentIdentity(contents[i].Component) < environmentComponentIdentity(contents[j].Component)
	})
	return contents, nil
}

func sameEnvironmentComponentRef(component environment.Component, ref sessionhub.EnvironmentComponentRef) bool {
	return component.Kind == ref.Kind && component.Name == ref.Name && component.Scope == ref.Scope && component.ProjectID == ref.ProjectID && component.Fingerprint == ref.Fingerprint && component.Portability == ref.Portability
}

func environmentComponentIdentity(component environment.Component) string {
	return component.Kind + "\x00" + component.Name + "\x00" + component.Scope + "\x00" + component.ProjectID
}

func cloneEnvironmentContents(contents []environment.ComponentContent) []environment.ComponentContent {
	if contents == nil {
		return nil
	}
	result := make([]environment.ComponentContent, 0, len(contents))
	for _, content := range contents {
		result = append(result, environment.ComponentContent{
			Component: content.Component,
			Content:   append([]byte(nil), content.Content...),
		})
	}
	return result
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
		matching := make(map[string]struct{})
		for _, id := range graph.ContributionIDs() {
			contribution, ok := graph.Contribution(id)
			if ok && contribution.Source.Agent == sourceAgent {
				matching[id] = struct{}{}
			}
		}
		if len(matching) == 0 {
			return nil, fmt.Errorf("%w: no Contribution from source Agent %q", ErrMaterializeHeadSelection, sourceAgent)
		}

		// A source Agent may have several Contributions on one causal track.
		// Passing every matching node as a graph head would select the same
		// ancestry repeatedly and would make the materialize range planner see
		// an artificial overlap. Keep only the matching frontier: a matching
		// Contribution that is an ancestor of another matching Contribution is
		// represented by that descendant's ancestry automatically.
		frontier := make(map[string]bool, len(matching))
		for id := range matching {
			frontier[id] = true
		}
		for id := range matching {
			contribution, ok := graph.Contribution(id)
			if !ok {
				return nil, fmt.Errorf("%w: Contribution %q is unavailable", ErrMaterializeHeadSelection, id)
			}
			seen := make(map[string]struct{})
			stack := append([]string(nil), contribution.Parents...)
			for len(stack) > 0 {
				parentID := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if _, done := seen[parentID]; done {
					continue
				}
				seen[parentID] = struct{}{}
				if _, isMatching := matching[parentID]; isMatching {
					frontier[parentID] = false
				}
				parent, ok := graph.Contribution(parentID)
				if !ok {
					return nil, fmt.Errorf("%w: Contribution %q is unavailable", ErrMaterializeHeadSelection, parentID)
				}
				stack = append(stack, parent.Parents...)
			}
		}
		selected := make([]string, 0, len(frontier))
		for id, isFrontier := range frontier {
			if isFrontier {
				selected = append(selected, id)
			}
		}
		sort.Strings(selected)
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
