package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// These tests deliberately use the package name adapter rather than
// adapter_test.  Materialization is a safety boundary: exercising the small
// internal parsing and validation helpers directly makes it possible to cover
// malformed and fail-closed paths without creating real Agent files.

type materializeCoverageUnknownLayout struct{}

func (materializeCoverageUnknownLayout) Name() string { return "unknown" }

func (materializeCoverageUnknownLayout) Detect(context.Context) (Installation, error) {
	return Installation{}, nil
}

func (materializeCoverageUnknownLayout) DiscoverSessions(string) ([]SessionRef, error) {
	return nil, nil
}

func (materializeCoverageUnknownLayout) ReadSession(SessionRef) (SessionData, error) {
	return SessionData{}, nil
}

func (materializeCoverageUnknownLayout) WriteSession(string, string, [][]byte) error {
	return nil
}

func (materializeCoverageUnknownLayout) ReplaceSession(string, string, [][]byte) error {
	return nil
}

func (materializeCoverageUnknownLayout) TouchedFiles([][]byte, string) []FileAccess {
	return nil
}

// materializeCoverageContext lets tests cancel between the repeated checks
// made by decode, encode, validation, and NewSessionID.
type materializeCoverageContext struct {
	calls    int
	cancelAt int
}

func (c *materializeCoverageContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *materializeCoverageContext) Done() <-chan struct{} { return nil }

func (c *materializeCoverageContext) Err() error {
	c.calls++
	if c.cancelAt > 0 && c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func (c *materializeCoverageContext) Value(any) any { return nil }

func materializeCoverageJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T) error = %v", value, err)
	}
	return data
}

func materializeCoverageTarget() MaterializeTarget {
	return MaterializeTarget{
		NativeID: "coverage-target",
		PathSpace: PathSpace{
			ProjectRoot: `C:\coverage\project`,
			AgentHome:   `C:\coverage\agent-home`,
		},
		CreatedAt: time.Date(2026, 8, 28, 1, 2, 3, 0, time.FixedZone("coverage", 8*60*60)),
	}
}

func materializeCoverageClaudeRecord(t *testing.T, recordType, role string, content any) []byte {
	t.Helper()
	return materializeCoverageJSON(t, map[string]any{
		"type":      recordType,
		"sessionId": "coverage-source",
		"timestamp": "2026-08-28T01:02:03Z",
		"cwd":       `C:\coverage\project`,
		"message": map[string]any{
			"role":    role,
			"content": content,
		},
	})
}

func materializeCoverageClaudeTargetRecord(t *testing.T, target MaterializeTarget, recordType, role string, content any, completed any) []byte {
	t.Helper()
	record := map[string]any{
		"type":      recordType,
		"sessionId": target.NativeID,
		"timestamp": "2026-08-28T01:02:03Z",
		"cwd":       target.PathSpace.ProjectRoot,
		"message": map[string]any{
			"role":    role,
			"content": content,
		},
	}
	if completed != nil {
		record["completed"] = completed
	}
	return materializeCoverageJSON(t, record)
}

func materializeCoverageCodexRecord(t *testing.T, recordType string, payload any, timestamp string) []byte {
	t.Helper()
	if timestamp == "" {
		timestamp = "2026-08-28T01:02:03Z"
	}
	return materializeCoverageJSON(t, map[string]any{
		"timestamp": timestamp,
		"type":      recordType,
		"payload":   payload,
	})
}

func materializeCoverageCodexMeta(t *testing.T, target MaterializeTarget) []byte {
	t.Helper()
	return materializeCoverageCodexRecord(t, "session_meta", map[string]any{
		"id":              target.NativeID,
		"session_id":      target.NativeID,
		"cwd":             target.PathSpace.ProjectRoot,
		"workspace_roots": []string{target.PathSpace.ProjectRoot},
		"timestamp":       "2026-08-28T01:02:03Z",
	}, "2026-08-28T01:02:03Z")
}

func materializeCoverageValidClaudeRecords(t *testing.T, target MaterializeTarget) [][]byte {
	t.Helper()
	return [][]byte{
		materializeCoverageClaudeTargetRecord(t, target, "user", "user", "hello", true),
		materializeCoverageClaudeTargetRecord(t, target, "assistant", "assistant", []any{
			map[string]any{"type": "text", "text": "response"},
		}, true),
		materializeCoverageClaudeTargetRecord(t, target, "notice", "assistant", "boundary", false),
	}
}

func materializeCoverageValidCodexRecords(t *testing.T, target MaterializeTarget) [][]byte {
	t.Helper()
	return [][]byte{
		materializeCoverageCodexMeta(t, target),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{
			"type":    "user_message",
			"message": "hello",
		}, "2026-08-28T01:02:04Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "response"},
			},
		}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{
			"type":      "notice",
			"message":   "boundary",
			"completed": false,
		}, "2026-08-28T01:02:06Z"),
	}
}

func materializeCoverageDecodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return value
}

func materializeCoverageMutateRecord(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	value := materializeCoverageDecodeMap(t, raw)
	mutate(value)
	return materializeCoverageJSON(t, value)
}

func materializeCoverageExpectError(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is(..., %v)", err, target)
	}
}

