package syncflow

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

func TestFetchRestorePlanPropagatesRemoteReadFailures(t *testing.T) {
	_, err := FetchRestorePlan(
		context.Background(),
		nil,
		"project",
		"session",
		nil,
		adapter.PathSpace{ProjectRoot: "/project", AgentHome: "/agent"},
		adapter.Installation{Compatibility: adapter.CompatFull},
		RestoreOptions{},
	)
	if err == nil {
		t.Fatal("FetchRestorePlan accepted a nil remote")
	}
}

func TestPlanRestoreRejectsMalformedResolutionMetadata(t *testing.T) {
	records := [][]byte{[]byte(`{"ok":true}`)}
	digest := mustDigest(t, records)
	version := syncer.Version{Records: records, Devices: []string{"device"}, HeadDigest: digest}
	space := adapter.PathSpace{ProjectRoot: "/project", AgentHome: "/agent"}
	full := adapter.Installation{Compatibility: adapter.CompatFull}

	cases := []struct {
		name       string
		resolution syncer.Resolution
	}{
		{
			name:       "unknown kind",
			resolution: syncer.Resolution{Kind: syncer.ResolutionKind(99), Versions: []syncer.Version{version}},
		},
		{
			name:       "no versions",
			resolution: syncer.Resolution{Kind: syncer.ResolutionConsistent},
		},
		{
			name: "fork with one version",
			resolution: syncer.Resolution{
				Kind:     syncer.ResolutionFork,
				Versions: []syncer.Version{version},
			},
		},
		{
			name: "consistent with multiple versions",
			resolution: syncer.Resolution{
				Kind:     syncer.ResolutionConsistent,
				Versions: []syncer.Version{version, version},
			},
		},
		{
			name: "common prefix too long",
			resolution: syncer.Resolution{
				Kind:         syncer.ResolutionConsistent,
				CommonPrefix: 2,
				Versions:     []syncer.Version{version},
			},
		},
		{
			name: "empty records",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Devices: []string{"device"},
				}},
			},
		},
		{
			name: "no devices",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Records:    records,
					HeadDigest: digest,
				}},
			},
		},
		{
			name: "empty device",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Records:    records,
					Devices:    []string{""},
					HeadDigest: digest,
				}},
			},
		},
		{
			name: "duplicate device",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Records:    records,
					Devices:    []string{"device", "device"},
					HeadDigest: digest,
				}},
			},
		},
		{
			name: "invalid record",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Records: [][]byte{[]byte("not json")},
					Devices: []string{"device"},
				}},
			},
		},
		{
			name: "wrong digest",
			resolution: syncer.Resolution{
				Kind: syncer.ResolutionConsistent,
				Versions: []syncer.Version{{
					Records: records,
					Devices: []string{"device"},
				}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanRestore(tc.resolution, space, full, RestoreOptions{})
			if err == nil || !errors.Is(err, ErrInvalidRestoreResolution) {
				t.Fatalf("error = %v, want ErrInvalidRestoreResolution", err)
			}
		})
	}
}

func TestPlanRestoreAcceptsFastForwardResolutionWithOneMaximalVersion(t *testing.T) {
	records := [][]byte{[]byte(`{"ok":true}`)}
	plan, err := PlanRestore(
		syncer.Resolution{
			Kind:         syncer.ResolutionFastForward,
			CommonPrefix: 0,
			Versions: []syncer.Version{{
				Records:    records,
				Devices:    []string{"device"},
				HeadDigest: mustDigest(t, records),
			}},
		},
		adapter.PathSpace{ProjectRoot: "/project", AgentHome: "/agent"},
		adapter.Installation{Compatibility: adapter.CompatFull},
		RestoreOptions{},
	)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}
	if plan.ResolutionKind != syncer.ResolutionFastForward {
		t.Fatalf("kind = %v, want fast-forward", plan.ResolutionKind)
	}
}
