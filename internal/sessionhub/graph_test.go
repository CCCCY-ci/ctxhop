package sessionhub

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func graphTestContribution(id string, parents ...string) Contribution {
	return Contribution{
		Version:        ModelVersion,
		ContributionID: id,
		SessionID:      "s",
		Source: ContributionSource{
			Agent:      "codex",
			ReplicaID:  "replica",
			DeviceID:   "device",
			Generation: 1,
		},
		Parents: parents,
		Ranges: []RangeRef{{
			ReplicaID:    "replica",
			StartRecord:  0,
			EndRecord:    1,
			PrefixDigest: strings.Repeat("0", 64),
			RangeDigest:  strings.Repeat("1", 64),
		}},
		CreatedAt: time.Unix(1, 0).UTC(),
	}
}

func TestGraphRootsAndHeads(t *testing.T) {
	graph, err := NewGraph("s", []Contribution{
		graphTestContribution("b", "a"),
		graphTestContribution("c"),
		graphTestContribution("a"),
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	if got, want := graph.Roots(), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Roots() = %v, want %v", got, want)
	}
	if got, want := graph.Heads(), []string{"b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Heads() = %v, want %v", got, want)
	}
	if got, want := graph.ContributionIDs(), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ContributionIDs() = %v, want %v", got, want)
	}
}

func TestGraphParallelBranchesAndAncestrySelection(t *testing.T) {
	root := graphTestContribution("a")
	left := graphTestContribution("b", "a")
	right := graphTestContribution("c", "a")
	// Timestamps are intentionally unrelated to the expected order.
	root.CreatedAt = time.Unix(30, 0).UTC()
	left.CreatedAt = time.Unix(10, 0).UTC()
	right.CreatedAt = time.Unix(20, 0).UTC()

	graph, err := NewGraph("s", []Contribution{right, root, left})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if got, want := graph.Heads(), []string{"b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Heads() = %v, want %v", got, want)
	}

	selection, err := graph.Select("b", "c")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got, want := selection.SelectedIDs, []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SelectedIDs = %v, want %v", got, want)
	}
	if got, want := selection.OmittedIDs, []string{}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OmittedIDs = %v, want %v", got, want)
	}
	if selection.Incomplete || selection.Reason != "" {
		t.Fatalf("selection incomplete state = (%t, %q), want (false, empty)", selection.Incomplete, selection.Reason)
	}
}

func TestGraphAncestryIncludesHeadsAndOmitsUnselectedContributions(t *testing.T) {
	graph, err := NewGraph("s", []Contribution{
		graphTestContribution("d", "b", "c"),
		graphTestContribution("c", "a"),
		graphTestContribution("e"),
		graphTestContribution("a"),
		graphTestContribution("b", "a"),
	})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}

	got, err := graph.Ancestry("d")
	if err != nil {
		t.Fatalf("Ancestry: %v", err)
	}
	if want := []string{"a", "b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ancestry(d) = %v, want %v", got, want)
	}

	selection, err := graph.SelectAncestry("d")
	if err != nil {
		t.Fatalf("SelectAncestry: %v", err)
	}
	if got, want := selection.OmittedIDs, []string{"e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OmittedIDs = %v, want %v", got, want)
	}
	got, err = graph.AncestryForHeads([]string{"b", "e"})
	if err != nil {
		t.Fatalf("AncestryForHeads: %v", err)
	}
	if want := []string{"a", "b", "e"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AncestryForHeads(b,e) = %v, want %v", got, want)
	}
}

func TestGraphRejectsUnknownParentAndHead(t *testing.T) {
	_, err := NewGraph("s", []Contribution{graphTestContribution("b", "missing")})
	if !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("unknown parent error = %v, want ErrUnknownParent", err)
	}
	if !errors.Is(err, ErrIncompleteGraph) {
		t.Fatalf("unknown parent error = %v, want ErrIncompleteGraph", err)
	}

	graph, err := NewGraph("s", []Contribution{graphTestContribution("a")})
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if _, err := graph.Ancestry("missing"); !errors.Is(err, ErrUnknownHead) {
		t.Fatalf("unknown head error = %v, want ErrUnknownHead", err)
	}
}

func TestGraphRejectsSelfParentAndCycles(t *testing.T) {
	for name, contributions := range map[string][]Contribution{
		"self parent": {graphTestContribution("a", "a")},
		"cycle": {
			graphTestContribution("a", "b"),
			graphTestContribution("b", "a"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGraph("s", contributions); !errors.Is(err, ErrCycle) {
				t.Fatalf("NewGraph error = %v, want ErrCycle", err)
			}
		})
	}
}

func TestGraphRejectsDuplicateContributionID(t *testing.T) {
	_, err := NewGraph("s", []Contribution{
		graphTestContribution("a"),
		graphTestContribution("a"),
	})
	if !errors.Is(err, ErrDuplicateContribution) {
		t.Fatalf("duplicate error = %v, want ErrDuplicateContribution", err)
	}
}

func TestGraphDoesNotMutateInputsOrExposeMutableSlices(t *testing.T) {
	parent := graphTestContribution("a")
	child := graphTestContribution("b", "a")
	contributions := []Contribution{child, parent}
	original := []Contribution{cloneContribution(child), cloneContribution(parent)}

	graph, err := NewGraph("s", contributions)
	if err != nil {
		t.Fatalf("NewGraph: %v", err)
	}
	if !reflect.DeepEqual(contributions, original) {
		t.Fatalf("NewGraph mutated input contributions: got %+v, want %+v", contributions, original)
	}

	contributions[0].ContributionID = "z"
	contributions[0].Parents[0] = "z"
	contributions[1].ContributionID = "z"
	contributions[1].Parents = []string{"z"}

	ids := graph.ContributionIDs()
	ids[0] = "z"
	heads := graph.Heads()
	heads[0] = "z"
	roots := graph.Roots()
	roots[0] = "z"
	selected, err := graph.Ancestry("b")
	if err != nil {
		t.Fatalf("Ancestry after input mutation: %v", err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(selected, want) {
		t.Fatalf("Ancestry after input mutation = %v, want %v", selected, want)
	}

	coverage, err := graph.Select("b")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	coverage.SelectedIDs[0] = "z"
	if got, want := graph.ContributionIDs(), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("graph changed through returned slices: IDs = %v, want %v", got, want)
	}
}

func TestGraphRejectsSessionMismatchAndUnsafeSessionID(t *testing.T) {
	wrongSession := graphTestContribution("a")
	wrongSession.SessionID = "other"
	if _, err := NewGraph("s", []Contribution{wrongSession}); !errors.Is(err, ErrSessionMismatch) {
		t.Fatalf("session mismatch error = %v, want ErrSessionMismatch", err)
	}

	if _, err := NewGraph("unsafe session", nil); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("unsafe session error = %v, want ErrInvalidIdentity", err)
	}
}
