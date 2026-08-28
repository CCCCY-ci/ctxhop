package sessionhub

import (
	"errors"
	"fmt"
	"sort"
)

var (
	// ErrIncompleteGraph reports a snapshot that cannot describe a complete
	// contribution graph, such as one that omits a referenced parent.
	ErrIncompleteGraph = errors.New("sessionhub: incomplete contribution graph")

	// ErrUnknownParent reports a contribution that refers to a parent absent
	// from the supplied snapshot.
	ErrUnknownParent = errors.New("sessionhub: unknown contribution parent")

	// ErrUnknownHead reports a requested ancestry head absent from the graph.
	ErrUnknownHead = errors.New("sessionhub: unknown contribution head")

	// ErrCycle reports a self-parent or a cycle in contribution ancestry.
	ErrCycle = errors.New("sessionhub: contribution graph contains a cycle")

	// ErrContributionCycle is an explicit alias for ErrCycle.
	ErrContributionCycle = ErrCycle

	// ErrSessionMismatch reports a contribution whose session differs from
	// the session for which a graph is being built.
	ErrSessionMismatch = errors.New("sessionhub: contribution session mismatch")

	// ErrDuplicateContribution reports repeated contribution IDs in a
	// snapshot.
	ErrDuplicateContribution = errors.New("sessionhub: duplicate contribution")

	// ErrNoHeads reports an ancestry selection without an explicit head.
	ErrNoHeads = errors.New("sessionhub: no contribution heads selected")
)

// Coverage describes a pure selection of contribution IDs from a Graph. It
// deliberately says nothing about record ranges or Replica bodies.
//
// SelectedIDs is in deterministic topological order: every selected parent
// appears before each selected child, with lexical ID order breaking ties.
// OmittedIDs is sorted lexically. A successful Graph selection is complete;
// Incomplete and Reason are retained so callers can carry an explicit
// incompleteness state without making the graph access storage.
type Coverage struct {
	// SelectedIDs contains the requested heads and all of their ancestors.
	SelectedIDs []string
	// OmittedIDs contains graph contributions outside the selected ancestry.
	OmittedIDs []string
	// Incomplete reports whether the selected graph is known to be partial.
	Incomplete bool
	// Reason explains an incomplete selection when Incomplete is true.
	Reason string
}

// Graph is an immutable, in-memory causal graph of Contributions for one
// logical session. NewGraph copies all caller-owned slices before retaining
// them, and every method that returns a slice returns a fresh copy.
//
// Graph does not contact a Remote, inspect a filesystem, calculate record
// coverage, or read Replica bodies. It is safe for concurrent read-only use
// after construction.
type Graph struct {
	sessionID string

	contributions map[string]Contribution
	parents       map[string][]string
	ids           []string
	roots         []string
	heads         []string
	topology      []string
}

// ContributionGraph is an alias for Graph for callers that prefer the
// domain-specific type name.
type ContributionGraph = Graph

// NewGraph validates and builds an immutable in-memory contribution graph for
// sessionID. Every contribution must validate, belong exactly to sessionID,
// and have a unique ID. Every parent must be present in contributions; a
// missing parent makes the snapshot incomplete and returns ErrUnknownParent.
// Self-parent links and longer cycles return ErrCycle.
func NewGraph(sessionID string, contributions []Contribution) (*Graph, error) {
	if err := validateOpaqueID(sessionID); err != nil {
		return nil, fmt.Errorf("sessionhub: graph session id %q: %w", sessionID, err)
	}

	graph := &Graph{
		sessionID:     sessionID,
		contributions: make(map[string]Contribution, len(contributions)),
		parents:       make(map[string][]string, len(contributions)),
		ids:           make([]string, 0, len(contributions)),
	}
	for _, contribution := range contributions {
		if err := contribution.Validate(); err != nil {
			return nil, fmt.Errorf("sessionhub: graph contribution %q: %w", contribution.ContributionID, err)
		}
		if contribution.SessionID != sessionID {
			return nil, fmt.Errorf(
				"%w: contribution %q belongs to session %q, graph is for %q",
				ErrSessionMismatch,
				contribution.ContributionID,
				contribution.SessionID,
				sessionID,
			)
		}
		if _, exists := graph.contributions[contribution.ContributionID]; exists {
			return nil, fmt.Errorf("%w: contribution ID %q", ErrDuplicateContribution, contribution.ContributionID)
		}

		id := contribution.ContributionID
		graph.contributions[id] = cloneContribution(contribution)
		graph.parents[id] = sortedStrings(contribution.Parents)
		graph.ids = append(graph.ids, id)
	}
	sort.Strings(graph.ids)

	children := make(map[string][]string, len(graph.contributions))
	for _, id := range graph.ids {
		children[id] = []string{}
	}
	for _, childID := range graph.ids {
		for _, parentID := range graph.parents[childID] {
			if parentID == childID {
				return nil, fmt.Errorf("%w: contribution %q is its own parent", ErrCycle, childID)
			}
			if _, exists := graph.contributions[parentID]; !exists {
				return nil, fmt.Errorf(
					"%w: %w: contribution %q refers to parent %q",
					ErrIncompleteGraph,
					ErrUnknownParent,
					childID,
					parentID,
				)
			}
			children[parentID] = append(children[parentID], childID)
		}
	}
	for _, id := range graph.ids {
		sort.Strings(children[id])
		if len(graph.parents[id]) == 0 {
			graph.roots = append(graph.roots, id)
		}
		if len(children[id]) == 0 {
			graph.heads = append(graph.heads, id)
		}
	}

	topology, err := graph.topologicalOrder()
	if err != nil {
		return nil, err
	}
	graph.topology = topology
	return graph, nil
}