func TestMaterializeCoverageRoutingAndPrimitiveHelpers(t *testing.T) {
	t.Run("capability routing", func(t *testing.T) {
		var nilClaude *Layout
		var nilCodex *CodexLayout
		cases := []struct {
			name    string
			layout  SessionLayout
			check   func(MaterializeCapability) bool
			wantErr bool
		}{
			{name: "Claude value", layout: Layout{}, check: func(capability MaterializeCapability) bool {
				_, ok := capability.(Layout)
				return ok
			}},
			{name: "Claude pointer", layout: &Layout{}, check: func(capability MaterializeCapability) bool {
				_, ok := capability.(*Layout)
				return ok
			}},
			{name: "Codex value", layout: CodexLayout{}, check: func(capability MaterializeCapability) bool {
				_, ok := capability.(CodexLayout)
				return ok
			}},
			{name: "Codex pointer", layout: &CodexLayout{}, check: func(capability MaterializeCapability) bool {
				_, ok := capability.(*CodexLayout)
				return ok
			}},
			{name: "nil Claude pointer", layout: nilClaude, check: func(capability MaterializeCapability) bool {
				value, ok := capability.(*Layout)
				return ok && value == nil
			}},
			{name: "nil Codex pointer", layout: nilCodex, check: func(capability MaterializeCapability) bool {
				value, ok := capability.(*CodexLayout)
				return ok && value == nil
			}},
			{name: "nil interface", layout: nil, wantErr: true},
			{name: "unknown layout", layout: materializeCoverageUnknownLayout{}, wantErr: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				capability, err := MaterializeFor(tc.layout)
				if tc.wantErr {
					materializeCoverageExpectError(t, err, ErrMaterializeUnsupportedLayout)
					if capability != nil {
						t.Fatalf("capability = %T, want nil", capability)
					}
					return
				}
				if err != nil || capability == nil || !tc.check(capability) {
					t.Fatalf("MaterializeFor() = (%T, %v), unexpected capability", capability, err)
				}
			})
		}
		if capability, err := MaterializeCapabilityFor(Layout{}); err != nil || capability == nil {
			t.Fatalf("MaterializeCapabilityFor() = (%T, %v)", capability, err)
		}
	})

	t.Run("object and scalar readers", func(t *testing.T) {
		stringCases := []struct {
			name                   string
			object                 map[string]json.RawMessage
			wantValue              string
			wantPresent, wantValid bool
		}{
			{name: "missing", object: map[string]json.RawMessage{}, wantValid: true},
			{name: "null", object: map[string]json.RawMessage{"value": json.RawMessage("null")}, wantPresent: true},
			{name: "wrong type", object: map[string]json.RawMessage{"value": json.RawMessage("123")}, wantPresent: true},
			{name: "string", object: map[string]json.RawMessage{"value": json.RawMessage(`"text"`)}, wantValue: "text", wantPresent: true, wantValid: true},
		}
		for _, tc := range stringCases {
			t.Run("string/"+tc.name, func(t *testing.T) {
				value, present, valid := materializeString(tc.object, "value")
				if value != tc.wantValue || present != tc.wantPresent || valid != tc.wantValid {
					t.Fatalf("materializeString() = (%q, %v, %v), want (%q, %v, %v)", value, present, valid, tc.wantValue, tc.wantPresent, tc.wantValid)
				}
			})
		}

		boolCases := []struct {
			name                   string
			object                 map[string]json.RawMessage
			wantValue              bool
			wantPresent, wantValid bool
		}{
			{name: "missing", object: map[string]json.RawMessage{}, wantValid: true},
			{name: "null", object: map[string]json.RawMessage{"value": json.RawMessage("null")}, wantPresent: true},
			{name: "wrong type", object: map[string]json.RawMessage{"value": json.RawMessage(`"true"`)}, wantPresent: true},
			{name: "true", object: map[string]json.RawMessage{"value": json.RawMessage("true")}, wantValue: true, wantPresent: true, wantValid: true},
		}
		for _, tc := range boolCases {
			t.Run("bool/"+tc.name, func(t *testing.T) {
				value, present, valid := materializeBool(tc.object, "value")
				if value != tc.wantValue || present != tc.wantPresent || valid != tc.wantValid {
					t.Fatalf("materializeBool() = (%v, %v, %v), want (%v, %v, %v)", value, present, valid, tc.wantValue, tc.wantPresent, tc.wantValid)
				}
			})
		}

		objectCases := []struct {
			name  string
			raw   json.RawMessage
			valid bool
		}{
			{name: "nil", raw: nil},
			{name: "whitespace", raw: json.RawMessage("  ")},
			{name: "null", raw: json.RawMessage("null")},
			{name: "array", raw: json.RawMessage("[]")},
			{name: "invalid", raw: json.RawMessage("{")},
			{name: "object", raw: json.RawMessage(`{"field":true}`), valid: true},
		}
		for _, tc := range objectCases {
			t.Run("object/"+tc.name, func(t *testing.T) {
				object, valid := materializeObject(tc.raw)
				if valid != tc.valid || valid && object == nil {
					t.Fatalf("materializeObject() = (%v, %v), want valid=%v", object, valid, tc.valid)
				}
			})
		}

		for _, tc := range []struct {
			name        string
			object      map[string]json.RawMessage
			wantValid   bool
			wantPresent bool
		}{
			{name: "missing", object: map[string]json.RawMessage{}, wantValid: true},
			{name: "null", object: map[string]json.RawMessage{"timestamp": json.RawMessage("null")}, wantPresent: true},
			{name: "empty", object: map[string]json.RawMessage{"timestamp": json.RawMessage(`""`)}, wantPresent: true},
			{name: "bad", object: map[string]json.RawMessage{"timestamp": json.RawMessage(`"not-time"`)}, wantPresent: true},
			{name: "good", object: map[string]json.RawMessage{"timestamp": json.RawMessage(`"2026-08-28T01:02:03.123Z"`)}, wantPresent: true, wantValid: true},
		} {
			t.Run("timestamp/"+tc.name, func(t *testing.T) {
				value, present, valid := materializeTimestamp(tc.object, "timestamp")
				if present != tc.wantPresent || valid != tc.wantValid || (valid && present && value.IsZero()) {
					t.Fatalf("materializeTimestamp() = (%v, %v, %v), want present/valid=(%v, %v)", value, present, valid, tc.wantPresent, tc.wantValid)
				}
			})
		}

		rawStringObject := map[string]json.RawMessage{
			"first":  json.RawMessage(`"one"`),
			"second": json.RawMessage(`"two"`),
			"null":   json.RawMessage("null"),
		}
		if value, present, valid := materializeRawString(rawStringObject, "missing", "first"); value != "one" || !present || !valid {
			t.Fatalf("raw string fallback = (%q, %v, %v)", value, present, valid)
		}
		if value, present, valid := materializeRawString(rawStringObject, "null", "second"); value != "" || !present || valid {
			t.Fatalf("raw string invalid first value = (%q, %v, %v)", value, present, valid)
		}
		if value, present, valid := materializeRawString(rawStringObject, "missing"); value != "" || present || !valid {
			t.Fatalf("raw string missing = (%q, %v, %v)", value, present, valid)
		}
	})

	t.Run("record and outcome helpers", func(t *testing.T) {
		if _, err := parseMaterializeObject(nil, 0); !errors.Is(err, ErrMaterializeStructural) {
			t.Fatalf("empty parse error = %v", err)
		}
		if _, err := parseMaterializeObject([]byte("{}\n"), 1); !errors.Is(err, ErrMaterializeStructural) {
			t.Fatalf("newline parse error = %v", err)
		}
		if _, err := parseMaterializeObject([]byte("not-json"), 2); !errors.Is(err, ErrMaterializeStructural) {
			t.Fatalf("invalid parse error = %v", err)
		}
		if _, err := parseMaterializeObject([]byte("null"), 3); !errors.Is(err, ErrMaterializeStructural) {
			t.Fatalf("null parse error = %v", err)
		}
		if object, err := parseMaterializeObject([]byte(`{"ok":true}`), 4); err != nil || object == nil {
			t.Fatalf("object parse = (%v, %v)", object, err)
		}

		var merged materializeOutcome
		merged.merge(materializeOutcome{unsupported: true})
		merged.merge(materializeOutcome{filtered: true})
		if !merged.unsupported || !merged.filtered {
			t.Fatalf("merged outcome = %+v", merged)
		}
		view := ContextView{}
		applyMaterializeOutcome(&view, merged)
		if view.Unsupported != 1 || view.Filtered != 1 {
			t.Fatalf("applied outcome = %+v", view)
		}
		if added, truncated, safe := appendDecodedItem(&view, ContextItemUser, "hello", time.Time{}, 2, true); !added || truncated || !safe {
			t.Fatalf("valid append = (%v, %v, %v)", added, truncated, safe)
		}
		if added, truncated, safe := appendDecodedItem(&view, ContextItemUser, "", time.Time{}, 3, true); added || truncated || !safe {
			t.Fatalf("empty append = (%v, %v, %v)", added, truncated, safe)
		}
		if added, truncated, safe := appendDecodedItem(&view, ContextItemUser, "password=secret", time.Time{}, 4, true); added || truncated || safe {
			t.Fatalf("unsafe append = (%v, %v, %v)", added, truncated, safe)
		}
		appendIncompleteNotice(&view, time.Time{}, 5, "incomplete")
		if len(view.Items) != 2 || view.Items[1].Completed || view.Items[1].Kind != ContextItemNotice {
			t.Fatalf("notice append = %+v", view.Items)
		}
	})

	t.Run("text safety and time helpers", func(t *testing.T) {
		for _, marker := range []string{
			"-----BEGIN PRIVATE KEY-----",
			"-----BEGIN RSA PRIVATE KEY-----",
			"Authorization: bearer x",
			"Proxy-Authorization: x",
			"X-API-Key: x",
			"api-key=x",
			"api_key=x",
			"apikey=x",
			"access_token=x",
			"client_secret=x",
			"password=x",
			`{"authorization":"x"}`,
			`{"api_key":"x"}`,
			`{"api-key":"x"}`,
			`{"access_token":"x"}`,
			`{"client_secret":"x"}`,
			`{"password":"x"}`,
		} {
			if !materializeLooksSensitive(marker) {
				t.Errorf("materializeLooksSensitive(%q) = false", marker)
			}
		}
		if materializeLooksSensitive("ordinary visible text") {
			t.Fatal("ordinary text was considered sensitive")
		}
		for _, input := range []string{"", "  \t"} {
			text, truncated, safe := materializeTextSafe(input)
			if text != "" || truncated || !safe {
				t.Errorf("empty safe text = (%q, %v, %v)", text, truncated, safe)
			}
		}
		if text, truncated, safe := materializeTextSafe("password=secret"); text != "" || truncated || safe {
			t.Fatalf("sensitive safe text = (%q, %v, %v)", text, truncated, safe)
		}
		long := strings.Repeat("a", maxMaterializeTextBytes+17)
		text, truncated, safe := materializeTextSafe(long)
		if !truncated || !safe || len(text) != maxMaterializeTextBytes || !strings.HasSuffix(text, materializeTextSuffix) {
			t.Fatalf("bounded ASCII text = len %d, truncated=%v, safe=%v", len(text), truncated, safe)
		}
		unicodeText := strings.Repeat("界", maxMaterializeTextBytes)
		text, truncated, safe = materializeTextSafe(unicodeText)
		if !truncated || !safe || !utf8.ValidString(text) || !strings.HasSuffix(text, materializeTextSuffix) {
			t.Fatalf("bounded Unicode text = len %d, truncated=%v, safe=%v", len(text), truncated, safe)
		}
		if text, truncated := boundMaterializeText("short"); text != "short" || truncated {
			t.Fatalf("short bound = (%q, %v)", text, truncated)
		}
		if targetMaterializeTime(MaterializeTarget{}).IsZero() {
			t.Fatal("zero target time was not filled")
		}
		fixed := time.Date(2026, 8, 28, 1, 2, 3, 0, time.FixedZone("coverage", 8*60*60))
		if got := targetMaterializeTime(MaterializeTarget{CreatedAt: fixed}); !got.Equal(fixed) || got.Location() != time.UTC {
			t.Fatalf("fixed target time = %v", got)
		}
		if got := materializeTimestampString(time.Time{}, fixed); got != fixed.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("fallback timestamp = %q", got)
		}
		if got := materializeTimestampString(fixed, time.Time{}); got != fixed.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("explicit timestamp = %q", got)
		}
	})

	t.Run("context and target helpers", func(t *testing.T) {
		if !errors.Is(checkMaterializeContext(nil), ErrMaterializeContextRequired) {
			t.Fatal("nil context was accepted")
		}
		if err := checkMaterializeContext(context.Background()); err != nil {
			t.Fatalf("background context error = %v", err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if !errors.Is(checkMaterializeContext(cancelled), context.Canceled) {
			t.Fatal("cancelled context was accepted")
		}
		for _, id := range []string{"", ".", "..", strings.Repeat("x", 129), "has/slash", "has space", "has\x00nul"} {
			if safeMaterializeNativeID(id) {
				t.Errorf("safeMaterializeNativeID(%q) = true", id)
			}
		}
		for _, id := range []string{"a", "A-1_2.3", strings.Repeat("x", 128)} {
			if !safeMaterializeNativeID(id) {
				t.Errorf("safeMaterializeNativeID(%q) = false", id)
			}
		}
		badTargets := []MaterializeTarget{
			{NativeID: "bad/id", PathSpace: materializeCoverageTarget().PathSpace},
			{NativeID: "ok", PathSpace: PathSpace{ProjectRoot: "", AgentHome: "home"}},
			{NativeID: "ok", PathSpace: PathSpace{ProjectRoot: "project", AgentHome: "  "}},
			{NativeID: "ok", PathSpace: PathSpace{ProjectRoot: "project\nunsafe", AgentHome: "home"}},
			{NativeID: "ok", PathSpace: PathSpace{ProjectRoot: string([]byte{0xff}), AgentHome: "home"}},
			{NativeID: "ok", PathSpace: PathSpace{ProjectRoot: "project", AgentHome: "home\x00unsafe"}},
		}
		for _, target := range badTargets {
			if err := validateMaterializeTarget(target); !errors.Is(err, ErrMaterializeInvalidTarget) {
				t.Errorf("validateMaterializeTarget(%+v) = %v", target, err)
			}
		}
		if err := validateMaterializeTarget(materializeCoverageTarget()); err != nil {
			t.Fatalf("valid target error = %v", err)
		}
	})
}

func TestMaterializeCoverageSessionIDsAndCancellation(t *testing.T) {
	for _, capability := range []MaterializeCapability{Layout{}, &Layout{}, CodexLayout{}, &CodexLayout{}} {
		id, err := capability.NewSessionID(context.Background())
		if err != nil || len(id) != 36 || !safeMaterializeNativeID(id) {
			t.Fatalf("NewSessionID(%T) = (%q, %v)", capability, id, err)
		}
		if id[14] != '4' || id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' && id[19] != 'A' && id[19] != 'B' {
			t.Fatalf("NewSessionID(%T) has invalid UUID bits: %q", capability, id)
		}
	}
	ctx := &materializeCoverageContext{cancelAt: 2}
	if _, err := (Layout{}).NewSessionID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-random cancellation error = %v", err)
	}
}

