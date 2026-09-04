package syncflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
)

type materializeCapabilityStub struct {
	view             adapter.ContextView
	encoded          adapter.EncodedContext
	decodeErr        error
	newIDErr         error
	encodeErr        error
	validateErr      error
	newIDCalls       int
	decodeCalls      int
	encodeCalls      int
	validateCalls    int
	validatedRecords [][]byte
}

func (s *materializeCapabilityStub) DecodeContext(_ context.Context, records [][]byte) (adapter.ContextView, error) {
	s.decodeCalls++
	if s.decodeErr != nil {
		return adapter.ContextView{}, s.decodeErr
	}
	if len(records) == 0 {
		return adapter.ContextView{}, errors.New("empty source")
	}
	return s.view, nil
}

func (s *materializeCapabilityStub) NewSessionID(context.Context) (string, error) {
	s.newIDCalls++
	if s.newIDErr != nil {
		return "", s.newIDErr
	}
	return "generated-target", nil
}

func (s *materializeCapabilityStub) EncodeContext(_ context.Context, _ adapter.ContextView, target adapter.MaterializeTarget) (adapter.EncodedContext, error) {
	s.encodeCalls++
	if s.encodeErr != nil {
		return adapter.EncodedContext{}, s.encodeErr
	}
	if target.NativeID == "" {
		return adapter.EncodedContext{}, errors.New("target ID is empty")
	}
	return s.encoded, nil
}

func (s *materializeCapabilityStub) ValidateMaterialized(_ context.Context, records [][]byte, _ adapter.MaterializeTarget) error {
	s.validateCalls++
	s.validatedRecords = cloneMaterializeRecords(records)
	return s.validateErr
}

func testMaterializeOptions() MaterializeOptions {
	return MaterializeOptions{
		SourceAgent: "claude-code",
		TargetAgent: "codex",
		SourceSnapshot: adapter.SessionData{
			Records: [][]byte{[]byte(`{"type":"user"}`)},
		},
		Target: adapter.MaterializeTarget{
			PathSpace: adapter.PathSpace{ProjectRoot: `C:\work\project`, AgentHome: `C:\Users\user\.codex`},
			CreatedAt: time.Unix(100, 0).UTC(),
		},
	}
}

func testMaterializeView() adapter.ContextView {
	return adapter.ContextView{
		Version:      adapter.MaterializeViewVersion,
		SourceAgent:  "claude-code",
		SourceFormat: "claude-jsonl-v1",
		Items: []adapter.ContextItem{{
			Kind:        adapter.ContextItemUser,
			Text:        "continue the task",
			SourceIndex: 0,
			Completed:   true,
		}},
	}
}

func testEncodedContext() adapter.EncodedContext {
	return adapter.EncodedContext{
		Records:              [][]byte{[]byte(`{"type":"session_meta"}`), []byte(`{"type":"event_msg"}`)},
		Stats:                adapter.MaterializeStats{Converted: 1},
		SourceViewVersion:    adapter.MaterializeViewVersion,
		TargetAdapterVersion: "codex-materialize-v1",
	}
}

func TestPlanMaterializeIsReadOnlyAndAllocatesTargetID(t *testing.T) {
	options := testMaterializeOptions()
	original := append([]byte(nil), options.SourceSnapshot.Records[0]...)
	source := &materializeCapabilityStub{view: testMaterializeView()}
	target := &materializeCapabilityStub{encoded: testEncodedContext()}

	plan, err := PlanMaterialize(context.Background(), source, target, options)
	if err != nil {
		t.Fatalf("PlanMaterialize() error = %v", err)
	}
	if plan.TargetNativeID != "generated-target" {
		t.Fatalf("TargetNativeID = %q", plan.TargetNativeID)
	}
	if source.decodeCalls != 1 || target.newIDCalls != 1 || target.encodeCalls != 1 || target.validateCalls != 1 {
		t.Fatalf("capability calls = decode %d, new ID %d, encode %d, validate %d", source.decodeCalls, target.newIDCalls, target.encodeCalls, target.validateCalls)
	}
	if string(options.SourceSnapshot.Records[0]) != string(original) {
		t.Fatal("source snapshot was modified")
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("plan.Validate() error = %v", err)
	}
	plan.EncodedRecords[0][0] = 'X'
	if string(target.validatedRecords[0]) != `{"type":"session_meta"}` {
		t.Fatal("plan records alias the target capability input")
	}
}

func TestPlanMaterializePreservesMultilineContext(t *testing.T) {
	options := testMaterializeOptions()
	view := testMaterializeView()
	view.Items[0].Text = "line one\nline two"
	source := &materializeCapabilityStub{view: view}
	target := &materializeCapabilityStub{encoded: testEncodedContext()}

	if _, err := PlanMaterialize(context.Background(), source, target, options); err != nil {
		t.Fatalf("PlanMaterialize() error = %v", err)
	}
}

