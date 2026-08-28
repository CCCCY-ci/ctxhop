package syncflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
)

var (
	// ErrMaterializeCapability reports an adapter that cannot participate in
	// cross-Agent conversion.
	ErrMaterializeCapability = errors.New("syncflow: materialize capability is unavailable")

	// ErrMaterializeUnsupportedSource reports source records that the adapter
	// could not safely represent. A caller must opt in to a diagnostic-only
	// plan before these records may be omitted from target output.
	ErrMaterializeUnsupportedSource = errors.New("syncflow: source context contains unsupported records")

	// ErrInvalidMaterializeRequest reports a request that is unsafe to plan.
	ErrInvalidMaterializeRequest = errors.New("syncflow: invalid materialize request")

	// ErrInvalidMaterializePlan reports a plan that is incomplete or has been
	// changed after it was produced.
	ErrInvalidMaterializePlan = errors.New("syncflow: invalid materialize plan")
)

// MaterializeOptions describes one local, cross-Agent conversion.
//
// SourceSnapshot must be a complete snapshot returned by the source adapter.
// Incomplete tails and leniently skipped records are rejected before decoding.
// The records are copied before they are handed to the capability, and no
// function in this file writes them or contacts the remote store.
type MaterializeOptions struct {
	SourceAgent    string
	TargetAgent    string
	SourceSnapshot adapter.SessionData
	Target         adapter.MaterializeTarget

	// AllowUnsupported permits a caller that is explicitly producing a
	// diagnostic/preview result to continue after the source adapter reports
	// unsupported records. Apply code should leave this false so an incomplete
	// conversion cannot be installed accidentally.
	AllowUnsupported bool
}

// MaterializePlan is the result of a read-only conversion plan.
//
// EncodedRecords are target-native records held in memory for a later apply
// phase. They are deliberately not printed by command code. Source session
// records, credentials, paths, and hidden reasoning are not included in the
// plan's diagnostic fields; adapters decide which safe context items survive
// conversion.
type MaterializePlan struct {
	SourceAgent          string
	SourceFormat         string
	TargetAgent          string
	TargetNativeID       string
	SourceViewVersion    int
	TargetAdapterVersion string
	ContextItems         int
	Stats                adapter.MaterializeStats
	EncodedRecords       [][]byte
}

// PlanMaterialize decodes a complete source snapshot into the adapter-neutral
// context view and encodes it as a new target-native session. It is strictly
// read-only: it does not access Agent files, modify a LocalBinding, invoke an
// Agent, or perform remote I/O.
func PlanMaterialize(ctx context.Context, sourceCap, targetCap adapter.MaterializeCapability, options MaterializeOptions) (MaterializePlan, error) {
	if ctx == nil {
		return MaterializePlan{}, fmt.Errorf("%w: context is required", ErrInvalidMaterializeRequest)
	}
	if err := ctx.Err(); err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: %w", ErrInvalidMaterializeRequest, err)
	}
	if sourceCap == nil || targetCap == nil {
		return MaterializePlan{}, fmt.Errorf("%w: %w", ErrInvalidMaterializeRequest, ErrMaterializeCapability)
	}
	if err := validateMaterializeAgent(options.SourceAgent, "source"); err != nil {
		return MaterializePlan{}, err
	}
	if err := validateMaterializeAgent(options.TargetAgent, "target"); err != nil {
		return MaterializePlan{}, err
	}
	if options.SourceAgent == options.TargetAgent {
		return MaterializePlan{}, fmt.Errorf("%w: source and target Agent are the same", ErrInvalidMaterializeRequest)
	}
	if options.SourceSnapshot.Skipped != 0 {
		return MaterializePlan{}, fmt.Errorf("%w: source snapshot skipped %d record(s)", ErrInvalidMaterializeRequest, options.SourceSnapshot.Skipped)
	}
	if options.SourceSnapshot.DroppedTail {
		return MaterializePlan{}, fmt.Errorf("%w: source snapshot has an incomplete tail", ErrInvalidMaterializeRequest)
	}
	if len(options.SourceSnapshot.Records) == 0 {
		return MaterializePlan{}, fmt.Errorf("%w: source snapshot is empty", ErrInvalidMaterializeRequest)
	}
	if err := validateMaterializePathSpace(options.Target.PathSpace); err != nil {
		return MaterializePlan{}, err
	}

	sourceRecords := cloneMaterializeRecords(options.SourceSnapshot.Records)
	view, err := sourceCap.DecodeContext(ctx, sourceRecords)
	if err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: decode source context: %w", ErrMaterializeCapability, err)
	}
	if err := validateMaterializeView(view, options.SourceAgent); err != nil {
		return MaterializePlan{}, err
	}
	if err := adapter.ValidateContextView(view); err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: source context view: %v", ErrInvalidMaterializeRequest, err)
	}
	if err := validateMaterializeSourceIndexes(view, len(sourceRecords)); err != nil {
		return MaterializePlan{}, err
	}
	if view.Unsupported > 0 && !options.AllowUnsupported {
		return MaterializePlan{}, fmt.Errorf("%w: %d source record(s) omitted", ErrMaterializeUnsupportedSource, view.Unsupported)
	}

	target := options.Target
	if strings.TrimSpace(target.NativeID) == "" {
		target.NativeID, err = targetCap.NewSessionID(ctx)
		if err != nil {
			return MaterializePlan{}, fmt.Errorf("%w: allocate target session ID: %w", ErrMaterializeCapability, err)
		}
	}
	if err := validateMaterializeNativeID(target.NativeID); err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: target session ID: %v", ErrInvalidMaterializeRequest, err)
	}

	encoded, err := targetCap.EncodeContext(ctx, view, target)
	if err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: encode target context: %w", ErrMaterializeCapability, err)
	}
	if err := validateEncodedContext(encoded); err != nil {
		return MaterializePlan{}, err
	}
	validatedRecords := cloneMaterializeRecords(encoded.Records)
	if err := targetCap.ValidateMaterialized(ctx, validatedRecords, target); err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: validate target context: %w", ErrMaterializeCapability, err)
	}
	if err := ctx.Err(); err != nil {
		return MaterializePlan{}, fmt.Errorf("%w: %w", ErrInvalidMaterializeRequest, err)
	}

	stats := encoded.Stats
	return MaterializePlan{
		SourceAgent:          options.SourceAgent,
		SourceFormat:         view.SourceFormat,
		TargetAgent:          options.TargetAgent,
		TargetNativeID:       target.NativeID,
		SourceViewVersion:    view.Version,
		TargetAdapterVersion: encoded.TargetAdapterVersion,
		ContextItems:         len(view.Items),
		Stats:                stats,
		EncodedRecords:       validatedRecords,
	}, nil
}

