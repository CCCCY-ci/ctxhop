package syncflow

import (
	"encoding/json"
	"fmt"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

// DigestMaterializePreview returns the stable digest of the complete planned
// target output and its selection metadata. It is suitable for a local
// transaction journal, but is not a replacement for the authenticated Remote
// objects: a retry must still re-read and validate the source snapshot.
func DigestMaterializePreview(preview MaterializePreview) ([32]byte, error) {
	if err := preview.Validate(); err != nil {
		return [32]byte{}, err
	}
	wire := struct {
		Coverage              sessionhub.Coverage        `json:"coverage"`
		SelectedHeads         []string                   `json:"selectedHeads"`
		Sources               []MaterializeSourceSummary `json:"sources"`
		TargetAgent           string                     `json:"targetAgent"`
		TargetNativeID        string                     `json:"targetNativeId"`
		SourceViewVersion     int                        `json:"sourceViewVersion"`
		TargetAdapterVersion  string                     `json:"targetAdapterVersion"`
		SourceSnapshotDigest  string                     `json:"sourceSnapshotDigest,omitempty"`
		SelectedRecordCount   uint64                     `json:"selectedRecordCount"`
		ContextItems          int                        `json:"contextItems"`
		Stats                 adapter.MaterializeStats   `json:"stats"`
		EnvironmentComponents []environment.Component    `json:"environmentComponents,omitempty"`
		EncodedRecords        [][]byte                   `json:"encodedRecords"`
	}{
		Coverage:              cloneMaterializeCoverage(preview.Coverage),
		SelectedHeads:         append([]string(nil), preview.SelectedHeads...),
		Sources:               append([]MaterializeSourceSummary(nil), preview.Sources...),
		TargetAgent:           preview.TargetAgent,
		TargetNativeID:        preview.TargetNativeID,
		SourceViewVersion:     preview.SourceViewVersion,
		TargetAdapterVersion:  preview.TargetAdapterVersion,
		SourceSnapshotDigest:  preview.SourceSnapshotDigest,
		SelectedRecordCount:   preview.SelectedRecordCount,
		ContextItems:          preview.ContextItems,
		Stats:                 preview.Stats,
		EnvironmentComponents: append([]environment.Component(nil), preview.EnvironmentComponents...),
		EncodedRecords:        cloneMaterializeRecords(preview.EncodedRecords),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode materialize preview digest: %w", err)
	}
	return sessionhub.DigestBytes(data), nil
}
