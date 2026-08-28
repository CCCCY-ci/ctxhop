package adapter

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaterializeViewVersion is the version of the safe, transient context view
// exchanged by materialization adapters.
const MaterializeViewVersion = 1

// ClaudeMaterializeAdapterVersion identifies the minimal Claude Code encoder
// used by this package.
const ClaudeMaterializeAdapterVersion = "claude-code-materialize-v1"

// CodexMaterializeAdapterVersion identifies the minimal Codex encoder used by
// this package.
const CodexMaterializeAdapterVersion = "codex-materialize-v1"

const (
	maxMaterializeTextBytes = 32 << 10
	materializeTextSuffix   = "..."
	materializeClaudeFormat = "claude-code-jsonl"
	materializeCodexFormat  = "codex-jsonl"
)

var (
	// ErrMaterializeUnsupportedLayout reports a layout without a safe
	// materialization implementation.
	ErrMaterializeUnsupportedLayout = errors.New("adapter: materialization is unsupported for this layout")

	// ErrMaterializeUnsupported is an alias for
	// ErrMaterializeUnsupportedLayout.
	ErrMaterializeUnsupported = ErrMaterializeUnsupportedLayout

	// ErrMaterializeContextRequired reports a nil context passed to a
	// materialization operation.
	ErrMaterializeContextRequired = errors.New("adapter: materialization context is required")

	// ErrMaterializeStructural reports valid-JSON structural input that cannot
	// be interpreted as one JSONL object.
	ErrMaterializeStructural = errors.New("adapter: materialization input has invalid structure")

	// ErrMaterializeInvalidContext reports a context view that is not a version
	// and kind combination understood by the encoder.
	ErrMaterializeInvalidContext = errors.New("adapter: materialization context view is invalid")

	// ErrMaterializeInvalidTarget reports a target whose native identity or
	// path space is not safe for materialization.
	ErrMaterializeInvalidTarget = errors.New("adapter: materialization target is invalid")

	// ErrMaterializeInvalidOutput reports records that are not valid target
	// native JSONL.
	ErrMaterializeInvalidOutput = errors.New("adapter: materialized output is invalid")
)

// ContextItemKind identifies a safe, visible semantic in a context view.
type ContextItemKind string

const (
	// ContextItemUser is visible user-authored text.
	ContextItemUser ContextItemKind = "user"
	// ContextItemAssistant is visible assistant-authored text.
	ContextItemAssistant ContextItemKind = "assistant"
	// ContextItemToolCall is the safe summary of a tool invocation.
	ContextItemToolCall ContextItemKind = "tool-call"
	// ContextItemToolResult is the safe summary of a tool result.
	ContextItemToolResult ContextItemKind = "tool-result"
	// ContextItemNotice is an adapter or safety notice, including a marked
	// incomplete item.
	ContextItemNotice ContextItemKind = "notice"
)

const (
	// ContextItemKindUser is an alias for ContextItemUser.
	ContextItemKindUser = ContextItemUser
	// ContextItemKindAssistant is an alias for ContextItemAssistant.
	ContextItemKindAssistant = ContextItemAssistant
	// ContextItemKindToolCall is an alias for ContextItemToolCall.
	ContextItemKindToolCall = ContextItemToolCall
	// ContextItemKindToolResult is an alias for ContextItemToolResult.
	ContextItemKindToolResult = ContextItemToolResult
	// ContextItemKindNotice is an alias for ContextItemNotice.
	ContextItemKindNotice = ContextItemNotice
)

// ContextItem is one safe visible semantic extracted from a source session.
// SourceIndex is zero-based and refers to the source record that contributed
// the item. Source records themselves are never retained in this type.
type ContextItem struct {
	Kind        ContextItemKind `json:"kind"`
	Text        string          `json:"text"`
	Timestamp   time.Time       `json:"timestamp"`
	SourceIndex int             `json:"sourceIndex"`
	Completed   bool            `json:"completed"`
}

// ContextView is the versioned, transient, source-neutral view used by a
// materialization encoder. Unsupported and Filtered count source records, not
// individual fields or bytes.
type ContextView struct {
	Version      int           `json:"version"`
	SourceAgent  string        `json:"sourceAgent"`
	SourceFormat string        `json:"sourceFormat"`
	Items        []ContextItem `json:"items"`
	Unsupported  int           `json:"unsupported"`
	Filtered     int           `json:"filtered"`
}

// MaterializeTarget describes the local native session that an encoder is
// allowed to create. NativeID must be a new safe target identifier selected by
// the caller, normally from NewSessionID.
type MaterializeTarget struct {
	NativeID  string    `json:"nativeId"`
	PathSpace PathSpace `json:"pathSpace"`
	CreatedAt time.Time `json:"createdAt"`
}

// MaterializeStats reports how a context view was represented in target
// records. Converted counts source-view items represented in output records;
// Summarized counts items represented through a notice, a bounded text value,
// or an incomplete marker.
type MaterializeStats struct {
	Converted   int `json:"converted"`
	Summarized  int `json:"summarized"`
	Unsupported int `json:"unsupported"`
	Filtered    int `json:"filtered"`
}

// EncodedContext is a validated target-native JSONL context and the
// provenance needed by a later materialization plan.
type EncodedContext struct {
	Records              [][]byte         `json:"records"`
	Stats                MaterializeStats `json:"stats"`
	SourceViewVersion    int              `json:"sourceViewVersion"`
	TargetAdapterVersion string           `json:"targetAdapterVersion"`
}

// MaterializeCapability is the optional adapter capability for turning a
// source session into a bounded visible context view and then encoding that
// view into a target-native session. Implementations do not perform file I/O,
// start an Agent, execute commands, invoke MCP, or run hooks.
type MaterializeCapability interface {
	DecodeContext(context.Context, [][]byte) (ContextView, error)
	NewSessionID(context.Context) (string, error)
	EncodeContext(context.Context, ContextView, MaterializeTarget) (EncodedContext, error)
	ValidateMaterialized(context.Context, [][]byte, MaterializeTarget) error
}

// MaterializeFor returns the safe materialization capability for a built-in
// layout. Unknown layouts fail closed with ErrMaterializeUnsupportedLayout;
// the helper never turns an unsupported layout into a successful no-op.
func MaterializeFor(layout SessionLayout) (MaterializeCapability, error) {
	switch value := layout.(type) {
	case Layout:
		return value, nil
	case *Layout:
		if value != nil {
			return value, nil
		}
	case CodexLayout:
		return value, nil
	case *CodexLayout:
		if value != nil {
			return value, nil
		}
	}
	if capability, ok := layout.(MaterializeCapability); ok && capability != nil {
		return capability, nil
	}
	return nil, ErrMaterializeUnsupportedLayout
}

// MaterializeCapabilityFor is a descriptive alias for MaterializeFor.
func MaterializeCapabilityFor(layout SessionLayout) (MaterializeCapability, error) {
	return MaterializeFor(layout)
}

// DecodeContext extracts safe visible Claude Code semantics. Metadata,
// prompts marked as meta, hidden reasoning, credentials, and unsupported
// structures are not emitted as source records. A malformed JSONL object is a
// structural error; safely unmappable records are counted and skipped.
func (l Layout) DecodeContext(ctx context.Context, records [][]byte) (ContextView, error) {
	return decodeClaudeContext(ctx, records)
}

