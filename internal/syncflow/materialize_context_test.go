package syncflow

import (
	"context"
	"errors"
	"testing"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

type previewCaptureCapability struct {
	materializeCapabilityStub
	lastView   adapter.ContextView
	lastTarget adapter.MaterializeTarget
}

func (s *previewCaptureCapability) EncodeContext(_ context.Context, view adapter.ContextView, target adapter.MaterializeTarget) (adapter.EncodedContext, error) {
	s.lastView = view
	s.lastTarget = target
	return s.encoded, s.encodeErr
}

func testMaterializeSelection() MaterializeSelection {
	return MaterializeSelection{
		Coverage: sessionhub.Coverage{
			SelectedIDs: []string{"contribution-a", "contribution-b"},
		},
		Ranges: []MaterializeRange{
			{
				ContributionID: "contribution-a",
				SourceAgent:    "claude-code",
				ReplicaID:      "replica-a",
				StartRecord:    0,
				EndRecord:      1,
				Records:        [][]byte{[]byte(`{"source":"claude"}`)},
			},
			{
				ContributionID: "contribution-b",
				SourceAgent:    "codex",
				ReplicaID:      "replica-b",
				StartRecord:    0,
				EndRecord:      1,
				Records:        [][]byte{[]byte(`{"source":"codex"}`)},
			},
		},
		SelectedRecordCount: 2,
	}
}

func testPreviewOptions(target adapter.MaterializeCapability) MaterializePreviewOptions {
	return MaterializePreviewOptions{
		SourceCapabilities: map[string]adapter.MaterializeCapability{
			"claude-code": &materializeCapabilityStub{view: adapter.ContextView{
				Version:      adapter.MaterializeViewVersion,
				SourceAgent:  "claude-code",
				SourceFormat: "claude-jsonl-v1",
				Items: []adapter.ContextItem{{
					Kind:        adapter.ContextItemUser,
					Text:        "from Claude",
					SourceIndex: 0,
					Completed:   true,
				}},
			}},
			"codex": &materializeCapabilityStub{view: adapter.ContextView{
				Version:      adapter.MaterializeViewVersion,
				SourceAgent:  "codex",
				SourceFormat: "codex-jsonl-v1",
				Items: []adapter.ContextItem{{
					Kind:        adapter.ContextItemAssistant,
					Text:        "from Codex",
					SourceIndex: 0,
					Completed:   true,
				}},
			}},
		},
		TargetAgent:      "codex",
		TargetCapability: target,
		Target:           adapter.MaterializeTarget{PathSpace: adapter.PathSpace{ProjectRoot: `C:\project`, AgentHome: `C:\agent`}},
		AllowUnsupported: false,
	}
}

func TestPlanMaterializePreviewCombinesRangesWithProvenance(t *testing.T) {
	selection := testMaterializeSelection()
	target := &previewCaptureCapability{materializeCapabilityStub: materializeCapabilityStub{encoded: testEncodedContext()}}
	options := testPreviewOptions(target)
	originalClaude := append([]byte(nil), selection.Ranges[0].Records[0]...)

	preview, err := PlanMaterializePreview(context.Background(), selection, options)
	if err != nil {
		t.Fatalf("PlanMaterializePreview() error = %v", err)
	}
	if preview.TargetNativeID != "generated-target" || preview.TargetAgent != "codex" {
		t.Fatalf("target = %q/%q", preview.TargetNativeID, preview.TargetAgent)
	}
	if preview.SelectedRecordCount != 2 || preview.ContextItems != 2 || len(preview.Sources) != 2 {
		t.Fatalf("preview counts = records %d items %d sources %d", preview.SelectedRecordCount, preview.ContextItems, len(preview.Sources))
	}
	if target.lastView.SourceAgent != materializeMultiSourceAgent || target.lastView.SourceFormat != materializeMultiSourceFormat {
		t.Fatalf("combined view source = %q/%q", target.lastView.SourceAgent, target.lastView.SourceFormat)
	}
	if len(target.lastView.Items) != 2 {
		t.Fatalf("combined items = %+v", target.lastView.Items)
	}
	for index, item := range target.lastView.Items {
		if item.Provenance != nil {
			t.Fatalf("item %d target provenance should be stripped: %+v", index, item.Provenance)
		}
		if item.SourceIndex != 0 {
			t.Fatalf("item %d source index = %d, want local range index 0", index, item.SourceIndex)
		}
	}
	if string(selection.Ranges[0].Records[0]) != string(originalClaude) {
		t.Fatal("selection source records were modified")
	}
	if err := preview.Validate(); err != nil {
		t.Fatalf("preview.Validate() error = %v", err)
	}
}

func TestMaterializePreviewAnnotatesTransientItemsBeforeTargetSanitization(t *testing.T) {
	combined := adapter.ContextView{
		Version: adapter.MaterializeViewVersion,
		Items:   []adapter.ContextItem{{Kind: adapter.ContextItemUser, Text: "visible", SourceIndex: 0}},
	}
	source := combined
	combined.Items = nil
	sourceRange := MaterializeRange{ContributionID: "contribution-a", SourceAgent: "claude-code", ReplicaID: "replica-a"}
	if err := appendMaterializePreviewItems(&combined, source, sourceRange); err != nil {
		t.Fatalf("appendMaterializePreviewItems() error = %v", err)
	}
	if combined.Items[0].Provenance == nil || combined.Items[0].Provenance.ReplicaID != "replica-a" {
		t.Fatalf("transient provenance = %+v", combined.Items[0].Provenance)
	}
	sanitized := materializeTargetView(combined)
	if sanitized.Items[0].Provenance != nil {
		t.Fatalf("target view retained transient provenance: %+v", sanitized.Items[0].Provenance)
	}
}

func TestPlanMaterializePreviewRequiresCompleteSourcesAndCapabilities(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*MaterializeSelection, *MaterializePreviewOptions)
		want   error
	}{
		{
			name: "missing capability",
			mutate: func(_ *MaterializeSelection, options *MaterializePreviewOptions) {
				delete(options.SourceCapabilities, "codex")
			},
			want: ErrMaterializeSourceCapabilityMissing,
		},
		{
			name: "incomplete coverage",
			mutate: func(selection *MaterializeSelection, _ *MaterializePreviewOptions) {
				selection.Coverage.Incomplete = true
				selection.Coverage.Reason = "parent was not fetched"
			},
			want: ErrMaterializePreview,
		},
		{
			name: "same agent only",
			mutate: func(selection *MaterializeSelection, options *MaterializePreviewOptions) {
				options.TargetAgent = "claude-code"
				selection.Ranges = selection.Ranges[:1]
				selection.Coverage.SelectedIDs = selection.Coverage.SelectedIDs[:1]
				selection.SelectedRecordCount = 1
			},
			want: ErrInvalidMaterializeRequest,
		},
		{
			name: "source index outside range",
			mutate: func(_ *MaterializeSelection, options *MaterializePreviewOptions) {
				view := options.SourceCapabilities["claude-code"].(*materializeCapabilityStub).view
				view.Items[0].SourceIndex = 1
				options.SourceCapabilities["claude-code"] = &materializeCapabilityStub{view: view}
			},
			want: ErrInvalidMaterializeRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection := testMaterializeSelection()
			target := &previewCaptureCapability{materializeCapabilityStub: materializeCapabilityStub{encoded: testEncodedContext()}}
			options := testPreviewOptions(target)
			test.mutate(&selection, &options)
			_, err := PlanMaterializePreview(context.Background(), selection, options)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestPlanMaterializePreviewRequiresExplicitUnsupportedPolicy(t *testing.T) {
	selection := testMaterializeSelection()
	target := &previewCaptureCapability{materializeCapabilityStub: materializeCapabilityStub{encoded: testEncodedContext()}}
	options := testPreviewOptions(target)
	view := options.SourceCapabilities["codex"].(*materializeCapabilityStub).view
	view.Unsupported = 1
	options.SourceCapabilities["codex"] = &materializeCapabilityStub{view: view}

	if _, err := PlanMaterializePreview(context.Background(), selection, options); !errors.Is(err, ErrMaterializeUnsupportedSource) {
		t.Fatalf("unsupported source error = %v", err)
	}
	options.AllowUnsupported = true
	if _, err := PlanMaterializePreview(context.Background(), selection, options); err != nil {
		t.Fatalf("explicit unsupported policy error = %v", err)
	}
}
