package syncflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

const (
	materializeMultiSourceAgent  = "sessionhub-multi-source"
	materializeMultiSourceFormat = "sessionhub-multi-source-v1"
)

var (
	// ErrMaterializeSourceCapabilityMissing reports a selected source Agent for
	// which the caller did not provide a local adapter capability.
	ErrMaterializeSourceCapabilityMissing = errors.New("syncflow: source materialize capability is missing")

	// ErrMaterializePreview reports a selected source set that cannot produce a
	// complete, read-only cross-Agent preview.
	ErrMaterializePreview = errors.New("syncflow: invalid materialize preview")
)

// MaterializePreviewOptions supplies the local capabilities used by a
// multi-source materialization preview. SourceCapabilities is keyed by the
// source Agent name in each MaterializeRange. No option causes remote or
// filesystem I/O.
type MaterializePreviewOptions struct {
	SourceCapabilities map[string]adapter.MaterializeCapability
	TargetAgent        string
	TargetCapability   adapter.MaterializeCapability
	Target             adapter.MaterializeTarget
	AllowUnsupported   bool
}

// MaterializeSourceSummary describes one selected source range. Ranges stay
// separate in the summary even when they belong to the same Agent so the UI
// can explain exactly which Contribution and Replica supplied the preview.
type MaterializeSourceSummary struct {
	ContributionID string `json:"contributionId"`
	SourceAgent    string `json:"sourceAgent"`
	ReplicaID      string `json:"replicaId"`
	StartRecord    uint64 `json:"startRecord"`
	EndRecord      uint64 `json:"endRecord"`
	RecordCount    uint64 `json:"recordCount"`
	ContextItems   int    `json:"contextItems"`
	Unsupported    int    `json:"unsupported"`
	Filtered       int    `json:"filtered"`
	SourceFormat   string `json:"sourceFormat"`
}

// MaterializePreview is the in-memory result of combining selected
// Contribution ranges into a target-native session. EncodedRecords are kept
// for a later apply phase but are never written by this package or printed by
// the command layer.
type MaterializePreview struct {
	Coverage             sessionhub.Coverage        `json:"coverage"`
	SelectedHeads        []string                   `json:"selectedHeads,omitempty"`
	Sources              []MaterializeSourceSummary `json:"sources"`
	TargetAgent          string                     `json:"targetAgent"`
	TargetNativeID       string                     `json:"targetNativeId"`
	SourceViewVersion    int                        `json:"sourceViewVersion"`
	TargetAdapterVersion string                     `json:"targetAdapterVersion"`
	SelectedRecordCount  uint64                     `json:"selectedRecordCount"`
	ContextItems         int                        `json:"contextItems"`
	Stats                adapter.MaterializeStats   `json:"stats"`
	EncodedRecords       [][]byte                   `json:"-"`
}