// NewContributionGraph is the domain-specific spelling of NewGraph.
func NewContributionGraph(sessionID string, contributions []Contribution) (*ContributionGraph, error) {
	return NewGraph(sessionID, contributions)
}

// SessionID returns the graph's validated logical session ID.
func (g *Graph) SessionID() string {
	if g == nil {
		return ""
	}
	return g.sessionID
}

// ContributionIDs returns all contribution IDs sorted lexically. The result
// is detached from the graph and may be freely modified by the caller.
func (g *Graph) ContributionIDs() []string {
	if g == nil {
		return []string{}
	}
	return cloneStrings(g.ids)
}

// Contribution returns a detached copy of one immutable contribution. The
// lookup is intentionally read-only so a caller can validate its ranges
// against an already verified Replica body without gaining access to the
// graph's internal slices.
func (g *Graph) Contribution(id string) (Contribution, bool) {
	if g == nil {
		return Contribution{}, false
	}
	contribution, ok := g.contributions[id]
	if !ok {
		return Contribution{}, false
	}
	return cloneContribution(contribution), true
}

// IDs is a concise alias for ContributionIDs.
func (g *Graph) IDs() []string {
	return g.ContributionIDs()
}

// Heads returns the sorted current heads: contributions that are not a
// parent of any contribution in the supplied snapshot. The result is a fresh
// slice.
func (g *Graph) Heads() []string {
	if g == nil {
		return []string{}
	}
	return cloneStrings(g.heads)
}

// CurrentHeads is an explicit alias for Heads.
func (g *Graph) CurrentHeads() []string {
	return g.Heads()
}

// Roots returns the sorted contributions with no parents. The result is a
// fresh slice.
func (g *Graph) Roots() []string {
	if g == nil {
		return []string{}
	}
	return cloneStrings(g.roots)
}

// Ancestry returns the selected heads and every transitive parent in
// deterministic topological order. Parents precede children; lexical ID
// order breaks ties. A head may be any known contribution ID, which lets a
// caller intentionally select an older snapshot point as a boundary. Unknown
// heads, unknown parents, cycles, or an empty head list return an error.
func (g *Graph) Ancestry(heads ...string) ([]string, error) {
	selection, err := g.Select(heads...)
	if err != nil {
		return nil, err
	}
	return cloneStrings(selection.SelectedIDs), nil
}

// AncestryForHeads accepts a slice form of Ancestry without taking ownership
// of the slice.
func (g *Graph) AncestryForHeads(heads []string) ([]string, error) {
	return g.Ancestry(heads...)
}

// SelectedAncestry is an explicit alias for Ancestry.
func (g *Graph) SelectedAncestry(heads ...string) ([]string, error) {
	return g.Ancestry(heads...)
}