func TestMaterializeCoverageClaudeDecodeBranches(t *testing.T) {
	pairedCall := map[string]any{"type": "tool_use", "id": "claude-paired", "name": "read_file", "input": map[string]any{}}
	pairedResult := map[string]any{"type": "tool_result", "tool_use_id": "claude-paired", "content": "file contents"}
	aliasCall := map[string]any{"type": "server_tool_use", "tool_use_id": "claude-alias", "name": "list", "input": map[string]any{}}
	aliasResult := map[string]any{"type": "tool_output", "call_id": "claude-alias", "output": []any{map[string]any{"type": "output_text", "text": "items"}}}
	resultWithError := map[string]any{"type": "function_result", "id": "claude-error", "content": "failure", "is_error": true}
	errorCall := map[string]any{"type": "tool_call", "id": "claude-error", "tool": "run", "input": true}

	records := [][]byte{
		materializeCoverageClaudeRecord(t, "user", "user", "plain user"),
		materializeCoverageClaudeRecord(t, "assistant", "assistant", "plain assistant"),
		materializeCoverageClaudeRecord(t, "assistant", "assistant", []any{
			map[string]any{"type": "thinking", "thinking": "hidden"},
			map[string]any{"type": "redacted_thinking"},
			map[string]any{"type": "reasoning", "text": "hidden"},
			map[string]any{"type": "text", "text": "visible"},
			map[string]any{"type": "text"},
			nil,
			map[string]any{},
			map[string]any{"type": "mystery", "text": "unsupported"},
		}),
		materializeCoverageClaudeRecord(t, "assistant", "assistant", []any{pairedCall, aliasCall, errorCall}),
		materializeCoverageClaudeRecord(t, "user", "user", []any{pairedResult, aliasResult, resultWithError}),
		materializeCoverageClaudeRecord(t, "tool", "tool", []any{map[string]any{"type": "tool_result", "id": "claude-paired", "content": []any{map[string]any{"type": "text", "text": "tool"}}}}),
		materializeCoverageClaudeRecord(t, "notice", "assistant", "visible notice"),
		materializeCoverageClaudeRecord(t, "notice", "assistant", "password=hidden"),
		materializeCoverageClaudeRecord(t, "notice", "assistant", ""),
		materializeCoverageClaudeRecord(t, "notice", "assistant", nil),
		materializeCoverageClaudeRecord(t, "notice", "assistant", map[string]any{"not": "text"}),
		materializeCoverageClaudeRecord(t, "user", "user", nil),
		materializeCoverageClaudeRecord(t, "user", "user", []any{}),
		materializeCoverageClaudeRecord(t, "user", "user", 123),
		materializeCoverageClaudeRecord(t, "assistant", "assistant", []any{map[string]any{"type": "thinking"}}),
		materializeCoverageJSON(t, map[string]any{"type": "user", "message": map[string]any{"role": "user"}}),
		materializeCoverageJSON(t, map[string]any{"type": "user"}),
		materializeCoverageJSON(t, map[string]any{"type": "user", "message": nil}),
		materializeCoverageJSON(t, map[string]any{"type": "system", "message": map[string]any{"role": "system", "content": "prompt"}}),
		materializeCoverageJSON(t, map[string]any{"type": "assistant", "message": map[string]any{"role": "system", "content": "prompt"}}),
		materializeCoverageJSON(t, map[string]any{"type": "user", "isMeta": true, "message": map[string]any{"role": "user", "content": "meta"}}),
		materializeCoverageJSON(t, map[string]any{"type": "user", "isSidechain": true, "message": map[string]any{"role": "user", "content": "side"}}),
		materializeCoverageJSON(t, map[string]any{"type": "user", "isApiErrorMessage": true, "message": map[string]any{"role": "user", "content": "error"}}),
		materializeCoverageJSON(t, map[string]any{"type": "progress"}),
		materializeCoverageJSON(t, map[string]any{"type": "summary"}),
		materializeCoverageJSON(t, map[string]any{"type": "unknown"}),
		materializeCoverageJSON(t, map[string]any{"type": 3}),
		materializeCoverageJSON(t, map[string]any{}),
		materializeCoverageJSON(t, map[string]any{"type": ""}),
	}

	view, err := (Layout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	if view.Version != MaterializeViewVersion || view.SourceAgent != "claude-code" || view.SourceFormat != materializeClaudeFormat {
		t.Fatalf("Claude view metadata = %+v", view)
	}
	if len(view.Items) < 10 || view.Unsupported < 5 || view.Filtered < 8 {
		t.Fatalf("Claude branch counters/items = %+v", view)
	}
	completedCalls := 0
	completedResults := 0
	for _, item := range view.Items {
		if item.Kind == ContextItemToolCall && item.Completed {
			completedCalls++
		}
		if item.Kind == ContextItemToolResult && item.Completed {
			completedResults++
		}
		if strings.Contains(strings.ToLower(item.Text), "hidden") || strings.Contains(item.Text, "password=") {
			t.Fatalf("unsafe text survived Claude decode: %+v", item)
		}
	}
	if completedCalls < 2 || completedResults < 2 {
		t.Fatalf("paired Claude tools = calls %d, results %d; items=%+v", completedCalls, completedResults, view.Items)
	}

	ctx := &materializeCoverageContext{cancelAt: 2}
	if _, err := (Layout{}).DecodeContext(ctx, records); !errors.Is(err, context.Canceled) {
		t.Fatalf("Claude mid-decode cancellation = %v", err)
	}
}

func TestMaterializeCoverageClaudeHelpersAndToolFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		object map[string]json.RawMessage
		want   bool
	}{
		{name: "system type", object: map[string]json.RawMessage{}, want: true},
		{name: "meta true", object: map[string]json.RawMessage{"isMeta": json.RawMessage("true")}, want: true},
		{name: "sidechain true", object: map[string]json.RawMessage{"isSidechain": json.RawMessage("true")}, want: true},
		{name: "api error true", object: map[string]json.RawMessage{"isApiErrorMessage": json.RawMessage("true")}, want: true},
		{name: "invalid flags and ordinary role", object: map[string]json.RawMessage{
			"isMeta":  json.RawMessage(`"true"`),
			"message": json.RawMessage(`{"role":"user"}`),
		}, want: true},
		{name: "system role", object: map[string]json.RawMessage{"message": json.RawMessage(`{"role":"system"}`)}, want: true},
		{name: "developer role", object: map[string]json.RawMessage{"message": json.RawMessage(`{"role":"developer"}`)}, want: true},
	} {
		t.Run("filtered/"+tc.name, func(t *testing.T) {
			if got := claudeFilteredRecord(tc.object, map[bool]string{true: "system", false: "user"}[tc.want]); got != tc.want {
				t.Fatalf("claudeFilteredRecord() = %v, want %v", got, tc.want)
			}
		})
	}

	view := ContextView{}
	for _, tc := range []struct {
		name      string
		raw       json.RawMessage
		want      materializeOutcome
		wantItems int
	}{
		{name: "null", raw: json.RawMessage("null"), want: materializeOutcome{filtered: true}},
		{name: "invalid object", raw: json.RawMessage("[]"), want: materializeOutcome{unsupported: true}},
		{name: "unsafe", raw: json.RawMessage(`"password=x"`), want: materializeOutcome{filtered: true}},
		{name: "empty", raw: json.RawMessage(`""`), want: materializeOutcome{filtered: true}},
		{name: "valid", raw: json.RawMessage(`"notice"`), wantItems: 1},
	} {
		t.Run("plain/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeClaudePlainContent(&view, tc.raw, ContextItemNotice, time.Time{}, 1)
			if got != tc.want {
				t.Fatalf("decodeClaudePlainContent() = %+v, want %+v", got, tc.want)
			}
			if len(view.Items) != before+tc.wantItems {
				t.Fatalf("items = %d, want increase %d", len(view.Items), tc.wantItems)
			}
		})
	}

	paired := map[string]struct{}{"paired": {}}
	toolCallCases := []struct {
		name            string
		block           map[string]json.RawMessage
		wantUnsupported bool
		wantNotice      bool
	}{
		{name: "missing id", block: map[string]json.RawMessage{"name": json.RawMessage(`"read"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "bad id", block: map[string]json.RawMessage{"id": json.RawMessage("3"), "name": json.RawMessage(`"read"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "missing name", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsafe name", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "name": json.RawMessage(`"password=x"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "missing input", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "name": json.RawMessage(`"read"`)}, wantUnsupported: true, wantNotice: true},
		{name: "null input", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "name": json.RawMessage(`"read"`), "input": json.RawMessage("null")}, wantUnsupported: true, wantNotice: true},
		{name: "empty input", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "name": json.RawMessage(`"read"`), "input": json.RawMessage(" ")}, wantUnsupported: true, wantNotice: true},
		{name: "unpaired", block: map[string]json.RawMessage{"id": json.RawMessage(`"unpaired"`), "name": json.RawMessage(`"read"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "paired", block: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "name": json.RawMessage(`"read"`), "input": json.RawMessage(`{}`)}},
	}
	for _, tc := range toolCallCases {
		t.Run("tool-call/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeClaudeToolCall(&view, tc.block, time.Time{}, 2, paired)
			if got.unsupported != tc.wantUnsupported || (tc.wantNotice && len(view.Items) != before+1) {
				t.Fatalf("decodeClaudeToolCall() = %+v, items increased %d", got, len(view.Items)-before)
			}
		})
	}

	toolResultCases := []struct {
		name            string
		block           map[string]json.RawMessage
		wantUnsupported bool
		wantNotice      bool
	}{
		{name: "missing id", block: map[string]json.RawMessage{"content": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "missing output", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsafe output", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "content": json.RawMessage(`"password=x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "filtered block output", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "content": json.RawMessage(`[{"type":"thinking","text":"hidden"}]`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsupported block output", block: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "content": json.RawMessage(`[{"type":"image"}]`)}, wantUnsupported: true, wantNotice: true},
		{name: "unpaired", block: map[string]json.RawMessage{"id": json.RawMessage(`"unpaired"`), "content": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "empty paired result", block: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "content": json.RawMessage(`[]`)}},
		{name: "error result", block: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "content": json.RawMessage(`"x"`), "is_error": json.RawMessage("true")}},
		{name: "output fallback", block: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "output": json.RawMessage(`"x"`)}},
	}
	for _, tc := range toolResultCases {
		t.Run("tool-result/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeClaudeToolResult(&view, tc.block, time.Time{}, 3, paired)
			if got.unsupported != tc.wantUnsupported {
				t.Fatalf("decodeClaudeToolResult() = %+v, want unsupported=%v", got, tc.wantUnsupported)
			}
			if tc.wantNotice && len(view.Items) != before+1 {
				t.Fatalf("notice items increased %d, want 1", len(view.Items)-before)
			}
		})
	}

	for _, tc := range []struct {
		name     string
		raw      json.RawMessage
		present  bool
		accepted map[string]bool
		want     materializeContentValue
	}{
		{name: "missing", accepted: map[string]bool{"text": true}},
		{name: "null", raw: json.RawMessage("null"), present: true},
		{name: "plain", raw: json.RawMessage(`"text"`), present: true, accepted: map[string]bool{"text": true}, want: materializeContentValue{text: "text", valid: true}},
		{name: "sensitive plain", raw: json.RawMessage(`"password=x"`), present: true, accepted: map[string]bool{"text": true}, want: materializeContentValue{valid: true, sensitive: true}},
		{name: "invalid", raw: json.RawMessage("{"), present: true, accepted: map[string]bool{"text": true}},
		{name: "empty blocks", raw: json.RawMessage("[]"), present: true, accepted: map[string]bool{"text": true}, want: materializeContentValue{valid: true}},
		{name: "mixed blocks", raw: json.RawMessage(`[{"type":"thinking"},{"type":"text","text":"one"},{"type":"output_text","output":"two"},{"type":"image"},null,{"type":"text"}]`), present: true, accepted: map[string]bool{"text": true, "output_text": true}, want: materializeContentValue{text: "one\ntwo", valid: true, filtered: true, unsupported: true}},
		{name: "unsafe block", raw: json.RawMessage(`[{"type":"text","text":"password=x"}]`), present: true, accepted: map[string]bool{"text": true}, want: materializeContentValue{valid: true, sensitive: true}},
	} {
		t.Run("content/"+tc.name, func(t *testing.T) {
			got := readMaterializeContent(tc.raw, tc.present, tc.accepted)
			if got.text != tc.want.text || got.valid != tc.want.valid || got.sensitive != tc.want.sensitive || got.unsupported != tc.want.unsupported || got.filtered != tc.want.filtered {
				t.Fatalf("readMaterializeContent() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMaterializeCoverageCodexDecodeBranches(t *testing.T) {
	pairedCall := map[string]any{"type": "function_call", "call_id": "codex-paired", "name": "read_file", "arguments": map[string]any{}}
	pairedResult := map[string]any{"type": "function_call_output", "call_id": "codex-paired", "output": "file contents"}
	aliasCall := map[string]any{"type": "tool_use", "id": "codex-alias", "tool": "list", "input": true}
	aliasResult := map[string]any{"type": "tool_output", "id": "codex-alias", "result": []any{map[string]any{"type": "output_text", "text": "items"}}}
	nestedCall := map[string]any{"type": "tool_call", "tool_call_id": "codex-nested", "name": "run", "arguments": true}
	nestedResult := map[string]any{"type": "tool_result", "tool_call_id": "codex-nested", "message": "done"}

	records := [][]byte{
		materializeCoverageCodexRecord(t, "session_meta", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "turn_context", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "compacted", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "context_compacted", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "task_started", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "task_complete", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "turn_started", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "turn_complete", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "thread_started", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "response_started", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "response_completed", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "model_snapshot", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "system", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "developer", map[string]any{}, "2026-08-28T01:02:03Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "user_message", "message": "user"}, "2026-08-28T01:02:04Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "user", "text": "user alias"}, "2026-08-28T01:02:04Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "input", "message": "input"}, "2026-08-28T01:02:04Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "agent_message", "message": "assistant"}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "assistant_message", "text": "assistant alias"}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "assistant", "message": "assistant"}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "output", "message": "output"}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "response", "message": "response"}, "2026-08-28T01:02:05Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "warning", "message": "warning"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "error", "text": "error"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "notice", "message": "notice"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "reasoning", "message": "hidden"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "thinking", "message": "hidden"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "unknown", "message": "unsupported"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "user_message"}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "function_call", "call_id": "codex-unpaired", "name": "read", "arguments": true}, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", pairedCall, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", pairedResult, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", aliasCall, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", aliasResult, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", nestedCall, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "event_msg", nestedResult, "2026-08-28T01:02:06Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "message", "role": "user", "content": "response user"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "text"},
			map[string]any{"type": "output_text", "text": "output"},
			map[string]any{"type": "input_text", "text": "input"},
			map[string]any{"type": "refusal", "content": "refusal"},
			map[string]any{"type": "reasoning"},
			map[string]any{"type": "unsupported"},
			nil,
			map[string]any{"type": "text"},
		},
		}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "message", "role": "system", "content": "hidden"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "message", "role": "developer", "content": "hidden"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "message", "role": "other", "content": "unsupported"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"item": map[string]any{"type": "message", "role": "assistant", "content": "nested assistant"}}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"item": "not-an-object", "type": "reasoning"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "reasoning"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "thinking"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "refusal_reasoning"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "function_call", "call_id": "codex-nested", "name": "run", "arguments": true}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "codex-nested", "output": "done"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{"type": "unknown"}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "response_item", map[string]any{}, "2026-08-28T01:02:07Z"),
		materializeCoverageCodexRecord(t, "event_msg", map[string]any{"type": "notice", "message": "password=x"}, "not-a-time"),
		materializeCoverageCodexRecord(t, "unknown", map[string]any{}, "2026-08-28T01:02:08Z"),
		materializeCoverageJSON(t, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "no timestamp"}}),
		materializeCoverageJSON(t, map[string]any{"type": "event_msg"}),
		materializeCoverageJSON(t, map[string]any{"type": 3, "payload": map[string]any{}}),
		materializeCoverageJSON(t, map[string]any{}),
	}

	view, err := (CodexLayout{}).DecodeContext(context.Background(), records)
	if err != nil {
		t.Fatalf("DecodeContext() error = %v", err)
	}
	if view.Version != MaterializeViewVersion || view.SourceAgent != "codex" || view.SourceFormat != materializeCodexFormat {
		t.Fatalf("Codex view metadata = %+v", view)
	}
	if len(view.Items) < 20 || view.Unsupported < 6 || view.Filtered < 15 {
		t.Fatalf("Codex branch counters/items = %+v", view)
	}
	ctx := &materializeCoverageContext{cancelAt: 2}
	if _, err := (CodexLayout{}).DecodeContext(ctx, records); !errors.Is(err, context.Canceled) {
		t.Fatalf("Codex mid-decode cancellation = %v", err)
	}
}