// PlanMaterializePreview selects no new data. It decodes every verified
// source range with that range's owning Agent capability, annotates each
// transient context item with source provenance, combines the safe items in
// causal selection order, and validates the target encoding in memory.
//
// The source ranges and their records are copied before adapter calls. The
// original MaterializeSelection, graph, Replica bodies, Agent sessions and
// local bindings remain unchanged.
func PlanMaterializePreview(ctx context.Context, selection MaterializeSelection, options MaterializePreviewOptions) (MaterializePreview, error) {
	if ctx == nil {
		return MaterializePreview{}, fmt.Errorf("%w: context is required", ErrMaterializePreview)
	}
	if err := ctx.Err(); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializePreview, err)
	}
	if options.TargetCapability == nil {
		return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializePreview, ErrMaterializeCapability)
	}
	if err := validateMaterializeAgent(options.TargetAgent, "target"); err != nil {
		return MaterializePreview{}, err
	}
	if err := validateMaterializePathSpace(options.Target.PathSpace); err != nil {
		return MaterializePreview{}, err
	}
	if selection.Coverage.Incomplete {
		return MaterializePreview{}, fmt.Errorf("%w: selected contribution graph is incomplete: %s", ErrMaterializePreview, selection.Coverage.Reason)
	}
	if len(selection.Coverage.SelectedIDs) == 0 || len(selection.Ranges) == 0 || selection.SelectedRecordCount == 0 {
		return MaterializePreview{}, fmt.Errorf("%w: selected source ranges are empty", ErrMaterializePreview)
	}

	combined := adapter.ContextView{
		Version:      adapter.MaterializeViewVersion,
		SourceAgent:  materializeMultiSourceAgent,
		SourceFormat: materializeMultiSourceFormat,
		Items:        make([]adapter.ContextItem, 0),
	}
	summaries := make([]MaterializeSourceSummary, 0, len(selection.Ranges))
	seenAgents := make(map[string]struct{}, len(selection.Ranges))
	var selectedRecordCount uint64
	for rangeIndex, sourceRange := range selection.Ranges {
		if err := ctx.Err(); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializePreview, err)
		}
		if err := validateMaterializePreviewRange(sourceRange); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		capability, ok := options.SourceCapabilities[sourceRange.SourceAgent]
		if !ok || capability == nil {
			return MaterializePreview{}, fmt.Errorf("%w: Agent %q", ErrMaterializeSourceCapabilityMissing, sourceRange.SourceAgent)
		}
		seenAgents[sourceRange.SourceAgent] = struct{}{}
		if ^uint64(0)-selectedRecordCount < uint64(len(sourceRange.Records)) {
			return MaterializePreview{}, fmt.Errorf("%w: selected record count overflows", ErrMaterializePreview)
		}
		selectedRecordCount += uint64(len(sourceRange.Records))

		records := cloneMaterializeRecords(sourceRange.Records)
		view, err := capability.DecodeContext(ctx, records)
		if err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: decode range %d from Agent %q: %w", ErrMaterializeCapability, rangeIndex+1, sourceRange.SourceAgent, err)
		}
		if err := validateMaterializeView(view, sourceRange.SourceAgent); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d source view: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		if err := adapter.ValidateContextView(view); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d source view: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		if err := validateMaterializeSourceIndexes(view, len(records)); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d source indexes: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		if view.Unsupported > 0 && !options.AllowUnsupported {
			return MaterializePreview{}, fmt.Errorf("%w: range %d omitted %d unsupported source record(s)", ErrMaterializeUnsupportedSource, rangeIndex+1, view.Unsupported)
		}

		if err := appendMaterializePreviewItems(&combined, view, sourceRange); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d items: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		if err := addMaterializePreviewCounters(&combined, view); err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: range %d counters: %w", ErrMaterializePreview, rangeIndex+1, err)
		}
		summaries = append(summaries, MaterializeSourceSummary{
			ContributionID: sourceRange.ContributionID,
			SourceAgent:    sourceRange.SourceAgent,
			ReplicaID:      sourceRange.ReplicaID,
			StartRecord:    sourceRange.StartRecord,
			EndRecord:      sourceRange.EndRecord,
			RecordCount:    uint64(len(records)),
			ContextItems:   len(view.Items),
			Unsupported:    view.Unsupported,
			Filtered:       view.Filtered,
			SourceFormat:   view.SourceFormat,
		})
	}
	if selectedRecordCount != selection.SelectedRecordCount {
		return MaterializePreview{}, fmt.Errorf("%w: selected record count does not match ranges", ErrMaterializePreview)
	}
	if len(seenAgents) == 0 {
		return MaterializePreview{}, fmt.Errorf("%w: no source Agent was selected", ErrMaterializePreview)
	}
	foreignSource := false
	for sourceAgent := range seenAgents {
		if sourceAgent != options.TargetAgent {
			foreignSource = true
			break
		}
	}
	if !foreignSource {
		return MaterializePreview{}, fmt.Errorf("%w: source and target Agent are the same", ErrInvalidMaterializeRequest)
	}
	if err := adapter.ValidateContextView(combined); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: combined source view: %w", ErrMaterializePreview, err)
	}

	target := options.Target
	if strings.TrimSpace(target.NativeID) == "" {
		var err error
		target.NativeID, err = options.TargetCapability.NewSessionID(ctx)
		if err != nil {
			return MaterializePreview{}, fmt.Errorf("%w: allocate target session ID: %w", ErrMaterializeCapability, err)
		}
	}
	if err := validateMaterializeNativeID(target.NativeID); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: target session ID: %v", ErrMaterializePreview, err)
	}
	// Provenance is useful for the preview and source summaries, but it is not
	// target-native context. Strip it before crossing the target adapter
	// boundary so an adapter cannot accidentally persist hub-only identifiers
	// in a native Agent session.
	targetView := materializeTargetView(combined)
	encoded, err := options.TargetCapability.EncodeContext(ctx, targetView, target)
	if err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: encode target context: %w", ErrMaterializeCapability, err)
	}
	if err := validateEncodedContext(encoded); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: target context: %w", ErrMaterializePreview, err)
	}
	validatedRecords := cloneMaterializeRecords(encoded.Records)
	if err := options.TargetCapability.ValidateMaterialized(ctx, validatedRecords, target); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: validate target context: %w", ErrMaterializeCapability, err)
	}
	if err := ctx.Err(); err != nil {
		return MaterializePreview{}, fmt.Errorf("%w: %w", ErrMaterializePreview, err)
	}

	preview := MaterializePreview{
		Coverage:             cloneMaterializeCoverage(selection.Coverage),
		SelectedHeads:        append([]string(nil), selection.SelectedHeads...),
		Sources:              summaries,
		TargetAgent:          options.TargetAgent,
		TargetNativeID:       target.NativeID,
		SourceViewVersion:    combined.Version,
		TargetAdapterVersion: encoded.TargetAdapterVersion,
		SelectedRecordCount:  selectedRecordCount,
		ContextItems:         len(combined.Items),
		Stats:                encoded.Stats,
		EncodedRecords:       validatedRecords,
	}
	if err := preview.Validate(); err != nil {
		return MaterializePreview{}, err
	}
	return preview, nil
}