func TestPlanMaterializeUsesExplicitTargetID(t *testing.T) {
	options := testMaterializeOptions()
	options.Target.NativeID = "existing-target"
	source := &materializeCapabilityStub{view: testMaterializeView()}
	target := &materializeCapabilityStub{encoded: testEncodedContext()}

	plan, err := PlanMaterialize(context.Background(), source, target, options)
	if err != nil {
		t.Fatalf("PlanMaterialize() error = %v", err)
	}
	if plan.TargetNativeID != options.Target.NativeID {
		t.Fatalf("TargetNativeID = %q, want %q", plan.TargetNativeID, options.Target.NativeID)
	}
	if target.newIDCalls != 0 {
		t.Fatalf("NewSessionID calls = %d, want 0", target.newIDCalls)
	}
}

func TestPlanMaterializeRejectsUnsafeOrIncompleteRequest(t *testing.T) {
	validSource := &materializeCapabilityStub{view: testMaterializeView()}
	validTarget := &materializeCapabilityStub{encoded: testEncodedContext()}
	tests := []struct {
		name    string
		mutate  func(*MaterializeOptions)
		wantErr error
	}{
		{name: "same agent", mutate: func(o *MaterializeOptions) { o.TargetAgent = o.SourceAgent }, wantErr: ErrInvalidMaterializeRequest},
		{name: "empty source", mutate: func(o *MaterializeOptions) { o.SourceSnapshot.Records = nil }, wantErr: ErrInvalidMaterializeRequest},
		{name: "dropped tail", mutate: func(o *MaterializeOptions) { o.SourceSnapshot.DroppedTail = true }, wantErr: ErrInvalidMaterializeRequest},
		{name: "skipped record", mutate: func(o *MaterializeOptions) { o.SourceSnapshot.Skipped = 1 }, wantErr: ErrInvalidMaterializeRequest},
		{name: "missing project root", mutate: func(o *MaterializeOptions) { o.Target.PathSpace.ProjectRoot = "" }, wantErr: ErrInvalidPathSpace},
		{name: "unsafe agent home", mutate: func(o *MaterializeOptions) { o.Target.PathSpace.AgentHome = "C:\\agent\nunsafe" }, wantErr: ErrInvalidPathSpace},
		{name: "unsafe target id", mutate: func(o *MaterializeOptions) { o.Target.NativeID = "..\\target" }, wantErr: ErrInvalidMaterializeRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := testMaterializeOptions()
			test.mutate(&options)
			_, err := PlanMaterialize(context.Background(), validSource, validTarget, options)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.wantErr)
			}
		})
	}
}

func TestPlanMaterializeRejectsCapabilityFailure(t *testing.T) {
	options := testMaterializeOptions()
	source := &materializeCapabilityStub{view: testMaterializeView(), decodeErr: errors.New("malformed source")}
	target := &materializeCapabilityStub{encoded: testEncodedContext()}
	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrMaterializeCapability) {
		t.Fatalf("decode error = %v, want ErrMaterializeCapability", err)
	}

	source = &materializeCapabilityStub{view: testMaterializeView()}
	target = &materializeCapabilityStub{encoded: testEncodedContext(), validateErr: errors.New("invalid target")}
	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrMaterializeCapability) {
		t.Fatalf("validate error = %v, want ErrMaterializeCapability", err)
	}
}

func TestPlanMaterializeRejectsMismatchedSourceView(t *testing.T) {
	options := testMaterializeOptions()
	view := testMaterializeView()
	view.SourceAgent = "codex"
	source := &materializeCapabilityStub{view: view}
	target := &materializeCapabilityStub{encoded: testEncodedContext()}
	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrInvalidMaterializeRequest) {
		t.Fatalf("error = %v, want ErrInvalidMaterializeRequest", err)
	}

	view = testMaterializeView()
	view.SourceAgent = " claude-code"
	source = &materializeCapabilityStub{view: view}
	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrInvalidMaterializeRequest) {
		t.Fatalf("whitespace source Agent error = %v, want ErrInvalidMaterializeRequest", err)
	}

	view = testMaterializeView()
	view.SourceFormat = " " + view.SourceFormat
	source = &materializeCapabilityStub{view: view}
	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrInvalidMaterializeRequest) {
		t.Fatalf("whitespace source format error = %v, want ErrInvalidMaterializeRequest", err)
	}
}

func TestPlanMaterializeRequiresExplicitUnsupportedPolicy(t *testing.T) {
	options := testMaterializeOptions()
	source := &materializeCapabilityStub{view: testMaterializeView()}
	source.view.Unsupported = 1
	target := &materializeCapabilityStub{encoded: testEncodedContext()}

	if _, err := PlanMaterialize(context.Background(), source, target, options); !errors.Is(err, ErrMaterializeUnsupportedSource) {
		t.Fatalf("unsupported source error = %v, want ErrMaterializeUnsupportedSource", err)
	}
	if target.encodeCalls != 0 {
		t.Fatalf("target EncodeContext calls = %d, want 0 for blocked conversion", target.encodeCalls)
	}

	options.AllowUnsupported = true
	if _, err := PlanMaterialize(context.Background(), source, target, options); err != nil {
		t.Fatalf("explicit unsupported policy error = %v", err)
	}
}