// NewSessionID returns a cryptographically random UUID-shaped identifier that
// is safe as a native session filename for both built-in layouts.
func (l Layout) NewSessionID(ctx context.Context) (string, error) {
	return newMaterializeSessionID(ctx)
}

// EncodeContext renders a bounded visible view as minimal Claude Code JSONL.
// It uses only the target native ID and target project path from target.
func (l Layout) EncodeContext(ctx context.Context, view ContextView, target MaterializeTarget) (EncodedContext, error) {
	return encodeClaudeContext(ctx, view, target)
}

// ValidateMaterialized validates Claude Code JSONL in memory without writing
// files or starting Claude Code.
func (l Layout) ValidateMaterialized(ctx context.Context, records [][]byte, target MaterializeTarget) error {
	return validateMaterialized(ctx, records, target, materializeClaude)
}

// DecodeContext extracts safe visible Codex semantics. Session metadata,
// workspace metadata, hidden reasoning, credentials, and unsupported
// structures are not emitted as source records. A malformed JSONL object is a
// structural error; safely unmappable records are counted and skipped.
func (l CodexLayout) DecodeContext(ctx context.Context, records [][]byte) (ContextView, error) {
	return decodeCodexContext(ctx, records)
}

// NewSessionID returns a cryptographically random UUID-shaped identifier that
// is safe as a native session filename for both built-in layouts.
func (l CodexLayout) NewSessionID(ctx context.Context) (string, error) {
	return newMaterializeSessionID(ctx)
}

// EncodeContext renders a bounded visible view as minimal Codex JSONL with a
// target session_meta record followed by safe event or response records.
func (l CodexLayout) EncodeContext(ctx context.Context, view ContextView, target MaterializeTarget) (EncodedContext, error) {
	return encodeCodexContext(ctx, view, target)
}

// ValidateMaterialized validates Codex JSONL in memory without writing files
// or starting Codex.
func (l CodexLayout) ValidateMaterialized(ctx context.Context, records [][]byte, target MaterializeTarget) error {
	return validateMaterialized(ctx, records, target, materializeCodex)
}

type materializeLayoutKind uint8

const (
	materializeClaude materializeLayoutKind = iota + 1
	materializeCodex
)

type materializeOutcome struct {
	unsupported bool
	filtered    bool
}

func (o *materializeOutcome) merge(other materializeOutcome) {
	if other.unsupported {
		o.unsupported = true
	}
	if other.filtered {
		o.filtered = true
	}
}

func applyMaterializeOutcome(view *ContextView, outcome materializeOutcome) {
	if outcome.unsupported {
		view.Unsupported++
	}
	if outcome.filtered {
		view.Filtered++
	}
}

func checkMaterializeContext(ctx context.Context) error {
	if ctx == nil {
		return ErrMaterializeContextRequired
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("adapter: materialization: %w", err)
	}
	return nil
}

func parseMaterializeObject(raw []byte, index int) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.ContainsAny(raw, "\r\n") {
		return nil, fmt.Errorf("%w: record %d", ErrMaterializeStructural, index+1)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("%w: record %d: %w", ErrMaterializeStructural, index+1, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%w: record %d is not an object", ErrMaterializeStructural, index+1)
	}
	return object, nil
}

func materializeString(object map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, present := object[key]
	if !present {
		return "", false, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, false
	}
	return value, true, true
}

func materializeBool(object map[string]json.RawMessage, key string) (bool, bool, bool) {
	raw, present := object[key]
	if !present {
		return false, false, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, true, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, true, false
	}
	return value, true, true
}

func materializeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func materializeTimestamp(object map[string]json.RawMessage, key string) (time.Time, bool, bool) {
	value, present, valid := materializeString(object, key)
	if !present {
		return time.Time{}, false, true
	}
	if !valid || strings.TrimSpace(value) == "" {
		return time.Time{}, true, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, true, false
	}
	return timestamp, true, true
}

func materializeRawString(object map[string]json.RawMessage, keys ...string) (string, bool, bool) {
	for _, key := range keys {
		value, present, valid := materializeString(object, key)
		if present {
			return value, true, valid
		}
	}
	return "", false, true
}

func materializeTextSafe(text string) (string, bool, bool) {
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return "", false, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false, true
	}
	if materializeLooksSensitive(text) {
		return "", false, false
	}
	text, truncated := boundMaterializeText(text)
	return text, truncated, true
}

func boundMaterializeText(text string) (string, bool) {
	if len(text) <= maxMaterializeTextBytes {
		return text, false
	}

	budget := maxMaterializeTextBytes - len(materializeTextSuffix)
	var builder strings.Builder
	builder.Grow(maxMaterializeTextBytes)
	for _, r := range text {
		size := utf8.RuneLen(r)
		if size < 0 || builder.Len()+size > budget {
			break
		}
		builder.WriteRune(r)
	}
	return strings.TrimSpace(builder.String()) + materializeTextSuffix, true
}