// Validate checks the in-memory invariants of a multi-source preview before
// a future apply transaction is allowed to consume its encoded records.
func (p MaterializePreview) Validate() error {
	if err := validateMaterializeAgent(p.TargetAgent, "target"); err != nil {
		return fmt.Errorf("%w: %v", ErrMaterializePreview, err)
	}
	if p.SourceViewVersion != adapter.MaterializeViewVersion {
		return fmt.Errorf("%w: unsupported source view version %d", ErrMaterializePreview, p.SourceViewVersion)
	}
	if strings.TrimSpace(p.TargetAdapterVersion) == "" || !utf8.ValidString(p.TargetAdapterVersion) {
		return fmt.Errorf("%w: target adapter version is invalid", ErrMaterializePreview)
	}
	if err := validateMaterializeNativeID(p.TargetNativeID); err != nil {
		return fmt.Errorf("%w: target session ID: %v", ErrMaterializePreview, err)
	}
	if p.Coverage.Incomplete || len(p.Coverage.SelectedIDs) == 0 || len(p.Sources) == 0 || p.SelectedRecordCount == 0 {
		return fmt.Errorf("%w: coverage or source summary is incomplete", ErrMaterializePreview)
	}
	if p.ContextItems < 0 {
		return fmt.Errorf("%w: context item count is negative", ErrMaterializePreview)
	}
	if err := validateMaterializeStats(p.Stats); err != nil {
		return fmt.Errorf("%w: %v", ErrMaterializePreview, err)
	}
	var recordCount uint64
	var contextItemCount int
	foreignSource := false
	for index, source := range p.Sources {
		if err := validateMaterializeAgent(source.SourceAgent, "source"); err != nil {
			return fmt.Errorf("%w: source %d: %v", ErrMaterializePreview, index+1, err)
		}
		if err := validateMaterializeNativeID(source.ReplicaID); err != nil {
			return fmt.Errorf("%w: source %d Replica ID: %v", ErrMaterializePreview, index+1, err)
		}
		if err := validateMaterializeNativeID(source.ContributionID); err != nil {
			return fmt.Errorf("%w: source %d Contribution ID: %v", ErrMaterializePreview, index+1, err)
		}
		if source.EndRecord <= source.StartRecord || source.RecordCount != source.EndRecord-source.StartRecord {
			return fmt.Errorf("%w: source %d range is invalid", ErrMaterializePreview, index+1)
		}
		if source.ContextItems < 0 || source.Unsupported < 0 || source.Filtered < 0 || strings.TrimSpace(source.SourceFormat) == "" || source.SourceFormat != strings.TrimSpace(source.SourceFormat) || !utf8.ValidString(source.SourceFormat) {
			return fmt.Errorf("%w: source %d summary is invalid", ErrMaterializePreview, index+1)
		}
		if source.SourceAgent != p.TargetAgent {
			foreignSource = true
		}
		if source.ContextItems > int(^uint(0)>>1)-contextItemCount {
			return fmt.Errorf("%w: source context item count overflows", ErrMaterializePreview)
		}
		contextItemCount += source.ContextItems
		if ^uint64(0)-recordCount < source.RecordCount {
			return fmt.Errorf("%w: source record count overflows", ErrMaterializePreview)
		}
		recordCount += source.RecordCount
	}
	if recordCount != p.SelectedRecordCount {
		return fmt.Errorf("%w: source record count does not match coverage", ErrMaterializePreview)
	}
	if contextItemCount != p.ContextItems {
		return fmt.Errorf("%w: source context item count does not match preview", ErrMaterializePreview)
	}
	if !foreignSource {
		return fmt.Errorf("%w: source and target Agent are the same", ErrMaterializePreview)
	}
	if len(p.EncodedRecords) == 0 {
		return fmt.Errorf("%w: target encoded session is empty", ErrMaterializePreview)
	}
	for index, record := range p.EncodedRecords {
		if len(record) == 0 || !utf8.Valid(record) || strings.ContainsAny(string(record), "\r\n") {
			return fmt.Errorf("%w: encoded record %d is invalid", ErrMaterializePreview, index+1)
		}
	}
	return nil
}