// Select returns a pure graph-only Coverage for the requested heads. The
// selected IDs include each requested head and all ancestors; omitted IDs are
// every other contribution in the graph. No record or Replica data is read.
func (g *Graph) Select(heads ...string) (Coverage, error) {
	if g == nil {
		return Coverage{}, fmt.Errorf("%w: nil graph", ErrInvalidModel)
	}
	if len(heads) == 0 {
		return Coverage{}, ErrNoHeads
	}

	requested := cloneStrings(heads)
	sort.Strings(requested)
	for _, head := range requested {
		if _, exists := g.contributions[head]; !exists {
			return Coverage{}, fmt.Errorf("%w: contribution %q", ErrUnknownHead, head)
		}
	}

	topology, err := g.topologicalOrder()
	if err != nil {
		return Coverage{}, err
	}

	selected := make(map[string]struct{}, len(requested))
	for _, head := range requested {
		stack := []string{head}
		for len(stack) > 0 {
			last := len(stack) - 1
			id := stack[last]
			stack = stack[:last]
			if _, seen := selected[id]; seen {
				continue
			}
			selected[id] = struct{}{}

			parentIDs := g.parents[id]
			for index := len(parentIDs) - 1; index >= 0; index-- {
				parentID := parentIDs[index]
				if _, exists := g.contributions[parentID]; !exists {
					return Coverage{}, fmt.Errorf(
						"%w: %w: contribution %q refers to parent %q",
						ErrIncompleteGraph,
						ErrUnknownParent,
						id,
						parentID,
					)
				}
				stack = append(stack, parentID)
			}
		}
	}

	selection := Coverage{
		SelectedIDs: make([]string, 0, len(selected)),
		OmittedIDs:  make([]string, 0, len(g.ids)-len(selected)),
	}
	for _, id := range topology {
		if _, ok := selected[id]; ok {
			selection.SelectedIDs = append(selection.SelectedIDs, id)
		}
	}
	for _, id := range g.ids {
		if _, ok := selected[id]; !ok {
			selection.OmittedIDs = append(selection.OmittedIDs, id)
		}
	}
	return selection, nil
}

// SelectAncestry is an explicit alias for Select.
func (g *Graph) SelectAncestry(heads ...string) (Coverage, error) {
	return g.Select(heads...)
}

func (g *Graph) topologicalOrder() ([]string, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: nil graph", ErrInvalidModel)
	}

	indegree := make(map[string]int, len(g.ids))
	children := make(map[string][]string, len(g.ids))
	for _, id := range g.ids {
		if _, exists := g.contributions[id]; !exists {
			return nil, fmt.Errorf("%w: contribution index contains unknown ID %q", ErrInvalidModel, id)
		}
		parentIDs, exists := g.parents[id]
		if !exists {
			return nil, fmt.Errorf("%w: contribution %q has no parent index", ErrInvalidModel, id)
		}
		children[id] = []string{}
		indegree[id] = 0
		for _, parentID := range parentIDs {
			if parentID == id {
				return nil, fmt.Errorf("%w: contribution %q is its own parent", ErrCycle, id)
			}
			if _, exists := g.contributions[parentID]; !exists {
				return nil, fmt.Errorf(
					"%w: %w: contribution %q refers to parent %q",
					ErrIncompleteGraph,
					ErrUnknownParent,
					id,
					parentID,
				)
			}
			indegree[id]++
			children[parentID] = append(children[parentID], id)
		}
	}
	for _, childIDs := range children {
		sort.Strings(childIDs)
	}

	ready := make([]string, 0, len(g.ids))
	for _, id := range g.ids {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	emitted := make(map[string]struct{}, len(g.ids))
	order := make([]string, 0, len(g.ids))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		if _, alreadyEmitted := emitted[id]; alreadyEmitted {
			continue
		}
		emitted[id] = struct{}{}
		order = append(order, id)

		for _, childID := range children[id] {
			indegree[childID]--
			if indegree[childID] == 0 {
				insertSorted(&ready, childID)
			}
		}
	}
	if len(order) != len(g.ids) {
		for _, id := range g.ids {
			if _, alreadyEmitted := emitted[id]; !alreadyEmitted {
				return nil, fmt.Errorf("%w: cycle includes contribution %q", ErrCycle, id)
			}
		}
		return nil, ErrCycle
	}
	return order, nil
}

func insertSorted(values *[]string, value string) {
	index := sort.SearchStrings(*values, value)
	*values = append(*values, "")
	copy((*values)[index+1:], (*values)[index:])
	(*values)[index] = value
}

func cloneContribution(contribution Contribution) Contribution {
	copyContribution := contribution
	copyContribution.Parents = append([]string(nil), contribution.Parents...)
	copyContribution.Ranges = append([]RangeRef(nil), contribution.Ranges...)
	copyContribution.EnvironmentRefs = append([]string(nil), contribution.EnvironmentRefs...)
	return copyContribution
}

func cloneStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}