func TestMaterializeCoverageCodexHelpersAndToolFailures(t *testing.T) {
	if payload, ok := codexPayload(map[string]json.RawMessage{"payload": json.RawMessage(`{"type":"event"}`)}); !ok || payload == nil {
		t.Fatalf("valid codexPayload = (%v, %v)", payload, ok)
	}
	for _, object := range []map[string]json.RawMessage{
		{},
		{"payload": json.RawMessage("null")},
		{"payload": json.RawMessage("[]")},
		{"payload": json.RawMessage("{")},
	} {
		if _, ok := codexPayload(object); ok {
			t.Fatalf("invalid codexPayload(%v) = ok", object)
		}
	}

	paired := map[string]struct{}{"paired": {}}
	view := ContextView{}
	toolCallCases := []struct {
		name            string
		payload         map[string]json.RawMessage
		wantUnsupported bool
		wantNotice      bool
	}{
		{name: "missing id", payload: map[string]json.RawMessage{"name": json.RawMessage(`"read"`), "arguments": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "bad id", payload: map[string]json.RawMessage{"id": json.RawMessage("3"), "name": json.RawMessage(`"read"`), "arguments": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "missing name", payload: map[string]json.RawMessage{"call_id": json.RawMessage(`"x"`), "arguments": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsafe name", payload: map[string]json.RawMessage{"call_id": json.RawMessage(`"x"`), "name": json.RawMessage(`"password=x"`), "arguments": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "input fallback", payload: map[string]json.RawMessage{"call_id": json.RawMessage(`"x"`), "tool": json.RawMessage(`"read"`), "input": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "unpaired", payload: map[string]json.RawMessage{"call_id": json.RawMessage(`"unpaired"`), "name": json.RawMessage(`"read"`), "arguments": json.RawMessage(`{}`)}, wantUnsupported: true, wantNotice: true},
		{name: "paired", payload: map[string]json.RawMessage{"call_id": json.RawMessage(`"paired"`), "name": json.RawMessage(`"read"`), "arguments": json.RawMessage(`{}`)}},
	}
	for _, tc := range toolCallCases {
		t.Run("tool-call/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeCodexToolCall(&view, tc.payload, time.Time{}, 1, paired)
			if got.unsupported != tc.wantUnsupported || tc.wantNotice && len(view.Items) != before+1 {
				t.Fatalf("decodeCodexToolCall() = %+v, item delta=%d", got, len(view.Items)-before)
			}
		})
	}

	toolResultCases := []struct {
		name            string
		payload         map[string]json.RawMessage
		wantUnsupported bool
		wantNotice      bool
	}{
		{name: "missing id", payload: map[string]json.RawMessage{"output": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "missing output", payload: map[string]json.RawMessage{"id": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsafe output", payload: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "output": json.RawMessage(`"password=x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "filtered output", payload: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "output": json.RawMessage(`[{"type":"reasoning"}]`)}, wantUnsupported: true, wantNotice: true},
		{name: "unsupported output", payload: map[string]json.RawMessage{"id": json.RawMessage(`"x"`), "output": json.RawMessage(`[{"type":"image"}]`)}, wantUnsupported: true, wantNotice: true},
		{name: "unpaired", payload: map[string]json.RawMessage{"id": json.RawMessage(`"unpaired"`), "output": json.RawMessage(`"x"`)}, wantUnsupported: true, wantNotice: true},
		{name: "empty paired", payload: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "output": json.RawMessage(`[]`)}},
		{name: "result fallback", payload: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "result": json.RawMessage(`"x"`)}},
		{name: "message fallback", payload: map[string]json.RawMessage{"id": json.RawMessage(`"paired"`), "message": json.RawMessage(`"x"`)}},
	}
	for _, tc := range toolResultCases {
		t.Run("tool-result/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeCodexToolResult(&view, tc.payload, time.Time{}, 2, paired)
			if got.unsupported != tc.wantUnsupported || tc.wantNotice && len(view.Items) != before+1 {
				t.Fatalf("decodeCodexToolResult() = %+v, item delta=%d", got, len(view.Items)-before)
			}
		})
	}

	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want materializeOutcome
	}{
		{name: "null", raw: json.RawMessage("null"), want: materializeOutcome{filtered: true}},
		{name: "plain", raw: json.RawMessage(`"text"`)},
		{name: "unsafe", raw: json.RawMessage(`"password=x"`), want: materializeOutcome{filtered: true}},
		{name: "invalid", raw: json.RawMessage("{"), want: materializeOutcome{unsupported: true}},
		{name: "empty", raw: json.RawMessage("[]"), want: materializeOutcome{filtered: true}},
		{name: "mixed", raw: json.RawMessage(`[{"type":"reasoning"},{"type":"text","text":"one"},{"type":"output_text","text":"two"},{"type":"input_text","content":"three"},{"type":"refusal","text":"four"},{"type":"image"},null,{"type":"text"}]`), want: materializeOutcome{filtered: true, unsupported: true}},
	} {
		t.Run("content/"+tc.name, func(t *testing.T) {
			before := len(view.Items)
			got := decodeCodexContent(&view, tc.raw, ContextItemAssistant, time.Time{}, 3)
			if got != tc.want {
				t.Fatalf("decodeCodexContent() = %+v, want %+v", got, tc.want)
			}
			if tc.name == "plain" && len(view.Items) != before+1 {
				t.Fatalf("plain content item delta = %d", len(view.Items)-before)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		payload map[string]json.RawMessage
		want    materializeOutcome
	}{
		{name: "message fallback", payload: map[string]json.RawMessage{"type": json.RawMessage(`"user_message"`), "message": json.RawMessage(`"hello"`)}},
		{name: "text fallback", payload: map[string]json.RawMessage{"type": json.RawMessage(`"user_message"`), "text": json.RawMessage(`"hello"`)}},
		{name: "missing fallback", payload: map[string]json.RawMessage{"type": json.RawMessage(`"user_message"`)}, want: materializeOutcome{filtered: true}},
		{name: "bad type", payload: map[string]json.RawMessage{}, want: materializeOutcome{unsupported: true}},
	} {
		t.Run("event-message/"+tc.name, func(t *testing.T) {
			got := decodeCodexEvent(&view, tc.payload, time.Time{}, 4, paired)
			if got != tc.want {
				t.Fatalf("decodeCodexEvent() = %+v, want %+v", got, tc.want)
			}
		})
	}

	responseItemCases := []struct {
		name    string
		payload map[string]json.RawMessage
		want    materializeOutcome
	}{
		{name: "missing type", payload: map[string]json.RawMessage{}, want: materializeOutcome{unsupported: true}},
		{name: "bad role", payload: map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"other"`)}, want: materializeOutcome{unsupported: true}},
		{name: "missing role", payload: map[string]json.RawMessage{"type": json.RawMessage(`"message"`)}, want: materializeOutcome{unsupported: true}},
		{name: "system role", payload: map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"system"`)}, want: materializeOutcome{filtered: true}},
		{name: "nested object", payload: map[string]json.RawMessage{"item": json.RawMessage(`{"type":"message","role":"user","content":"nested"}`)}},
		{name: "invalid nested item", payload: map[string]json.RawMessage{"item": json.RawMessage(`"not-object"`), "type": json.RawMessage(`"reasoning"`)}, want: materializeOutcome{filtered: true}},
	}
	for _, tc := range responseItemCases {
		t.Run("response-item/"+tc.name, func(t *testing.T) {
			got := decodeCodexResponseItem(&view, tc.payload, time.Time{}, 5, paired)
			if got != tc.want {
				t.Fatalf("decodeCodexResponseItem() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMaterializeCoverageEncodingAndViewValidation(t *testing.T) {
	target := materializeCoverageTarget()
	view := ContextView{
		Version:      MaterializeViewVersion,
		SourceAgent:  "synthetic-source",
		SourceFormat: "synthetic-jsonl",
		Unsupported:  2,
		Filtered:     3,
		Items: []ContextItem{
			{Kind: ContextItemUser, Text: "user\nmessage", Timestamp: time.Time{}, SourceIndex: 0, Completed: true},
			{Kind: ContextItemAssistant, Text: strings.Repeat("assistant", 5000), Timestamp: target.CreatedAt, SourceIndex: 1, Completed: true},
			{Kind: ContextItemAssistant, Text: "incomplete assistant", SourceIndex: 2, Completed: false},
			{Kind: ContextItemToolCall, Text: "read_file", SourceIndex: 3, Completed: true},
			{Kind: ContextItemToolResult, Text: "output", SourceIndex: 4, Completed: false},
			{Kind: ContextItemNotice, Text: "notice", SourceIndex: 5, Completed: false},
			{Kind: ContextItemNotice, Text: "password=should-be-redacted", SourceIndex: 6, Completed: true},
			{Kind: ContextItemNotice, Text: "", SourceIndex: 7, Completed: false},
		},
	}

	claude, err := (Layout{}).EncodeContext(context.Background(), view, target)
	if err != nil {
		t.Fatalf("Claude EncodeContext() error = %v", err)
	}
	if claude.SourceViewVersion != MaterializeViewVersion || claude.TargetAdapterVersion != ClaudeMaterializeAdapterVersion || claude.Stats.Unsupported != 2 || claude.Stats.Filtered != 3 || claude.Stats.Summarized < 5 || claude.Stats.Converted != len(view.Items) {
		t.Fatalf("Claude encoding metadata/stats = %+v", claude)
	}
	if err := (Layout{}).ValidateMaterialized(context.Background(), claude.Records, target); err != nil {
		t.Fatalf("Claude encoded validation error = %v", err)
	}
	if strings.Contains(string(claude.Records[len(claude.Records)-2]), "password=should-be-redacted") {
		t.Fatal("unsafe content appeared verbatim in Claude output")
	}

	codex, err := (CodexLayout{}).EncodeContext(context.Background(), view, MaterializeTarget{NativeID: "codex-target", PathSpace: target.PathSpace})
	if err != nil {
		t.Fatalf("Codex EncodeContext() error = %v", err)
	}
	if codex.SourceViewVersion != MaterializeViewVersion || codex.TargetAdapterVersion != CodexMaterializeAdapterVersion || codex.Stats.Unsupported != 2 || codex.Stats.Filtered != 3 || codex.Stats.Summarized < 5 || codex.Stats.Converted != len(view.Items) {
		t.Fatalf("Codex encoding metadata/stats = %+v", codex)
	}
	if err := (CodexLayout{}).ValidateMaterialized(context.Background(), codex.Records, MaterializeTarget{NativeID: "codex-target", PathSpace: target.PathSpace}); err != nil {
		t.Fatalf("Codex encoded validation error = %v", err)
	}

	for _, layout := range []MaterializeCapability{Layout{}, CodexLayout{}} {
		encoded, err := layout.EncodeContext(context.Background(), ContextView{Version: MaterializeViewVersion}, target)
		if err != nil || len(encoded.Records) == 0 {
			t.Fatalf("empty %T encoding = (%+v, %v)", layout, encoded, err)
		}
	}

	invalidViews := []ContextView{
		{Version: MaterializeViewVersion + 1},
		{Version: MaterializeViewVersion, Unsupported: -1},
		{Version: MaterializeViewVersion, Filtered: -1},
		{Version: MaterializeViewVersion, Items: []ContextItem{{Kind: ContextItemKind("unknown")}}},
		{Version: MaterializeViewVersion, Items: []ContextItem{{Kind: ContextItemUser, SourceIndex: -1}}},
		{Version: MaterializeViewVersion, Items: []ContextItem{{Kind: ContextItemUser, Completed: true}}},
	}
	for _, invalid := range invalidViews {
		if err := validateMaterializeView(invalid); !errors.Is(err, ErrMaterializeInvalidContext) {
			t.Errorf("validateMaterializeView(%+v) = %v", invalid, err)
		}
		if _, err := (Layout{}).EncodeContext(context.Background(), invalid, target); !errors.Is(err, ErrMaterializeInvalidContext) {
			t.Errorf("Claude invalid view error = %v", err)
		}
	}

	ctx := &materializeCoverageContext{cancelAt: 3}
	if _, err := (Layout{}).EncodeContext(ctx, view, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("Claude mid-encode cancellation = %v", err)
	}
	ctx = &materializeCoverageContext{cancelAt: 3}
	if _, err := (CodexLayout{}).EncodeContext(ctx, view, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("Codex mid-encode cancellation = %v", err)
	}
}

func TestMaterializeCoverageClaudeValidationFailures(t *testing.T) {
	target := materializeCoverageTarget()
	validUser := materializeCoverageClaudeTargetRecord(t, target, "user", "user", "hello", nil)
	validAssistant := materializeCoverageClaudeTargetRecord(t, target, "assistant", "assistant", []any{map[string]any{"type": "text", "text": "answer"}}, true)
	validNotice := materializeCoverageClaudeTargetRecord(t, target, "notice", "assistant", "notice", false)

	userCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing type", mutate: func(record map[string]any) { delete(record, "type") }},
		{name: "null type", mutate: func(record map[string]any) { record["type"] = nil }},
		{name: "unknown type", mutate: func(record map[string]any) { record["type"] = "tool" }},
		{name: "missing session id", mutate: func(record map[string]any) { delete(record, "sessionId") }},
		{name: "null session id", mutate: func(record map[string]any) { record["sessionId"] = nil }},
		{name: "wrong session id", mutate: func(record map[string]any) { record["sessionId"] = "other" }},
		{name: "missing cwd", mutate: func(record map[string]any) { delete(record, "cwd") }},
		{name: "wrong cwd", mutate: func(record map[string]any) { record["cwd"] = "other" }},
		{name: "missing timestamp", mutate: func(record map[string]any) { delete(record, "timestamp") }},
		{name: "bad timestamp", mutate: func(record map[string]any) { record["timestamp"] = "bad" }},
		{name: "bad completed", mutate: func(record map[string]any) { record["completed"] = "yes" }},
		{name: "incomplete user", mutate: func(record map[string]any) { record["completed"] = false }},
		{name: "missing message", mutate: func(record map[string]any) { delete(record, "message") }},
		{name: "null message", mutate: func(record map[string]any) { record["message"] = nil }},
		{name: "array message", mutate: func(record map[string]any) { record["message"] = []any{} }},
		{name: "missing role", mutate: func(record map[string]any) { delete(record["message"].(map[string]any), "role") }},
		{name: "null role", mutate: func(record map[string]any) { record["message"].(map[string]any)["role"] = nil }},
		{name: "wrong role", mutate: func(record map[string]any) { record["message"].(map[string]any)["role"] = "assistant" }},
		{name: "missing content", mutate: func(record map[string]any) { delete(record["message"].(map[string]any), "content") }},
		{name: "null content", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = nil }},
		{name: "empty content", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = "" }},
		{name: "wrong content", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = 3 }},
	}
	for _, tc := range userCases {
		t.Run("user/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, validUser, tc.mutate)
			if err := (Layout{}).ValidateMaterialized(context.Background(), [][]byte{record}, target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	assistantCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "invalid content", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = "not-blocks" }},
		{name: "empty blocks", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = []any{} }},
		{name: "null block", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = []any{nil} }},
		{name: "missing block type", mutate: func(record map[string]any) {
			record["message"].(map[string]any)["content"] = []any{map[string]any{"text": "x"}}
		}},
		{name: "wrong block type", mutate: func(record map[string]any) {
			record["message"].(map[string]any)["content"] = []any{map[string]any{"type": "thinking", "text": "x"}}
		}},
		{name: "missing block text", mutate: func(record map[string]any) {
			record["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text"}}
		}},
		{name: "empty block text", mutate: func(record map[string]any) {
			record["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": ""}}
		}},
		{name: "false assistant completion", mutate: func(record map[string]any) { record["completed"] = false }},
	}
	for _, tc := range assistantCases {
		t.Run("assistant/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, validAssistant, tc.mutate)
			if err := (Layout{}).ValidateMaterialized(context.Background(), [][]byte{record}, target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "notice wrong role", mutate: func(record map[string]any) { record["message"].(map[string]any)["role"] = "user" }},
		{name: "notice empty content", mutate: func(record map[string]any) { record["message"].(map[string]any)["content"] = "" }},
		{name: "notice completed false is allowed", mutate: func(record map[string]any) { record["completed"] = false }},
	} {
		t.Run("notice/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, validNotice, tc.mutate)
			err := (Layout{}).ValidateMaterialized(context.Background(), [][]byte{record}, target)
			if tc.name == "notice completed false is allowed" {
				if err != nil {
					t.Fatalf("ValidateMaterialized() error = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}
}

func TestMaterializeCoverageCodexValidationFailures(t *testing.T) {
	target := materializeCoverageTarget()
	valid := materializeCoverageValidCodexRecords(t, target)

	if err := validateMaterialized(context.Background(), nil, target, materializeClaude); !errors.Is(err, ErrMaterializeInvalidOutput) {
		t.Fatalf("empty Claude output = %v", err)
	}
	if err := validateMaterialized(context.Background(), valid, target, materializeLayoutKind(99)); !errors.Is(err, ErrMaterializeInvalidOutput) {
		t.Fatalf("unknown output kind = %v", err)
	}
	if err := validateMaterialized(context.Background(), valid[1:], target, materializeCodex); !errors.Is(err, ErrMaterializeInvalidOutput) {
		t.Fatalf("missing Codex metadata = %v", err)
	}
	if err := validateMaterialized(context.Background(), [][]byte{valid[1], valid[0]}, target, materializeCodex); !errors.Is(err, ErrMaterializeInvalidOutput) {
		t.Fatalf("metadata not first = %v", err)
	}

	globalCases := []struct {
		name    string
		records func() [][]byte
	}{
		{name: "empty line", records: func() [][]byte { return [][]byte{nil} }},
		{name: "newline", records: func() [][]byte { return [][]byte{append([]byte{}, append(valid[0], '\n')...)} }},
		{name: "malformed", records: func() [][]byte { return [][]byte{[]byte("{")} }},
		{name: "array record", records: func() [][]byte { return [][]byte{[]byte("[]")} }},
	}
	for _, tc := range globalCases {
		t.Run("global/"+tc.name, func(t *testing.T) {
			if err := (CodexLayout{}).ValidateMaterialized(context.Background(), tc.records(), target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	metaCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "bad id", mutate: func(record map[string]any) { record["payload"].(map[string]any)["id"] = "other" }},
		{name: "missing id", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "id") }},
		{name: "bad session id", mutate: func(record map[string]any) { record["payload"].(map[string]any)["session_id"] = "other" }},
		{name: "bad cwd", mutate: func(record map[string]any) { record["payload"].(map[string]any)["cwd"] = "other" }},
		{name: "missing roots", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "workspace_roots") }},
		{name: "bad roots type", mutate: func(record map[string]any) { record["payload"].(map[string]any)["workspace_roots"] = "root" }},
		{name: "empty roots", mutate: func(record map[string]any) { record["payload"].(map[string]any)["workspace_roots"] = []any{} }},
		{name: "wrong root", mutate: func(record map[string]any) { record["payload"].(map[string]any)["workspace_roots"] = []any{"other"} }},
		{name: "missing payload timestamp", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "timestamp") }},
		{name: "bad payload timestamp", mutate: func(record map[string]any) { record["payload"].(map[string]any)["timestamp"] = "bad" }},
	}
	for _, tc := range metaCases {
		t.Run("meta/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, valid[0], tc.mutate)
			if err := (CodexLayout{}).ValidateMaterialized(context.Background(), [][]byte{record}, target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	eventCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing payload", mutate: func(record map[string]any) { delete(record, "payload") }},
		{name: "null payload", mutate: func(record map[string]any) { record["payload"] = nil }},
		{name: "array payload", mutate: func(record map[string]any) { record["payload"] = []any{} }},
		{name: "missing event type", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "type") }},
		{name: "invalid event type", mutate: func(record map[string]any) { record["payload"].(map[string]any)["type"] = "warning" }},
		{name: "missing message", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "message") }},
		{name: "empty message", mutate: func(record map[string]any) { record["payload"].(map[string]any)["message"] = "" }},
		{name: "bad completed", mutate: func(record map[string]any) { record["payload"].(map[string]any)["completed"] = "yes" }},
	}
	for _, tc := range eventCases {
		t.Run("event/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, valid[1], tc.mutate)
			records := [][]byte{valid[0], record}
			if err := (CodexLayout{}).ValidateMaterialized(context.Background(), records, target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	responseCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "invalid response type", mutate: func(record map[string]any) { record["type"] = "unknown" }},
		{name: "missing response payload", mutate: func(record map[string]any) { delete(record, "payload") }},
		{name: "bad response payload", mutate: func(record map[string]any) { record["payload"] = []any{} }},
		{name: "missing response type", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "type") }},
		{name: "bad response kind", mutate: func(record map[string]any) { record["payload"].(map[string]any)["type"] = "function_call" }},
		{name: "bad role", mutate: func(record map[string]any) { record["payload"].(map[string]any)["role"] = "user" }},
		{name: "missing role", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "role") }},
		{name: "missing content", mutate: func(record map[string]any) { delete(record["payload"].(map[string]any), "content") }},
		{name: "empty content", mutate: func(record map[string]any) { record["payload"].(map[string]any)["content"] = []any{} }},
		{name: "bad content", mutate: func(record map[string]any) { record["payload"].(map[string]any)["content"] = "text" }},
		{name: "null block", mutate: func(record map[string]any) { record["payload"].(map[string]any)["content"] = []any{nil} }},
		{name: "bad block type", mutate: func(record map[string]any) {
			record["payload"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "x"}}
		}},
		{name: "missing block text", mutate: func(record map[string]any) {
			record["payload"].(map[string]any)["content"] = []any{map[string]any{"type": "output_text"}}
		}},
		{name: "empty block text", mutate: func(record map[string]any) {
			record["payload"].(map[string]any)["content"] = []any{map[string]any{"type": "output_text", "text": ""}}
		}},
	}
	for _, tc := range responseCases {
		t.Run("response/"+tc.name, func(t *testing.T) {
			record := materializeCoverageMutateRecord(t, valid[2], tc.mutate)
			records := [][]byte{valid[0], record}
			if err := (CodexLayout{}).ValidateMaterialized(context.Background(), records, target); !errors.Is(err, ErrMaterializeInvalidOutput) {
				t.Fatalf("ValidateMaterialized() = %v", err)
			}
		})
	}

	ctx := &materializeCoverageContext{cancelAt: 3}
	if err := (CodexLayout{}).ValidateMaterialized(ctx, valid, target); !errors.Is(err, context.Canceled) {
		t.Fatalf("Codex validation cancellation = %v", err)
	}
}