// Validate checks the safe, in-memory invariants of a materialize plan. Apply
// code should call it immediately before any future filesystem transaction.
// Validation does not prove that an Agent will accept the records; the target
// capability's ValidateMaterialized method remains the authoritative format
// check for that purpose.
func (p MaterializePlan) Validate() error {
	if err := validateMaterializeAgent(p.SourceAgent, "source"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMaterializePlan, err)
	}
	if err := validateMaterializeAgent(p.TargetAgent, "target"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMaterializePlan, err)
	}
	if p.SourceAgent == p.TargetAgent {
		return fmt.Errorf("%w: source and target Agent are the same", ErrInvalidMaterializePlan)
	}
	if strings.TrimSpace(p.SourceFormat) == "" || !utf8.ValidString(p.SourceFormat) {
		return fmt.Errorf("%w: source format is invalid", ErrInvalidMaterializePlan)
	}
	if p.SourceViewVersion != adapter.MaterializeViewVersion {
		return fmt.Errorf("%w: unsupported source view version %d", ErrInvalidMaterializePlan, p.SourceViewVersion)
	}
	if strings.TrimSpace(p.TargetAdapterVersion) == "" || !utf8.ValidString(p.TargetAdapterVersion) {
		return fmt.Errorf("%w: target adapter version is invalid", ErrInvalidMaterializePlan)
	}
	if err := validateMaterializeNativeID(p.TargetNativeID); err != nil {
		return fmt.Errorf("%w: target session ID: %v", ErrInvalidMaterializePlan, err)
	}
	if p.ContextItems < 0 {
		return fmt.Errorf("%w: negative context item count", ErrInvalidMaterializePlan)
	}
	if err := validateMaterializeStats(p.Stats); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMaterializePlan, err)
	}
	if len(p.EncodedRecords) == 0 {
		return fmt.Errorf("%w: encoded target session is empty", ErrInvalidMaterializePlan)
	}
	for i, record := range p.EncodedRecords {
		if len(record) == 0 {
			return fmt.Errorf("%w: encoded record %d is empty", ErrInvalidMaterializePlan, i+1)
		}
		if !utf8.Valid(record) || strings.ContainsAny(string(record), "\r\n") {
			return fmt.Errorf("%w: encoded record %d is not a single valid line", ErrInvalidMaterializePlan, i+1)
		}
	}
	return nil
}