func materializeLooksSensitive(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{
		"-----begin private key-----",
		"-----begin rsa private key-----",
		"authorization:",
		"authorization=",
		"proxy-authorization:",
		"proxy-authorization=",
		"cookie:",
		"set-cookie:",
		"cookie=",
		"x-api-key:",
		"x-api-key=",
		"x-auth-token:",
		"x-auth-token=",
		"x-access-token:",
		"x-access-token=",
		"set-cookie=",
		"api-key=",
		"api_key=",
		"apikey=",
		"api-key:",
		"api_key:",
		"apikey:",
		"apikey\"",
		"access_token=",
		"access_token:",
		"refresh_token=",
		"refresh_token:",
		"client_secret=",
		"client_secret:",
		"password=",
		"password:",
		"passwd=",
		"passwd:",
		"pwd=",
		"pwd:",
		"token=",
		"token:",
		"secret=",
		"secret:",
		"bearer ",
		"bearer:",
		"bearer=",
		"\"authorization\"",
		"\"api_key\"",
		"\"api-key\"",
		"\"access_token\"",
		"\"refresh_token\"",
		"\"client_secret\"",
		"\"password\"",
		"\"passwd\"",
		"\"pwd\"",
		"\"token\"",
		"token\"",
		"\"secret\"",
		"secret\"",
		"\"cookie\"",
		"cookie\"",
		"accesstoken",
		"refreshtoken",
		"clientsecret",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func appendDecodedItem(view *ContextView, kind ContextItemKind, text string, timestamp time.Time, sourceIndex int, completed bool) (bool, bool, bool) {
	text, truncated, safe := materializeTextSafe(text)
	if !safe || text == "" {
		return false, truncated, safe
	}
	view.Items = append(view.Items, ContextItem{
		Kind:        kind,
		Text:        text,
		Timestamp:   timestamp,
		SourceIndex: sourceIndex,
		Completed:   completed,
	})
	return true, truncated, true
}

func appendIncompleteNotice(view *ContextView, timestamp time.Time, sourceIndex int, text string) {
	view.Items = append(view.Items, ContextItem{
		Kind:        ContextItemNotice,
		Text:        text,
		Timestamp:   timestamp,
		SourceIndex: sourceIndex,
		Completed:   false,
	})
}

// collectClaudeToolPairs indexes only tool IDs that have both a call and a
// result in the complete source snapshot. The context view intentionally does
// not carry tool IDs, so this pass prevents a one-sided tool record from being
// presented as completed history while keeping the transient API small.
func collectClaudeToolPairs(records [][]byte) map[string]struct{} {
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for index, raw := range records {
		object, err := parseMaterializeObject(raw, index)
		if err != nil {
			continue
		}
		typeValue, present, valid := materializeString(object, "type")
		if !present || !valid || claudeFilteredRecord(object, typeValue) {
			continue
		}
		message, ok := materializeObject(object["message"])
		if !ok {
			continue
		}
		content, ok := message["content"]
		if !ok {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(content, &blocks) != nil {
			continue
		}
		for _, blockRaw := range blocks {
			block, ok := materializeObject(blockRaw)
			if !ok {
				continue
			}
			kind, present, valid := materializeString(block, "type")
			if !present || !valid {
				continue
			}
			switch kind {
			case "tool_use", "tool_call", "server_tool_use":
				if id, _, complete := completeClaudeToolCall(block); complete {
					calls[id] = struct{}{}
				}
			case "tool_result", "tool_output", "function_result":
				if id, _, complete := completeClaudeToolResult(block); complete {
					results[id] = struct{}{}
				}
			}
		}
	}
	paired := make(map[string]struct{})
	for id := range calls {
		if _, ok := results[id]; ok {
			paired[id] = struct{}{}
		}
	}
	return paired
}

func decodeClaudeContext(ctx context.Context, records [][]byte) (ContextView, error) {
	view := ContextView{
		Version:      MaterializeViewVersion,
		SourceAgent:  "claude-code",
		SourceFormat: materializeClaudeFormat,
		Items:        make([]ContextItem, 0),
	}
	pairedTools := collectClaudeToolPairs(records)
	for index, raw := range records {
		if err := checkMaterializeContext(ctx); err != nil {
			return ContextView{}, err
		}
		object, err := parseMaterializeObject(raw, index)
		if err != nil {
			return ContextView{}, err
		}
		outcome := decodeClaudeRecord(&view, object, index, pairedTools)
		applyMaterializeOutcome(&view, outcome)
	}
	if err := checkMaterializeContext(ctx); err != nil {
		return ContextView{}, err
	}
	return view, nil
}

func decodeClaudeRecord(view *ContextView, object map[string]json.RawMessage, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	typeValue, present, valid := materializeString(object, "type")
	if !present || !valid || strings.TrimSpace(typeValue) == "" {
		return materializeOutcome{unsupported: true}
	}

	if claudeFilteredRecord(object, typeValue) {
		return materializeOutcome{filtered: true}
	}
	timestamp, _, _ := materializeTimestamp(object, "timestamp")
	switch typeValue {
	case "user":
		return decodeClaudeMessage(view, object, ContextItemUser, timestamp, sourceIndex, pairedTools)
	case "assistant":
		return decodeClaudeMessage(view, object, ContextItemAssistant, timestamp, sourceIndex, pairedTools)
	case "tool":
		appendIncompleteNotice(view, timestamp, sourceIndex, "Legacy tool record omitted.")
		return materializeOutcome{unsupported: true}
	case "notice":
		return decodeClaudeMessage(view, object, ContextItemNotice, timestamp, sourceIndex, pairedTools)
	case "system", "developer", "progress", "summary", "file-history-snapshot", "queue-operation", "custom-title", "ai-title", "compact_boundary", "turn_duration", "last-prompt":
		return materializeOutcome{filtered: true}
	default:
		return materializeOutcome{unsupported: true}
	}
}

func claudeFilteredRecord(object map[string]json.RawMessage, typeValue string) bool {
	if typeValue == "system" || typeValue == "developer" {
		return true
	}
	for _, key := range []string{"isMeta", "isSidechain", "isApiErrorMessage"} {
		value, present, valid := materializeBool(object, key)
		if present && (!valid || value) {
			return true
		}
	}
	if messageRaw, ok := object["message"]; ok {
		if message, valid := materializeObject(messageRaw); valid {
			role, present, valid := materializeString(message, "role")
			if present && valid && (role == "system" || role == "developer") {
				return true
			}
		}
	}
	return false
}

func decodeClaudeMessage(view *ContextView, object map[string]json.RawMessage, kind ContextItemKind, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	messageRaw, present := object["message"]
	if !present {
		return materializeOutcome{unsupported: true}
	}
	message, valid := materializeObject(messageRaw)
	if !valid {
		return materializeOutcome{unsupported: true}
	}
	if role, present, valid := materializeString(message, "role"); present && (!valid || !claudeRoleMatches(kind, role)) {
		return materializeOutcome{unsupported: true}
	}
	content, present := message["content"]
	if !present {
		return materializeOutcome{filtered: true}
	}
	if kind == ContextItemNotice {
		return decodeClaudePlainContent(view, content, kind, timestamp, sourceIndex)
	}
	return decodeClaudeContent(view, content, kind, timestamp, sourceIndex, pairedTools)
}

func claudeRoleMatches(kind ContextItemKind, role string) bool {
	switch kind {
	case ContextItemUser:
		return role == "user"
	case ContextItemAssistant:
		return role == "assistant"
	case ContextItemNotice:
		return role == "assistant" || role == "user"
	default:
		return true
	}
}

func decodeClaudePlainContent(view *ContextView, raw json.RawMessage, kind ContextItemKind, timestamp time.Time, sourceIndex int) materializeOutcome {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return materializeOutcome{filtered: true}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return materializeOutcome{unsupported: true}
	}
	added, _, safe := appendDecodedItem(view, kind, text, timestamp, sourceIndex, true)
	if !safe {
		return materializeOutcome{filtered: true}
	}
	if !added {
		return materializeOutcome{filtered: true}
	}
	return materializeOutcome{}
}

func decodeClaudeContent(view *ContextView, raw json.RawMessage, kind ContextItemKind, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return materializeOutcome{filtered: true}
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		added, _, safe := appendDecodedItem(view, kind, plain, timestamp, sourceIndex, true)
		if !safe {
			return materializeOutcome{filtered: true}
		}
		if !added {
			return materializeOutcome{filtered: true}
		}
		return materializeOutcome{}
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return materializeOutcome{unsupported: true}
	}
	if len(blocks) == 0 {
		return materializeOutcome{filtered: true}
	}

	var outcome materializeOutcome
	added := false
	for _, blockRaw := range blocks {
		block, ok := materializeObject(blockRaw)
		if !ok {
			outcome.unsupported = true
			continue
		}
		blockType, present, valid := materializeString(block, "type")
		if !present || !valid || strings.TrimSpace(blockType) == "" {
			outcome.unsupported = true
			continue
		}

		switch blockType {
		case "text":
			text, present, valid := materializeString(block, "text")
			if !present || !valid {
				outcome.unsupported = true
				continue
			}
			itemAdded, _, safe := appendDecodedItem(view, kind, text, timestamp, sourceIndex, true)
			if !safe {
				outcome.filtered = true
				continue
			}
			added = added || itemAdded

		case "tool_use", "tool_call", "server_tool_use":
			toolOutcome := decodeClaudeToolCall(view, block, timestamp, sourceIndex, pairedTools)
			outcome.merge(toolOutcome)
			if !toolOutcome.unsupported && !toolOutcome.filtered {
				added = true
			}

		case "tool_result", "tool_output", "function_result":
			toolOutcome := decodeClaudeToolResult(view, block, timestamp, sourceIndex, pairedTools)
			outcome.merge(toolOutcome)
			if !toolOutcome.unsupported && !toolOutcome.filtered {
				added = true
			}

		case "thinking", "redacted_thinking", "reasoning":
			outcome.filtered = true

		default:
			outcome.unsupported = true
		}
	}
	if !added && !outcome.unsupported && !outcome.filtered {
		outcome.filtered = true
	}
	return outcome
}

func decodeClaudeToolCall(view *ContextView, block map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	id, cleanName, complete := completeClaudeToolCall(block)
	if !complete {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Incomplete tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	if _, paired := pairedTools[id]; !paired {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unpaired tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	added, _, safe := appendDecodedItem(view, ContextItemToolCall, "tool call: "+cleanName, timestamp, sourceIndex, true)
	if !safe || !added {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unsafe tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	return materializeOutcome{}
}

func decodeClaudeToolResult(view *ContextView, block map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	id, contentValue, complete := completeClaudeToolResult(block)
	if !complete {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Incomplete or unsafe tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	if _, paired := pairedTools[id]; !paired {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unpaired tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	text := contentValue.text
	if text == "" {
		text = "tool result"
	}
	if isError, present, valid := materializeBool(block, "is_error"); present && valid && isError {
		text = "tool result (error): " + text
	} else {
		text = "tool result: " + text
	}
	added, _, safe := appendDecodedItem(view, ContextItemToolResult, text, timestamp, sourceIndex, true)
	if !safe || !added {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unsafe tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	return materializeOutcome{}
}

func completeClaudeToolCall(block map[string]json.RawMessage) (string, string, bool) {
	id, idPresent, idValid := materializeRawString(block, "id", "tool_use_id", "tool_call_id")
	name, namePresent, nameValid := materializeRawString(block, "name", "tool")
	input, inputPresent := block["input"]
	trimmedInput := bytes.TrimSpace(input)
	inputComplete := inputPresent && len(trimmedInput) != 0 && !bytes.Equal(trimmedInput, []byte("null")) && json.Valid(input)
	cleanName, _, nameSafe := materializeTextSafe(name)
	complete := idPresent && idValid && strings.TrimSpace(id) != "" && namePresent && nameValid && strings.TrimSpace(cleanName) != "" && inputComplete && nameSafe
	return id, cleanName, complete
}

func completeClaudeToolResult(block map[string]json.RawMessage) (string, materializeContentValue, bool) {
	id, idPresent, idValid := materializeRawString(block, "tool_use_id", "tool_call_id", "call_id", "id")
	content, contentPresent := block["content"]
	if !contentPresent {
		content, contentPresent = block["output"]
	}
	contentValue := readMaterializeContent(content, contentPresent, map[string]bool{"text": true, "output_text": true})
	_, errorPresent, errorValid := materializeBool(block, "is_error")
	complete := idPresent && idValid && strings.TrimSpace(id) != "" && contentValue.valid && !contentValue.sensitive && !contentValue.unsupported && !contentValue.filtered && (!errorPresent || errorValid)
	return id, contentValue, complete
}

type materializeContentValue struct {
	text        string
	valid       bool
	sensitive   bool
	unsupported bool
	filtered    bool
}

func readMaterializeContent(raw json.RawMessage, present bool, accepted map[string]bool) materializeContentValue {
	if !present || len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return materializeContentValue{}
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		text, _, safe := materializeTextSafe(plain)
		return materializeContentValue{text: text, valid: true, sensitive: !safe}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return materializeContentValue{}
	}
	value := materializeContentValue{valid: true}
	parts := make([]string, 0, len(blocks))
	for _, blockRaw := range blocks {
		block, ok := materializeObject(blockRaw)
		if !ok {
			value.unsupported = true
			continue
		}
		blockType, present, valid := materializeString(block, "type")
		if !present || !valid {
			value.unsupported = true
			continue
		}
		if blockType == "thinking" || blockType == "redacted_thinking" || blockType == "reasoning" {
			value.filtered = true
			continue
		}
		if !accepted[blockType] {
			value.unsupported = true
			continue
		}
		text, present, valid := materializeRawString(block, "text", "content", "output")
		if !present || !valid {
			value.unsupported = true
			continue
		}
		parts = append(parts, text)
	}
	text, _, safe := materializeTextSafe(strings.Join(parts, "\n"))
	value.text = text
	value.sensitive = !safe
	return value
}

func decodeCodexContext(ctx context.Context, records [][]byte) (ContextView, error) {
	view := ContextView{
		Version:      MaterializeViewVersion,
		SourceAgent:  "codex",
		SourceFormat: materializeCodexFormat,
		Items:        make([]ContextItem, 0),
	}
	pairedTools := collectCodexToolPairs(records)
	for index, raw := range records {
		if err := checkMaterializeContext(ctx); err != nil {
			return ContextView{}, err
		}
		object, err := parseMaterializeObject(raw, index)
		if err != nil {
			return ContextView{}, err
		}
		outcome := decodeCodexRecord(&view, object, index, pairedTools)
		applyMaterializeOutcome(&view, outcome)
	}
	if err := checkMaterializeContext(ctx); err != nil {
		return ContextView{}, err
	}
	return view, nil
}

func decodeCodexRecord(view *ContextView, object map[string]json.RawMessage, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	typeValue, present, valid := materializeString(object, "type")
	if !present || !valid || strings.TrimSpace(typeValue) == "" {
		return materializeOutcome{unsupported: true}
	}
	if codexFilteredRecord(typeValue) {
		return materializeOutcome{filtered: true}
	}

	timestamp, _, timestampValid := materializeTimestamp(object, "timestamp")
	if !timestampValid {
		timestamp = time.Time{}
	}
	switch typeValue {
	case "session_meta", "turn_context", "compacted", "context_compacted", "task_started", "task_complete", "turn_started", "turn_complete", "thread_started", "response_started", "response_completed", "model_snapshot":
		return materializeOutcome{filtered: true}
	case "event_msg":
		payload, ok := codexPayload(object)
		if !ok {
			return materializeOutcome{unsupported: true}
		}
		return decodeCodexEvent(view, payload, timestamp, sourceIndex, pairedTools)
	case "response_item":
		payload, ok := codexPayload(object)
		if !ok {
			return materializeOutcome{unsupported: true}
		}
		return decodeCodexResponseItem(view, payload, timestamp, sourceIndex, pairedTools)
	default:
		return materializeOutcome{unsupported: true}
	}
}

func codexFilteredRecord(typeValue string) bool {
	switch typeValue {
	case "session_meta", "turn_context", "compacted", "context_compacted", "task_started", "task_complete", "turn_started", "turn_complete", "thread_started", "response_started", "response_completed", "model_snapshot", "system", "developer":
		return true
	default:
		return false
	}
}

func codexPayload(object map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	raw, ok := object["payload"]
	if !ok {
		return nil, false
	}
	return materializeObject(raw)
}

// collectCodexToolPairs performs the same completeness check for Codex's
// event and response envelopes. A call/result without a matching counterpart
// is represented only by an incomplete notice and never by a completed tool
// item.
func collectCodexToolPairs(records [][]byte) map[string]struct{} {
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for index, raw := range records {
		object, err := parseMaterializeObject(raw, index)
		if err != nil {
			continue
		}
		payload, ok := codexPayload(object)
		if !ok {
			continue
		}
		semantic := payload
		if item, ok := materializeObject(payload["item"]); ok {
			semantic = item
		}
		kind, present, valid := materializeString(semantic, "type")
		if !present || !valid {
			continue
		}
		switch kind {
		case "function_call", "tool_call", "tool_use":
			id, _, complete := completeCodexToolCall(semantic)
			if complete {
				calls[id] = struct{}{}
			}
		case "function_call_output", "tool_result", "tool_output":
			id, _, complete := completeCodexToolResult(semantic)
			if complete {
				results[id] = struct{}{}
			}
		default:
			continue
		}
	}
	paired := make(map[string]struct{})
	for id := range calls {
		if _, ok := results[id]; ok {
			paired[id] = struct{}{}
		}
	}
	return paired
}

func decodeCodexEvent(view *ContextView, payload map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	kind, present, valid := materializeString(payload, "type")
	if !present || !valid || strings.TrimSpace(kind) == "" {
		return materializeOutcome{unsupported: true}
	}
	switch kind {
	case "user_message", "user", "input":
		return decodeCodexMessage(view, payload, ContextItemUser, timestamp, sourceIndex)
	case "agent_message", "assistant_message", "assistant", "output", "response":
		return decodeCodexMessage(view, payload, ContextItemAssistant, timestamp, sourceIndex)
	case "warning", "error", "notice":
		text, present, valid := materializeRawString(payload, "message", "text")
		if !present || !valid {
			return materializeOutcome{filtered: true}
		}
		added, _, safe := appendDecodedItem(view, ContextItemNotice, text, timestamp, sourceIndex, true)
		if !safe {
			return materializeOutcome{filtered: true}
		}
		if !added {
			return materializeOutcome{filtered: true}
		}
		return materializeOutcome{}
	case "function_call", "tool_call", "tool_use":
		return decodeCodexToolCall(view, payload, timestamp, sourceIndex, pairedTools)
	case "function_call_output", "tool_result", "tool_output":
		return decodeCodexToolResult(view, payload, timestamp, sourceIndex, pairedTools)
	case "reasoning", "thinking":
		return materializeOutcome{filtered: true}
	default:
		return materializeOutcome{unsupported: true}
	}
}

func decodeCodexResponseItem(view *ContextView, payload map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	semantic := payload
	if itemRaw, present := payload["item"]; present {
		if item, ok := materializeObject(itemRaw); ok {
			semantic = item
		}
	}
	typeValue, present, valid := materializeString(semantic, "type")
	if !present || !valid || strings.TrimSpace(typeValue) == "" {
		return materializeOutcome{unsupported: true}
	}
	switch typeValue {
	case "message":
		role, present, valid := materializeString(semantic, "role")
		if !present || !valid {
			return materializeOutcome{unsupported: true}
		}
		switch role {
		case "user":
			return decodeCodexMessage(view, semantic, ContextItemUser, timestamp, sourceIndex)
		case "assistant":
			return decodeCodexMessage(view, semantic, ContextItemAssistant, timestamp, sourceIndex)
		case "system", "developer":
			return materializeOutcome{filtered: true}
		default:
			return materializeOutcome{unsupported: true}
		}
	case "function_call", "tool_call", "tool_use":
		return decodeCodexToolCall(view, semantic, timestamp, sourceIndex, pairedTools)
	case "function_call_output", "tool_result", "tool_output":
		return decodeCodexToolResult(view, semantic, timestamp, sourceIndex, pairedTools)
	case "reasoning", "thinking", "refusal_reasoning":
		return materializeOutcome{filtered: true}
	default:
		return materializeOutcome{unsupported: true}
	}
}

func decodeCodexMessage(view *ContextView, payload map[string]json.RawMessage, kind ContextItemKind, timestamp time.Time, sourceIndex int) materializeOutcome {
	content, present := payload["content"]
	if !present {
		text, textPresent, textValid := materializeRawString(payload, "message", "text")
		if !textPresent || !textValid {
			return materializeOutcome{filtered: true}
		}
		added, _, safe := appendDecodedItem(view, kind, text, timestamp, sourceIndex, true)
		if !safe {
			return materializeOutcome{filtered: true}
		}
		if !added {
			return materializeOutcome{filtered: true}
		}
		return materializeOutcome{}
	}
	return decodeCodexContent(view, content, kind, timestamp, sourceIndex)
}

func decodeCodexContent(view *ContextView, raw json.RawMessage, kind ContextItemKind, timestamp time.Time, sourceIndex int) materializeOutcome {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return materializeOutcome{filtered: true}
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		added, _, safe := appendDecodedItem(view, kind, plain, timestamp, sourceIndex, true)
		if !safe {
			return materializeOutcome{filtered: true}
		}
		if !added {
			return materializeOutcome{filtered: true}
		}
		return materializeOutcome{}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return materializeOutcome{unsupported: true}
	}
	if len(blocks) == 0 {
		return materializeOutcome{filtered: true}
	}

	var outcome materializeOutcome
	added := false
	for _, blockRaw := range blocks {
		block, ok := materializeObject(blockRaw)
		if !ok {
			outcome.unsupported = true
			continue
		}
		blockType, present, valid := materializeString(block, "type")
		if !present || !valid || strings.TrimSpace(blockType) == "" {
			outcome.unsupported = true
			continue
		}
		if blockType == "reasoning" || blockType == "thinking" {
			outcome.filtered = true
			continue
		}
		accepted := blockType == "text" || blockType == "output_text" || blockType == "input_text" || blockType == "refusal"
		if !accepted {
			outcome.unsupported = true
			continue
		}
		text, present, valid := materializeRawString(block, "text", "content")
		if !present || !valid {
			outcome.unsupported = true
			continue
		}
		itemAdded, _, safe := appendDecodedItem(view, kind, text, timestamp, sourceIndex, true)
		if !safe {
			outcome.filtered = true
			continue
		}
		added = added || itemAdded
	}
	if !added && !outcome.unsupported && !outcome.filtered {
		outcome.filtered = true
	}
	return outcome
}

func decodeCodexToolCall(view *ContextView, payload map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	id, cleanName, complete := completeCodexToolCall(payload)
	if !complete {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Incomplete tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	if _, paired := pairedTools[id]; !paired {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unpaired tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	added, _, safe := appendDecodedItem(view, ContextItemToolCall, "tool call: "+cleanName, timestamp, sourceIndex, true)
	if !safe || !added {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unsafe tool call omitted.")
		return materializeOutcome{unsupported: true}
	}
	return materializeOutcome{}
}

func decodeCodexToolResult(view *ContextView, payload map[string]json.RawMessage, timestamp time.Time, sourceIndex int, pairedTools map[string]struct{}) materializeOutcome {
	id, value, complete := completeCodexToolResult(payload)
	if !complete {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Incomplete or unsafe tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	if _, paired := pairedTools[id]; !paired {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unpaired tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	text := value.text
	if text == "" {
		text = "tool result"
	}
	added, _, safe := appendDecodedItem(view, ContextItemToolResult, "tool result: "+text, timestamp, sourceIndex, true)
	if !safe || !added {
		appendIncompleteNotice(view, timestamp, sourceIndex, "Unsafe tool result omitted.")
		return materializeOutcome{unsupported: true}
	}
	return materializeOutcome{}
}

func completeCodexToolCall(payload map[string]json.RawMessage) (string, string, bool) {
	id, idPresent, idValid := materializeRawString(payload, "call_id", "tool_call_id", "id")
	name, namePresent, nameValid := materializeRawString(payload, "name", "tool")
	arguments, argumentsPresent := payload["arguments"]
	if !argumentsPresent {
		arguments, argumentsPresent = payload["input"]
	}
	trimmedArguments := bytes.TrimSpace(arguments)
	argumentsComplete := argumentsPresent && len(trimmedArguments) != 0 && !bytes.Equal(trimmedArguments, []byte("null"))
	cleanName, _, nameSafe := materializeTextSafe(name)
	complete := idPresent && idValid && strings.TrimSpace(id) != "" && namePresent && nameValid && cleanName != "" && argumentsComplete && nameSafe
	return id, cleanName, complete
}

func completeCodexToolResult(payload map[string]json.RawMessage) (string, materializeContentValue, bool) {
	id, idPresent, idValid := materializeRawString(payload, "call_id", "tool_call_id", "id")
	output, outputPresent := payload["output"]
	if !outputPresent {
		output, outputPresent = payload["result"]
	}
	if !outputPresent {
		output, outputPresent = payload["message"]
	}
	value := readMaterializeContent(output, outputPresent, map[string]bool{"text": true, "output_text": true})
	complete := idPresent && idValid && strings.TrimSpace(id) != "" && value.valid && !value.sensitive && !value.unsupported && !value.filtered
	return id, value, complete
}

func newMaterializeSessionID(ctx context.Context) (string, error) {
	if err := checkMaterializeContext(ctx); err != nil {
		return "", err
	}
	var randomID [16]byte
	if _, err := cryptorand.Read(randomID[:]); err != nil {
		return "", fmt.Errorf("adapter: materialization session id: %w", err)
	}
	randomID[6] = (randomID[6] & 0x0f) | 0x40
	randomID[8] = (randomID[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(randomID[:])
	if err := checkMaterializeContext(ctx); err != nil {
		return "", err
	}
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:], nil
}

func validateMaterializeTarget(target MaterializeTarget) error {
	if !safeMaterializeNativeID(target.NativeID) {
		return fmt.Errorf("%w: native id is not safe", ErrMaterializeInvalidTarget)
	}
	for name, value := range map[string]string{
		"project":    target.PathSpace.ProjectRoot,
		"Agent home": target.PathSpace.AgentHome,
	} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%w: target %s path is not configured", ErrMaterializeInvalidTarget, name)
		}
	}
	return nil
}

func safeMaterializeNativeID(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func validateMaterializeView(view ContextView) error {
	if view.Version != MaterializeViewVersion || view.Unsupported < 0 || view.Filtered < 0 {
		return fmt.Errorf("%w: unsupported view version or counter", ErrMaterializeInvalidContext)
	}
	for _, item := range view.Items {
		switch item.Kind {
		case ContextItemUser, ContextItemAssistant, ContextItemToolCall, ContextItemToolResult, ContextItemNotice:
		default:
			return fmt.Errorf("%w: context item kind is not supported", ErrMaterializeInvalidContext)
		}
		if item.SourceIndex < 0 {
			return fmt.Errorf("%w: context item index is negative", ErrMaterializeInvalidContext)
		}
		if !utf8.ValidString(item.Text) || strings.ContainsRune(item.Text, '\x00') {
			return fmt.Errorf("%w: context item text is invalid", ErrMaterializeInvalidContext)
		}
		if strings.TrimSpace(item.Text) == "" && item.Completed {
			return fmt.Errorf("%w: completed context item has no text", ErrMaterializeInvalidContext)
		}
	}
	return nil
}

func targetMaterializeTime(target MaterializeTarget) time.Time {
	if target.CreatedAt.IsZero() {
		return time.Now().UTC()
	}
	return target.CreatedAt.UTC()
}

func materializeTimestampString(timestamp, fallback time.Time) string {
	if timestamp.IsZero() {
		timestamp = fallback
	}
	return timestamp.UTC().Format(time.RFC3339Nano)
}

type claudeMaterializeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type claudeMaterializeTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeMaterializeRecord struct {
	Type      string                   `json:"type"`
	SessionID string                   `json:"sessionId"`
	Timestamp string                   `json:"timestamp"`
	CWD       string                   `json:"cwd"`
	Message   claudeMaterializeMessage `json:"message"`
	Completed bool                     `json:"completed,omitempty"`
}

func encodeClaudeContext(ctx context.Context, view ContextView, target MaterializeTarget) (EncodedContext, error) {
	if err := checkMaterializeContext(ctx); err != nil {
		return EncodedContext{}, err
	}
	if err := validateMaterializeView(view); err != nil {
		return EncodedContext{}, err
	}
	if err := validateMaterializeTarget(target); err != nil {
		return EncodedContext{}, err
	}

	when := targetMaterializeTime(target)
	encoded := EncodedContext{
		Records:              make([][]byte, 0, len(view.Items)),
		SourceViewVersion:    view.Version,
		TargetAdapterVersion: ClaudeMaterializeAdapterVersion,
		Stats: MaterializeStats{
			Unsupported: view.Unsupported,
			Filtered:    view.Filtered,
		},
	}
	for _, item := range view.Items {
		if err := checkMaterializeContext(ctx); err != nil {
			return EncodedContext{}, err
		}
		text, truncated, safe := materializeTextSafe(item.Text)
		summarized := truncated
		if !safe {
			text = "Visible content omitted by safety policy."
			item.Completed = false
			item.Kind = ContextItemNotice
			summarized = true
		}
		if text == "" {
			text = "Incomplete context item omitted."
			item.Completed = false
			item.Kind = ContextItemNotice
			summarized = true
		}

		recordType := string(item.Kind)
		content := any(text)
		role := "assistant"
		switch item.Kind {
		case ContextItemUser:
			role = "user"
		case ContextItemAssistant:
			content = []claudeMaterializeTextBlock{{Type: "text", Text: text}}
		case ContextItemToolCall:
			recordType = "notice"
			text = "Tool call: " + text
			content = text
			summarized = true
		case ContextItemToolResult:
			recordType = "notice"
			text = "Tool result: " + text
			content = text
			summarized = true
		case ContextItemNotice:
			recordType = "notice"
			content = text
			summarized = true
		}
		if !item.Completed {
			recordType = "notice"
			role = "assistant"
			content = text
			summarized = true
		}
		record := claudeMaterializeRecord{
			Type:      recordType,
			SessionID: target.NativeID,
			Timestamp: materializeTimestampString(item.Timestamp, when),
			CWD:       target.PathSpace.ProjectRoot,
			Message: claudeMaterializeMessage{
				Role:    role,
				Content: content,
			},
			Completed: item.Completed,
		}
		data, err := json.Marshal(record)
		if err != nil {
			return EncodedContext{}, fmt.Errorf("adapter: encode Claude context record: %w", err)
		}
		encoded.Records = append(encoded.Records, data)
		encoded.Stats.Converted++
		if summarized {
			encoded.Stats.Summarized++
		}
	}
	if len(encoded.Records) == 0 {
		encoded.Records = append(encoded.Records, claudeNoticeRecord(target, when, "No transferable visible context was found.", true))
		encoded.Stats.Converted++
		encoded.Stats.Summarized++
	}
	if err := validateMaterialized(ctx, encoded.Records, target, materializeClaude); err != nil {
		return EncodedContext{}, fmt.Errorf("adapter: encode Claude context validation: %w", err)
	}
	return encoded, nil
}

func claudeNoticeRecord(target MaterializeTarget, when time.Time, text string, completed bool) []byte {
	record := claudeMaterializeRecord{
		Type:      "notice",
		SessionID: target.NativeID,
		Timestamp: materializeTimestampString(time.Time{}, when),
		CWD:       target.PathSpace.ProjectRoot,
		Message: claudeMaterializeMessage{
			Role:    "assistant",
			Content: text,
		},
		Completed: completed,
	}
	data, _ := json.Marshal(record)
	return data
}

type codexMaterializeRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   any    `json:"payload"`
}

type codexMaterializeSessionMeta struct {
	ID             string   `json:"id"`
	SessionID      string   `json:"session_id"`
	CWD            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Timestamp      string   `json:"timestamp"`
}

type codexMaterializeEventPayload struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	Completed *bool  `json:"completed,omitempty"`
}

type codexMaterializeResponsePayload struct {
	Type    string                      `json:"type"`
	Role    string                      `json:"role"`
	Content []codexMaterializeTextBlock `json:"content"`
}

type codexMaterializeTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func encodeCodexContext(ctx context.Context, view ContextView, target MaterializeTarget) (EncodedContext, error) {
	if err := checkMaterializeContext(ctx); err != nil {
		return EncodedContext{}, err
	}
	if err := validateMaterializeView(view); err != nil {
		return EncodedContext{}, err
	}
	if err := validateMaterializeTarget(target); err != nil {
		return EncodedContext{}, err
	}

	when := targetMaterializeTime(target)
	encoded := EncodedContext{
		Records:              make([][]byte, 0, len(view.Items)+1),
		SourceViewVersion:    view.Version,
		TargetAdapterVersion: CodexMaterializeAdapterVersion,
		Stats: MaterializeStats{
			Unsupported: view.Unsupported,
			Filtered:    view.Filtered,
		},
	}
	meta := codexMaterializeRecord{
		Timestamp: materializeTimestampString(time.Time{}, when),
		Type:      "session_meta",
		Payload: codexMaterializeSessionMeta{
			ID:             target.NativeID,
			SessionID:      target.NativeID,
			CWD:            target.PathSpace.ProjectRoot,
			WorkspaceRoots: []string{target.PathSpace.ProjectRoot},
			Timestamp:      materializeTimestampString(time.Time{}, when),
		},
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return EncodedContext{}, fmt.Errorf("adapter: encode Codex session metadata: %w", err)
	}
	encoded.Records = append(encoded.Records, metaData)

	for _, item := range view.Items {
		if err := checkMaterializeContext(ctx); err != nil {
			return EncodedContext{}, err
		}
		text, truncated, safe := materializeTextSafe(item.Text)
		summarized := truncated
		if !safe {
			text = "Visible content omitted by safety policy."
			item.Completed = false
			item.Kind = ContextItemNotice
			summarized = true
		}
		if text == "" {
			text = "Incomplete context item omitted."
			item.Completed = false
			item.Kind = ContextItemNotice
			summarized = true
		}
		if !item.Completed && (item.Kind == ContextItemUser || item.Kind == ContextItemAssistant) {
			item.Kind = ContextItemNotice
			summarized = true
		}
		var record codexMaterializeRecord
		timestamp := materializeTimestampString(item.Timestamp, when)
		switch item.Kind {
		case ContextItemUser:
			record = codexMaterializeRecord{
				Timestamp: timestamp,
				Type:      "event_msg",
				Payload:   codexMaterializeEventPayload{Type: "user_message", Message: text},
			}
		case ContextItemAssistant:
			record = codexMaterializeRecord{
				Timestamp: timestamp,
				Type:      "response_item",
				Payload: codexMaterializeResponsePayload{
					Type:    "message",
					Role:    "assistant",
					Content: []codexMaterializeTextBlock{{Type: "output_text", Text: text}},
				},
			}
		case ContextItemToolCall:
			record = codexNoticeRecord(timestamp, "Tool call: "+text, item.Completed)
			summarized = true
		case ContextItemToolResult:
			record = codexNoticeRecord(timestamp, "Tool result: "+text, item.Completed)
			summarized = true
		case ContextItemNotice:
			record = codexNoticeRecord(timestamp, text, item.Completed)
			summarized = true
		}
		data, err := json.Marshal(record)
		if err != nil {
			return EncodedContext{}, fmt.Errorf("adapter: encode Codex context record: %w", err)
		}
		encoded.Records = append(encoded.Records, data)
		encoded.Stats.Converted++
		if summarized {
			encoded.Stats.Summarized++
		}
	}
	if len(encoded.Records) == 1 {
		noticeData, err := json.Marshal(codexNoticeRecord(
			materializeTimestampString(time.Time{}, when),
			"No transferable visible context was found.",
			true,
		))
		if err != nil {
			return EncodedContext{}, fmt.Errorf("adapter: encode Codex empty-context notice: %w", err)
		}
		encoded.Records = append(encoded.Records, noticeData)
		encoded.Stats.Converted++
		encoded.Stats.Summarized++
	}
	if err := validateMaterialized(ctx, encoded.Records, target, materializeCodex); err != nil {
		return EncodedContext{}, fmt.Errorf("adapter: encode Codex context validation: %w", err)
	}
	return encoded, nil
}

func codexNoticeRecord(timestamp, text string, completed bool) codexMaterializeRecord {
	var marker *bool
	if !completed {
		value := false
		marker = &value
	}
	return codexMaterializeRecord{
		Timestamp: timestamp,
		Type:      "event_msg",
		Payload: codexMaterializeEventPayload{
			Type:      "notice",
			Message:   text,
			Completed: marker,
		},
	}
}

func validateMaterialized(ctx context.Context, records [][]byte, target MaterializeTarget, kind materializeLayoutKind) error {
	if err := checkMaterializeContext(ctx); err != nil {
		return err
	}
	if err := validateMaterializeTarget(target); err != nil {
		return err
	}
	if len(records) == 0 {
		return fmt.Errorf("%w: record list is empty", ErrMaterializeInvalidOutput)
	}
	switch kind {
	case materializeClaude:
		return validateClaudeMaterialized(ctx, records, target)
	case materializeCodex:
		return validateCodexMaterialized(ctx, records, target)
	default:
		return fmt.Errorf("%w: target adapter kind is unknown", ErrMaterializeInvalidOutput)
	}
}

func validateMaterializeRecord(ctx context.Context, raw []byte, index int) (map[string]json.RawMessage, error) {
	if err := checkMaterializeContext(ctx); err != nil {
		return nil, err
	}
	if len(raw) == 0 || bytes.ContainsAny(raw, "\r\n") {
		return nil, fmt.Errorf("%w: record %d is not one line", ErrMaterializeInvalidOutput, index+1)
	}
	object, err := parseMaterializeObject(raw, index)
	if err != nil {
		return nil, fmt.Errorf("%w: record %d: %w", ErrMaterializeInvalidOutput, index+1, err)
	}
	return object, nil
}

func validateRequiredString(object map[string]json.RawMessage, key string, nonEmpty bool) (string, error) {
	value, present, valid := materializeString(object, key)
	if !present || !valid || (nonEmpty && strings.TrimSpace(value) == "") {
		return "", fmt.Errorf("%w: required target field is invalid", ErrMaterializeInvalidOutput)
	}
	return value, nil
}

func validateRequiredTimestamp(object map[string]json.RawMessage) error {
	_, present, valid := materializeTimestamp(object, "timestamp")
	if !present || !valid {
		return fmt.Errorf("%w: target timestamp is invalid", ErrMaterializeInvalidOutput)
	}
	return nil
}

func validateClaudeMaterialized(ctx context.Context, records [][]byte, target MaterializeTarget) error {
	for index, raw := range records {
		object, err := validateMaterializeRecord(ctx, raw, index)
		if err != nil {
			return err
		}
		typeValue, err := validateRequiredString(object, "type", true)
		if err != nil {
			return err
		}
		if typeValue != "user" && typeValue != "assistant" && typeValue != "notice" {
			return fmt.Errorf("%w: Claude record kind is invalid", ErrMaterializeInvalidOutput)
		}
		sessionID, err := validateRequiredString(object, "sessionId", true)
		if err != nil || sessionID != target.NativeID {
			return fmt.Errorf("%w: Claude native id does not match target", ErrMaterializeInvalidOutput)
		}
		cwd, err := validateRequiredString(object, "cwd", true)
		if err != nil || cwd != target.PathSpace.ProjectRoot {
			return fmt.Errorf("%w: Claude target path does not match target", ErrMaterializeInvalidOutput)
		}
		if err := validateRequiredTimestamp(object); err != nil {
			return err
		}
		if completed, present, valid := materializeBool(object, "completed"); present && (!valid || (!completed && typeValue != "notice")) {
			return fmt.Errorf("%w: incomplete Claude content is not marked as a notice", ErrMaterializeInvalidOutput)
		}
		messageRaw, present := object["message"]
		if !present {
			return fmt.Errorf("%w: Claude message is missing", ErrMaterializeInvalidOutput)
		}
		message, ok := materializeObject(messageRaw)
		if !ok {
			return fmt.Errorf("%w: Claude message is invalid", ErrMaterializeInvalidOutput)
		}
		role, err := validateRequiredString(message, "role", true)
		if err != nil {
			return err
		}
		if typeValue == "user" && role != "user" || typeValue == "assistant" && role != "assistant" || typeValue == "notice" && role != "assistant" {
			return fmt.Errorf("%w: Claude message role is invalid", ErrMaterializeInvalidOutput)
		}
		content, present := message["content"]
		if !present {
			return fmt.Errorf("%w: Claude message content is missing", ErrMaterializeInvalidOutput)
		}
		if typeValue == "assistant" {
			var blocks []json.RawMessage
			if err := json.Unmarshal(content, &blocks); err != nil || len(blocks) == 0 {
				return fmt.Errorf("%w: Claude assistant content is invalid", ErrMaterializeInvalidOutput)
			}
			for _, blockRaw := range blocks {
				block, ok := materializeObject(blockRaw)
				if !ok {
					return fmt.Errorf("%w: Claude assistant text block is invalid", ErrMaterializeInvalidOutput)
				}
				blockType, err := validateRequiredString(block, "type", true)
				if err != nil || blockType != "text" {
					return fmt.Errorf("%w: Claude assistant block kind is invalid", ErrMaterializeInvalidOutput)
				}
				if _, err := validateRequiredString(block, "text", true); err != nil {
					return err
				}
			}
		} else {
			if _, err := validateJSONText(content); err != nil {
				return fmt.Errorf("%w: Claude text content is invalid", ErrMaterializeInvalidOutput)
			}
		}
	}
	return nil
}

func validateJSONText(raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("%w: text is null", ErrMaterializeInvalidOutput)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: text is empty", ErrMaterializeInvalidOutput)
	}
	return value, nil
}

func validateCodexMaterialized(ctx context.Context, records [][]byte, target MaterializeTarget) error {
	if len(records) == 0 {
		return fmt.Errorf("%w: Codex record list is empty", ErrMaterializeInvalidOutput)
	}
	metaCount := 0
	for index, raw := range records {
		object, err := validateMaterializeRecord(ctx, raw, index)
		if err != nil {
			return err
		}
		if err := validateRequiredTimestamp(object); err != nil {
			return err
		}
		typeValue, err := validateRequiredString(object, "type", true)
		if err != nil {
			return err
		}
		payloadRaw, present := object["payload"]
		if !present {
			return fmt.Errorf("%w: Codex payload is missing", ErrMaterializeInvalidOutput)
		}
		payload, ok := materializeObject(payloadRaw)
		if !ok {
			return fmt.Errorf("%w: Codex payload is invalid", ErrMaterializeInvalidOutput)
		}
		switch typeValue {
		case "session_meta":
			if index != 0 {
				return fmt.Errorf("%w: Codex session metadata is not first", ErrMaterializeInvalidOutput)
			}
			metaCount++
			id, err := validateRequiredString(payload, "id", true)
			if err != nil || id != target.NativeID {
				return fmt.Errorf("%w: Codex native id does not match target", ErrMaterializeInvalidOutput)
			}
			sessionID, err := validateRequiredString(payload, "session_id", true)
			if err != nil || sessionID != target.NativeID {
				return fmt.Errorf("%w: Codex session id does not match target", ErrMaterializeInvalidOutput)
			}
			cwd, err := validateRequiredString(payload, "cwd", true)
			if err != nil || cwd != target.PathSpace.ProjectRoot {
				return fmt.Errorf("%w: Codex target path does not match target", ErrMaterializeInvalidOutput)
			}
			rootsRaw, present := payload["workspace_roots"]
			var roots []string
			if !present || json.Unmarshal(rootsRaw, &roots) != nil || len(roots) == 0 || roots[0] != target.PathSpace.ProjectRoot {
				return fmt.Errorf("%w: Codex workspace root is invalid", ErrMaterializeInvalidOutput)
			}
			if _, present, valid := materializeTimestamp(payload, "timestamp"); !present || !valid {
				return fmt.Errorf("%w: Codex metadata timestamp is invalid", ErrMaterializeInvalidOutput)
			}
		case "event_msg":
			payloadType, err := validateRequiredString(payload, "type", true)
			if err != nil || (payloadType != "user_message" && payloadType != "notice") {
				return fmt.Errorf("%w: Codex event kind is invalid", ErrMaterializeInvalidOutput)
			}
			if _, err := validateRequiredString(payload, "message", true); err != nil {
				return err
			}
			if _, present, valid := materializeBool(payload, "completed"); present && !valid {
				return fmt.Errorf("%w: Codex completion marker is invalid", ErrMaterializeInvalidOutput)
			}
		case "response_item":
			payloadType, err := validateRequiredString(payload, "type", true)
			if err != nil || payloadType != "message" {
				return fmt.Errorf("%w: Codex response kind is invalid", ErrMaterializeInvalidOutput)
			}
			role, err := validateRequiredString(payload, "role", true)
			if err != nil || role != "assistant" {
				return fmt.Errorf("%w: Codex response role is invalid", ErrMaterializeInvalidOutput)
			}
			contentRaw, present := payload["content"]
			var blocks []json.RawMessage
			if !present || json.Unmarshal(contentRaw, &blocks) != nil || len(blocks) == 0 {
				return fmt.Errorf("%w: Codex response content is invalid", ErrMaterializeInvalidOutput)
			}
			for _, blockRaw := range blocks {
				block, ok := materializeObject(blockRaw)
				if !ok {
					return fmt.Errorf("%w: Codex response block is invalid", ErrMaterializeInvalidOutput)
				}
				blockType, err := validateRequiredString(block, "type", true)
				if err != nil || blockType != "output_text" {
					return fmt.Errorf("%w: Codex response block kind is invalid", ErrMaterializeInvalidOutput)
				}
				if _, err := validateRequiredString(block, "text", true); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%w: Codex record kind is invalid", ErrMaterializeInvalidOutput)
		}
	}
	if metaCount != 1 {
		return fmt.Errorf("%w: Codex session metadata is missing", ErrMaterializeInvalidOutput)
	}
	return nil
}