func validateMaterializePreviewRange(sourceRange MaterializeRange) error {
	if err := validateMaterializeAgent(sourceRange.SourceAgent, "source"); err != nil {
		return err
	}
	if err := validateMaterializeNativeID(sourceRange.ReplicaID); err != nil {
		return fmt.Errorf("Replica ID: %v", err)
	}
	if err := validateMaterializeNativeID(sourceRange.ContributionID); err != nil {
		return fmt.Errorf("Contribution ID: %v", err)
	}
	if sourceRange.EndRecord <= sourceRange.StartRecord || sourceRange.EndRecord-sourceRange.StartRecord != uint64(len(sourceRange.Records)) {
		return errors.New("record range does not match its copied records")
	}
	for index, record := range sourceRange.Records {
		if len(record) == 0 {
			return fmt.Errorf("record %d is empty", index+1)
		}
	}
	return nil
}

func appendMaterializePreviewItems(combined *adapter.ContextView, source adapter.ContextView, sourceRange MaterializeRange) error {
	if combined == nil {
		return errors.New("combined view is nil")
	}
	maxInt := int(^uint(0) >> 1)
	if len(source.Items) > maxInt-len(combined.Items) {
		return errors.New("context item count overflows")
	}
	for _, item := range source.Items {
		copyItem := item
		copyItem.Provenance = &adapter.ContextProvenance{
			SourceAgent:    sourceRange.SourceAgent,
			ReplicaID:      sourceRange.ReplicaID,
			ContributionID: sourceRange.ContributionID,
		}
		combined.Items = append(combined.Items, copyItem)
	}
	return nil
}

func addMaterializePreviewCounters(combined *adapter.ContextView, source adapter.ContextView) error {
	if combined == nil {
		return errors.New("combined view is nil")
	}
	if source.Unsupported < 0 || source.Filtered < 0 {
		return errors.New("source diagnostic counter is negative")
	}
	maxInt := int(^uint(0) >> 1)
	if source.Unsupported > maxInt-combined.Unsupported || source.Filtered > maxInt-combined.Filtered {
		return errors.New("diagnostic counter overflows")
	}
	combined.Unsupported += source.Unsupported
	combined.Filtered += source.Filtered
	return nil
}

func materializeTargetView(source adapter.ContextView) adapter.ContextView {
	view := source
	view.Items = make([]adapter.ContextItem, len(source.Items))
	copy(view.Items, source.Items)
	for index := range view.Items {
		view.Items[index].Provenance = nil
	}
	return view
}

func cloneMaterializeCoverage(coverage sessionhub.Coverage) sessionhub.Coverage {
	return sessionhub.Coverage{
		SelectedIDs: append([]string(nil), coverage.SelectedIDs...),
		OmittedIDs:  append([]string(nil), coverage.OmittedIDs...),
		Incomplete:  coverage.Incomplete,
		Reason:      coverage.Reason,
	}
}