func validateMaterializeAgent(value, role string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed != value {
		return fmt.Errorf("%w: %s Agent must not have leading or trailing whitespace", ErrInvalidMaterializeRequest, role)
	}
	value = trimmed
	if value == "" {
		return fmt.Errorf("%w: %s Agent is empty", ErrInvalidMaterializeRequest, role)
	}
	if len(value) > 64 || !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s Agent is invalid", ErrInvalidMaterializeRequest, role)
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("%w: %s Agent contains an unsafe character", ErrInvalidMaterializeRequest, role)
	}
	return nil
}

func validateMaterializePathSpace(space adapter.PathSpace) error {
	for _, value := range []string{space.ProjectRoot, space.AgentHome} {
		if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
			return ErrInvalidPathSpace
		}
	}
	return nil
}

func validateMaterializeNativeID(value string) error {
	if value == "" || value == "." || value == ".." || len(value) > 128 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return errors.New("ID is empty or invalid")
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return errors.New("ID contains an unsafe character")
		}
	}
	return nil
}

func validateMaterializeView(view adapter.ContextView, sourceAgent string) error {
	if view.Version != adapter.MaterializeViewVersion {
		return fmt.Errorf("%w: unsupported source view version %d", ErrInvalidMaterializeRequest, view.Version)
	}
	if view.SourceAgent != sourceAgent {
		return fmt.Errorf("%w: source capability returned a different Agent", ErrInvalidMaterializeRequest)
	}
	sourceFormat := strings.TrimSpace(view.SourceFormat)
	if sourceFormat == "" || sourceFormat != view.SourceFormat || !utf8.ValidString(view.SourceFormat) {
		return fmt.Errorf("%w: source format is invalid", ErrInvalidMaterializeRequest)
	}
	if view.Unsupported < 0 || view.Filtered < 0 {
		return fmt.Errorf("%w: source view has a negative diagnostic counter", ErrInvalidMaterializeRequest)
	}
	for i, item := range view.Items {
		if !utf8.ValidString(item.Text) || strings.ContainsRune(item.Text, '\x00') {
			return fmt.Errorf("%w: context item %d contains unsafe text", ErrInvalidMaterializeRequest, i+1)
		}
		if item.SourceIndex < 0 {
			return fmt.Errorf("%w: context item %d has a negative source index", ErrInvalidMaterializeRequest, i+1)
		}
	}
	return nil
}

func validateMaterializeSourceIndexes(view adapter.ContextView, recordCount int) error {
	if recordCount <= 0 {
		return fmt.Errorf("%w: source record list is empty", ErrInvalidMaterializeRequest)
	}
	for index, item := range view.Items {
		if item.SourceIndex < 0 || item.SourceIndex >= recordCount {
			return fmt.Errorf("%w: context item %d refers to source record %d outside range", ErrInvalidMaterializeRequest, index+1, item.SourceIndex)
		}
	}
	return nil
}

func validateEncodedContext(encoded adapter.EncodedContext) error {
	if encoded.SourceViewVersion != adapter.MaterializeViewVersion {
		return fmt.Errorf("%w: target encoded an unsupported source view version %d", ErrInvalidMaterializeRequest, encoded.SourceViewVersion)
	}
	if strings.TrimSpace(encoded.TargetAdapterVersion) == "" || !utf8.ValidString(encoded.TargetAdapterVersion) {
		return fmt.Errorf("%w: target adapter version is invalid", ErrInvalidMaterializeRequest)
	}
	if err := validateMaterializeStats(encoded.Stats); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMaterializeRequest, err)
	}
	if len(encoded.Records) == 0 {
		return fmt.Errorf("%w: target encoded an empty session", ErrInvalidMaterializeRequest)
	}
	for i, record := range encoded.Records {
		if len(record) == 0 {
			return fmt.Errorf("%w: target encoded empty record %d", ErrInvalidMaterializeRequest, i+1)
		}
		if !utf8.Valid(record) || bytes.ContainsAny(record, "\r\n") {
			return fmt.Errorf("%w: target encoded record %d is not a single valid line", ErrInvalidMaterializeRequest, i+1)
		}
	}
	return nil
}

func validateMaterializeStats(stats adapter.MaterializeStats) error {
	if stats.Converted < 0 || stats.Summarized < 0 || stats.Unsupported < 0 || stats.Filtered < 0 {
		return errors.New("materialize statistics contain a negative value")
	}
	return nil
}

func cloneMaterializeRecords(records [][]byte) [][]byte {
	out := make([][]byte, len(records))
	for i, record := range records {
		out[i] = append([]byte(nil), record...)
	}
	return out
}
