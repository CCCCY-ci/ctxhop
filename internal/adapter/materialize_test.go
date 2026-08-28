package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func materializeTestTarget() MaterializeTarget {
	return MaterializeTarget{
		NativeID: "target-session",
		PathSpace: PathSpace{
			ProjectRoot: `C:\work\project`,
			AgentHome:   `C:\Users\user\.agent`,
		},
		CreatedAt: time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC),
	}
}

func TestClaudeDecodeFiltersMetadataReasoningSecretsAndUnpairedTools(t *testing.T) {
	records := [][]byte{
		[]byte(`{"type":"user","timestamp":"2026-08-27T08:00:00Z","message":{"role":"user","content":"please continue"}}`),
		[]byte(`{"type":"assistant","timestamp":"2026-08-27T08:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden chain of thought"},{"type":"text","text":"I will inspect the project."}]}}`),
		[]byte(`{"type":"system","message":{"role":"system","content":"system prompt"}}`),
		[]byte(`{"type":"user","isMeta":true,"message":{"role":"user","content":"internal metadata"}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":"Authorization: Bearer do-not-copy"}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read_file","input":{}}]}}`),
	}

	view, err := (Layout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	if view.SourceAgent != "claude-code" || view.SourceFormat == "" {
		t.Fatalf("source metadata = (%q, %q)", view.SourceAgent, view.SourceFormat)
	}
	if view.Filtered < 4 {
		t.Fatalf("Filtered = %d, want at least metadata/reasoning/secret records", view.Filtered)
	}
	if view.Unsupported < 1 {
		t.Fatalf("Unsupported = %d, want an unpaired tool", view.Unsupported)
	}
	for _, item := range view.Items {
		if strings.Contains(item.Text, "hidden chain") || strings.Contains(item.Text, "Authorization") || strings.Contains(item.Text, "Bearer") {
			t.Fatalf("unsafe content survived in item: %+v", item)
		}
		if item.Kind == ContextItemToolCall && item.Completed {
			t.Fatalf("unpaired tool call was marked completed: %+v", item)
		}
	}
}

func TestClaudeDecodePairsToolCallAndResult(t *testing.T) {
	records := [][]byte{
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read_file","input":{}}]}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"file contents"}]}}`),
	}

	view, err := (Layout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	if len(view.Items) != 2 {
		t.Fatalf("item count = %d, want 2", len(view.Items))
	}
	if view.Items[0].Kind != ContextItemToolCall || !view.Items[0].Completed || view.Items[1].Kind != ContextItemToolResult || !view.Items[1].Completed {
		t.Fatalf("paired items = %+v", view.Items)
	}
}

func TestClaudeToolPairIgnoresFilteredCounterpart(t *testing.T) {
	records := [][]byte{
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read_file","input":{}}]}}`),
		[]byte(`{"type":"user","isSidechain":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"hidden result"}]}}`),
	}

	view, err := (Layout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	for _, item := range view.Items {
		if item.Kind == ContextItemToolCall && item.Completed {
			t.Fatalf("tool call paired with filtered result: %+v", item)
		}
		if strings.Contains(item.Text, "hidden result") {
			t.Fatalf("filtered result survived: %+v", item)
		}
	}
	if view.Unsupported < 1 || view.Filtered < 1 {
		t.Fatalf("diagnostics = unsupported %d, filtered %d; want both", view.Unsupported, view.Filtered)
	}
}

func TestMaterializeSafetyFailsClosedForSecretsMalformedMetadataAndLegacyTools(t *testing.T) {
	records := [][]byte{
		[]byte(`{"type":"user","message":{"role":"user","content":"Cookie: session=do-not-copy"}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":"token: do-not-copy"}}`),
		[]byte(`{"type":"user","isMeta":"not-a-boolean","message":{"role":"user","content":"metadata"}}`),
		[]byte(`{"type":"tool","message":{"role":"tool","content":"legacy result"}}`),
		[]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read_file","input":{}}]}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"secret=do-not-copy"}]}}`),
	}

	view, err := (Layout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	for _, item := range view.Items {
		if strings.Contains(strings.ToLower(item.Text), "do-not-copy") {
			t.Fatalf("sensitive text survived: %+v", item)
		}
		if (item.Kind == ContextItemToolCall || item.Kind == ContextItemToolResult) && item.Completed {
			t.Fatalf("unsafe or legacy tool item was marked complete: %+v", item)
		}
	}
	if view.Unsupported < 3 {
		t.Fatalf("Unsupported = %d, want malformed metadata, legacy and unpaired tool diagnostics", view.Unsupported)
	}
}

func TestCodexToolPairRequiresSafeCounterpart(t *testing.T) {
	records := [][]byte{
		[]byte(`{"timestamp":"2026-08-27T08:00:00Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"Bearer do-not-copy"}}`),
	}
	view, err := (CodexLayout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	for _, item := range view.Items {
		if item.Kind == ContextItemToolCall || item.Kind == ContextItemToolResult {
			if item.Completed {
				t.Fatalf("unsafe Codex tool item was marked complete: %+v", item)
			}
		}
		if strings.Contains(strings.ToLower(item.Text), "do-not-copy") {
			t.Fatalf("sensitive Codex text survived: %+v", item)
		}
	}
}

func TestCodexDecodeAndNativeEncoding(t *testing.T) {
	records := [][]byte{
		[]byte(`{"timestamp":"2026-08-27T08:00:00Z","type":"session_meta","payload":{"id":"source","session_id":"source","cwd":"C:\\work\\project","workspace_roots":["C:\\work\\project"]}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:01Z","type":"turn_context","payload":{"cwd":"C:\\work\\project","workspace_roots":["C:\\work\\project"]}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"continue the task"}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will continue."}]}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:04Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"}}`),
		[]byte(`{"timestamp":"2026-08-27T08:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"file contents"}}`),
	}

	view, err := (CodexLayout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	if view.SourceAgent != "codex" || len(view.Items) != 4 {
		t.Fatalf("Codex view = %+v", view)
	}

	target := materializeTestTarget()
	encoded, err := (CodexLayout{}).EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("EncodeContext() error = %v", err)
	}
	if err := (CodexLayout{}).ValidateMaterialized(context.Background(), encoded.Records, target); err != nil {
		t.Fatalf("ValidateMaterialized() error = %v", err)
	}
	if len(encoded.Records) != len(view.Items)+1 {
		t.Fatalf("encoded record count = %d, want %d", len(encoded.Records), len(view.Items)+1)
	}
	joined := string(encoded.Records[0])
	for _, record := range encoded.Records[1:] {
		joined += string(record)
	}
	if strings.Contains(joined, "source") || strings.Contains(joined, "hidden") {
		t.Fatalf("source-native metadata leaked into target records: %s", joined)
	}
}

func TestCodexEncodingMarksIncompleteVisibleItems(t *testing.T) {
	view := ContextView{
		Version: MaterializeViewVersion,
		Items: []ContextItem{
			{Kind: ContextItemUser, Text: "unfinished user input", Completed: false},
			{Kind: ContextItemAssistant, Text: "unfinished assistant output", Completed: false},
		},
	}

	target := materializeTestTarget()
	encoded, err := (CodexLayout{}).EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("EncodeContext() error = %v", err)
	}
	if len(encoded.Records) != 3 {
		t.Fatalf("encoded record count = %d, want 3", len(encoded.Records))
	}
	for index, raw := range encoded.Records[1:] {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string `json:"type"`
				Completed *bool  `json:"completed"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("record %d JSON error = %v", index+2, err)
		}
		if record.Type != "event_msg" || record.Payload.Type != "notice" || record.Payload.Completed == nil || *record.Payload.Completed {
			t.Fatalf("record %d does not preserve incompleteness: %s", index+2, raw)
		}
	}
}

func TestClaudeNativeEncodingAndValidation(t *testing.T) {
	view := ContextView{
		Version:     MaterializeViewVersion,
		SourceAgent: "codex",
		Items: []ContextItem{
			{Kind: ContextItemUser, Text: "hello"},
			{Kind: ContextItemAssistant, Text: "world", Completed: true},
			{Kind: ContextItemNotice, Text: "Imported context boundary", Completed: false},
		},
	}
	target := materializeTestTarget()
	encoded, err := (Layout{}).EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("EncodeContext() error = %v", err)
	}
	if err := (Layout{}).ValidateMaterialized(context.Background(), encoded.Records, target); err != nil {
		t.Fatalf("ValidateMaterialized() error = %v", err)
	}
	if encoded.Stats.Converted != len(view.Items) || encoded.Stats.Summarized < 1 {
		t.Fatalf("encoding stats = %+v", encoded.Stats)
	}
}

func TestMaterializedRecordsRoundTripThroughBuiltInLayouts(t *testing.T) {
	view := ContextView{
		Version:      MaterializeViewVersion,
		SourceAgent:  "codex",
		SourceFormat: "codex-jsonl",
		Items: []ContextItem{
			{Kind: ContextItemUser, Text: "continue the task", Completed: true},
			{Kind: ContextItemAssistant, Text: "I will continue.", Completed: true},
		},
	}
	target := materializeTestTarget()

	claudeHome := t.TempDir()
	claude := Layout{Home: claudeHome}
	claudeRecords, err := claude.EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("Claude EncodeContext() error = %v", err)
	}
	if err := claude.WriteSession(target.PathSpace.ProjectRoot, target.NativeID, claudeRecords.Records); err != nil {
		t.Fatalf("Claude WriteSession() error = %v", err)
	}
	claudeRefs, err := claude.DiscoverSessions(target.PathSpace.ProjectRoot)
	if err != nil || len(claudeRefs) != 1 || claudeRefs[0].NativeID != target.NativeID {
		t.Fatalf("Claude discovery = refs %v, error %v", claudeRefs, err)
	}
	claudeRead, err := claude.ReadSession(claudeRefs[0])
	if err != nil {
		t.Fatalf("Claude ReadSession() error = %v", err)
	}
	if err := claude.ValidateMaterialized(context.Background(), claudeRead.Records, target); err != nil {
		t.Fatalf("Claude readback validation error = %v", err)
	}

	codexHome := t.TempDir()
	codex := CodexLayout{Home: codexHome}
	codexRecords, err := codex.EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("Codex EncodeContext() error = %v", err)
	}
	if err := codex.WriteSession(target.PathSpace.ProjectRoot, target.NativeID, codexRecords.Records); err != nil {
		t.Fatalf("Codex WriteSession() error = %v", err)
	}
	codexRefs, err := codex.DiscoverSessions(target.PathSpace.ProjectRoot)
	if err != nil || len(codexRefs) != 1 || codexRefs[0].NativeID != target.NativeID {
		t.Fatalf("Codex discovery = refs %v, error %v", codexRefs, err)
	}
	codexRead, err := codex.ReadSession(codexRefs[0])
	if err != nil {
		t.Fatalf("Codex ReadSession() error = %v", err)
	}
	if err := codex.ValidateMaterialized(context.Background(), codexRead.Records, target); err != nil {
		t.Fatalf("Codex readback validation error = %v", err)
	}
}

func TestMaterializeRejectsMalformedLinesAndUnsupportedLayouts(t *testing.T) {
	if _, err := (Layout{}).DecodeContext(context.Background(), [][]byte{[]byte(`{"type":`)}); !errors.Is(err, ErrMaterializeStructural) {
		t.Fatalf("malformed Claude input error = %v, want ErrMaterializeStructural", err)
	}
	if _, err := (CodexLayout{}).DecodeContext(context.Background(), [][]byte{[]byte("{\"type\":\"event_msg\"}\n")}); !errors.Is(err, ErrMaterializeStructural) {
		t.Fatalf("multiline Codex input error = %v, want ErrMaterializeStructural", err)
	}
	if _, err := MaterializeFor(nil); !errors.Is(err, ErrMaterializeUnsupportedLayout) {
		t.Fatalf("unsupported layout error = %v, want ErrMaterializeUnsupportedLayout", err)
	}
}

func TestMaterializeContextCancellationIsChecked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Layout{}).DecodeContext(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decode error = %v, want context.Canceled", err)
	}
	if _, err := (CodexLayout{}).NewSessionID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ID error = %v, want context.Canceled", err)
	}
}

func TestMaterializeTargetRequiresSafePathSpace(t *testing.T) {
	target := materializeTestTarget()
	target.PathSpace.AgentHome = ""
	view := ContextView{
		Version:     MaterializeViewVersion,
		SourceAgent: "codex",
		Items:       []ContextItem{{Kind: ContextItemUser, Text: "hello"}},
	}
	if _, err := (Layout{}).EncodeContext(context.Background(), view, target); !errors.Is(err, ErrMaterializeInvalidTarget) {
		t.Fatalf("empty Agent home error = %v, want ErrMaterializeInvalidTarget", err)
	}

	target = materializeTestTarget()
	target.PathSpace.ProjectRoot = "C:\\work\\project\nunsafe"
	if _, err := (CodexLayout{}).EncodeContext(context.Background(), view, target); !errors.Is(err, ErrMaterializeInvalidTarget) {
		t.Fatalf("newline project path error = %v, want ErrMaterializeInvalidTarget", err)
	}
}
