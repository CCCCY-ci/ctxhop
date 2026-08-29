package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const (
	migrationModeLazy          = "lazy"
	migrationModeFullPublish   = "full-publish"
	migrationCompatibilityV1   = "legacy-reader"
	migrationProvenanceRoot    = "legacy-root"
	migrationUnknownSourceCode = "legacy-source-unknown"
)

// sessionMigrationReport is deliberately metadata-only. In particular, it
// does not expose a native session ID, local path, remote address, credential,
// or record body. The report is safe to use as a preview artifact.
type sessionMigrationReport struct {
	Scope       string                      `json:"scope"`
	Hub         sessionHubScope             `json:"hub"`
	Project     sessionProjectScope         `json:"project"`
	Mode        string                      `json:"mode"`
	Preview     bool                        `json:"preview"`
	Sessions    []sessionMigrationEntry     `json:"sessions"`
	SideEffects sessionMigrationSideEffects `json:"sideEffects"`
	Warnings    []sessionMigrationWarning   `json:"warnings"`
}

type sessionMigrationEntry struct {
	LegacySessionID    string                `json:"legacySessionId"`
	SessionID          string                `json:"sessionId"`
	Title              string                `json:"title"`
	CreatedAt          time.Time             `json:"createdAt,omitempty"`
	UpdatedAt          time.Time             `json:"updatedAt,omitempty"`
	Status             string                `json:"status"`
	Compatibility      string                `json:"compatibility"`
	Provenance         string                `json:"provenance"`
	BranchCount        int                   `json:"branchCount"`
	RecordCount        uint64                `json:"recordCount"`
	KnownSourceCount   int                   `json:"knownSourceCount"`
	UnknownSourceCount int                   `json:"unknownSourceCount"`
	SourceAgents       []string              `json:"sourceAgents"`
	LegacyRefs         []sessionMigrationRef `json:"legacyRefs"`
	PublishedReplicas  []string              `json:"publishedReplicas"`
}

type sessionMigrationRef struct {
	DeviceID         string `json:"deviceId"`
	BranchHeadDigest string `json:"branchHeadDigest"`
	RecordCount      uint64 `json:"recordCount"`
}

type sessionMigrationSideEffects struct {
	WritesV1       bool `json:"writesV1"`
	WritesV2       bool `json:"writesV2"`
	WritesRegistry bool `json:"writesRegistry"`
	WritesLedger   bool `json:"writesLedger"`
	ReadsBodies    bool `json:"readsBodies"`
}

type sessionMigrationWarning struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	LegacySessionID string `json:"legacySessionId,omitempty"`
	DeviceID        string `json:"deviceId,omitempty"`
}

type legacyMigrationSource struct {
	deviceID  string
	agent     string
	nativeID  string
	known     bool
	title     string
	createdAt time.Time
	updatedAt time.Time
}

type legacyMigrationCandidate struct {
	legacyID     string
	sessionID    string
	title        string
	createdAt    time.Time
	hasCreatedAt bool
	updatedAt    time.Time
	records      uint64
	refs         []sessionhub.LegacyMigrationRef
	sources      []legacyMigrationSource
	warnings     []sessionMigrationWarning
}

// collectSessionMigrationWithPrompt implements the first migration slice:
// read v1 metadata, project it into stable v2 identities, and optionally
// persist only local mapping/ledger metadata. It never reads v1 shard bodies.
func collectSessionMigrationWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, options sessionOptions, input io.Reader, prompt io.Writer) (sessionMigrationReport, error) {
	if c == nil {
		return sessionMigrationReport{}, errors.New("session migrate: configuration is unavailable")
	}
	if ctx == nil {
		return sessionMigrationReport{}, errors.New("session migrate: context is required")
	}
	if err := ctx.Err(); err != nil {
		return sessionMigrationReport{}, fmt.Errorf("session migrate: %w", err)
	}
	if options.publishV2 && !options.preview {
		return sessionMigrationReport{}, errors.New("session migrate: --publish-v2 is not implemented yet; use --publish-v2 --preview to inspect the planned publish")
	}

	collection, err := collectListCollection(ctx, c, configDir, projectDir, input, prompt, "session migrate")
	if err != nil {
		return sessionMigrationReport{}, err
	}
	hubScope, projectScope, v2ProjectID, err := sessionHubAndProject(collection.identifierKey, collection.current)
	if err != nil {
		return sessionMigrationReport{}, err
	}
	registry, err := loadSessionRegistryForRead(configDir, collection.identifierKey, hubScope.ID)
	if err != nil {
		return sessionMigrationReport{}, fmt.Errorf("session migrate: load local Session Hub registry: %w", err)
	}
	candidates, err := buildLegacyMigrationCandidates(collection, v2ProjectID)
	if err != nil {
		return sessionMigrationReport{}, fmt.Errorf("session migrate: inspect legacy metadata: %w", err)
	}
	candidates, err = selectLegacyMigrationCandidates(candidates, options.sessionID)
	if err != nil {
		return sessionMigrationReport{}, err
	}
	ledgers, corrupt, ledgerWarnings, err := loadLegacyMigrationLedgers(configDir, hubScope.ID, projectScope.ID, candidates)
	if err != nil {
		return sessionMigrationReport{}, err
	}
	report := buildSessionMigrationReport(hubScope, projectScope, candidates, ledgers, corrupt, ledgerWarnings, options)
	if options.preview || len(candidates) == 0 {
		return report, nil
	}

	updatedLedgers, registryChanged, ledgerChanged, err := applyLazyLegacyMigration(configDir, collection, hubScope, projectScope, registry, candidates, ledgers, corrupt)
	if err != nil {
		return sessionMigrationReport{}, err
	}
	report = buildSessionMigrationReport(hubScope, projectScope, candidates, updatedLedgers, corrupt, ledgerWarnings, options)
	report.SideEffects.WritesRegistry = registryChanged
	report.SideEffects.WritesLedger = ledgerChanged
	return report, nil
}

func buildLegacyMigrationCandidates(collection listCollection, v2ProjectID string) ([]legacyMigrationCandidate, error) {
	candidates := make([]legacyMigrationCandidate, 0, len(collection.remoteSessions))
	for _, group := range collection.remoteSessions {
		if strings.TrimSpace(group.SessionID) == "" {
			return nil, errors.New("legacy-session-ambiguous: a v1 session has no stable identity")
		}
		logicalID, err := sessionhub.DeriveLegacySessionKey(collection.identifierKey, v2ProjectID, group.SessionID)
		if err != nil {
			return nil, fmt.Errorf("legacy-session-ambiguous: invalid v1 session identity: %w", err)
		}
		candidate := legacyMigrationCandidate{legacyID: group.SessionID, sessionID: logicalID}
		seenDevices := make(map[string]struct{}, len(group.Devices))
		for _, device := range group.Devices {
			if _, exists := seenDevices[device.DeviceID]; exists {
				return nil, fmt.Errorf("legacy-session-ambiguous: duplicate v1 device branch in session %q", group.SessionID)
			}
			seenDevices[device.DeviceID] = struct{}{}
			ref := sessionhub.LegacyMigrationRef{
				DeviceID:         device.DeviceID,
				BranchHeadDigest: "sha256:" + hex.EncodeToString(device.Metadata.HeadDigest[:]),
				RecordCount:      device.Metadata.RecordCount,
			}
			candidate.refs = append(candidate.refs, ref)
			candidate.records = maxUint64(candidate.records, device.Metadata.RecordCount)
			source, warning := legacyMigrationSourceFromMetadata(collection.identifierKey, group.SessionID, device)
			candidate.sources = append(candidate.sources, source)
			if warning != nil {
				candidate.warnings = append(candidate.warnings, *warning)
			}
			if source.title != "" && (candidate.title == "" || source.updatedAt.After(candidate.updatedAt)) {
				candidate.title = source.title
			}
			if !source.createdAt.IsZero() && (candidate.createdAt.IsZero() || source.createdAt.Before(candidate.createdAt)) {
				candidate.createdAt = source.createdAt.UTC()
				candidate.hasCreatedAt = true
			}
			if source.updatedAt.After(candidate.updatedAt) {
				candidate.updatedAt = source.updatedAt.UTC()
			}
		}
		if len(candidate.refs) == 0 {
			return nil, fmt.Errorf("legacy-session-ambiguous: v1 session %q has no visible device branch", group.SessionID)
		}
		if candidate.title == "" {
			candidate.title = "encrypted session metadata"
		}
		if candidate.createdAt.IsZero() {
			candidate.createdAt = legacyUnknownTime
		}
		if candidate.updatedAt.IsZero() {
			candidate.updatedAt = candidate.createdAt
		}
		sortLegacyMigrationRefs(candidate.refs)
		sort.SliceStable(candidate.sources, func(i, j int) bool {
			if candidate.sources[i].agent != candidate.sources[j].agent {
				return candidate.sources[i].agent < candidate.sources[j].agent
			}
			return candidate.sources[i].deviceID < candidate.sources[j].deviceID
		})
		sort.SliceStable(candidate.warnings, func(i, j int) bool {
			if candidate.warnings[i].DeviceID != candidate.warnings[j].DeviceID {
				return candidate.warnings[i].DeviceID < candidate.warnings[j].DeviceID
			}
			return candidate.warnings[i].Code < candidate.warnings[j].Code
		})
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].legacyID != candidates[j].legacyID {
			return candidates[i].legacyID < candidates[j].legacyID
		}
		return candidates[i].sessionID < candidates[j].sessionID
	})
	return candidates, nil
}

var legacyUnknownTime = time.Unix(0, 0).UTC()

func legacyMigrationSourceFromMetadata(identifierKey []byte, legacyID string, device syncer.MetadataRef) (legacyMigrationSource, *sessionMigrationWarning) {
	source := legacyMigrationSource{deviceID: device.DeviceID, agent: "unknown"}
	summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
	if err != nil {
		return source, &sessionMigrationWarning{
			Code:            migrationUnknownSourceCode,
			Message:         "Agent source is unavailable; no Agent was inferred",
			LegacySessionID: legacyID,
			DeviceID:        device.DeviceID,
		}
	}
	source.title = safeListText(summary.Title)
	source.createdAt = summary.CreatedAt
	source.updatedAt = summary.UpdatedAt
	source.nativeID = summary.NativeID
	if strings.TrimSpace(summary.Agent) == "" {
		return source, &sessionMigrationWarning{
			Code:            migrationUnknownSourceCode,
			Message:         "Agent source is unavailable; no Agent was inferred",
			LegacySessionID: legacyID,
			DeviceID:        device.DeviceID,
		}
	}
	agent := sessionAgentLabel(summary.Agent)
	// DeriveNativeSessionKey validates the source shape while keeping the
	// resulting key transient; it is not written to the migration report or
	// ledger.
	if _, err := sessionhub.DeriveNativeSessionKey(identifierKey, agent, summary.NativeID); err != nil {
		return source, &sessionMigrationWarning{
			Code:            migrationUnknownSourceCode,
			Message:         "Agent source metadata is invalid; no Agent was inferred",
			LegacySessionID: legacyID,
			DeviceID:        device.DeviceID,
		}
	}
	source.agent = agent
	source.known = true
	return source, nil
}

func selectLegacyMigrationCandidates(candidates []legacyMigrationCandidate, selector string) ([]legacyMigrationCandidate, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return candidates, nil
	}
	selected := make([]legacyMigrationCandidate, 0, 1)
	for _, candidate := range candidates {
		if candidate.legacyID == selector || candidate.sessionID == selector {
			selected = append(selected, candidate)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("session migrate: requested session is not present in legacy metadata")
	}
	if len(selected) > 1 {
		return nil, errors.New("session migrate: requested session matches multiple legacy records")
	}
	return selected, nil
}

func loadLegacyMigrationLedgers(configDir, hubID, projectID string, candidates []legacyMigrationCandidate) (map[string]sessionhub.MigrationLedger, map[string]bool, []sessionMigrationWarning, error) {
	ledgers := make(map[string]sessionhub.MigrationLedger, len(candidates))
	corrupt := make(map[string]bool)
	var warnings []sessionMigrationWarning
	for _, candidate := range candidates {
		ledger, err := sessionhub.LoadMigrationLedger(configDir, hubID, projectID, candidate.legacyID)
		if errors.Is(err, sessionhub.ErrMigrationLedgerNotFound) {
			continue
		}
		if errors.Is(err, sessionhub.ErrMigrationLedgerCorrupt) {
			corrupt[candidate.legacyID] = true
			warnings = append(warnings, sessionMigrationWarning{
				Code:            "migration-ledger-corrupt",
				Message:         "local migration progress is corrupt; only read-only discovery is allowed",
				LegacySessionID: candidate.legacyID,
			})
			continue
		}
		if errors.Is(err, sessionhub.ErrUnsupportedVersion) {
			corrupt[candidate.legacyID] = true
			warnings = append(warnings, sessionMigrationWarning{
				Code:            "unsupported-version",
				Message:         "local migration progress uses a newer format; only read-only discovery is allowed",
				LegacySessionID: candidate.legacyID,
			})
			continue
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("session migrate: load local migration ledger: %w", err)
		}
		if ledger.HubID != hubID || ledger.ProjectID != projectID || ledger.LegacySessionID != candidate.legacyID || ledger.SessionID != candidate.sessionID {
			return nil, nil, nil, errors.New("session migrate: migration ledger identity conflicts with current project")
		}
		ledgers[candidate.legacyID] = ledger
	}
	sort.Slice(warnings, func(i, j int) bool {
		return warnings[i].LegacySessionID < warnings[j].LegacySessionID
	})
	return ledgers, corrupt, warnings, nil
}

func buildSessionMigrationReport(hubScope sessionHubScope, projectScope sessionProjectScope, candidates []legacyMigrationCandidate, ledgers map[string]sessionhub.MigrationLedger, corrupt map[string]bool, ledgerWarnings []sessionMigrationWarning, options sessionOptions) sessionMigrationReport {
	mode := migrationModeLazy
	if options.publishV2 {
		mode = migrationModeFullPublish
	}
	report := sessionMigrationReport{
		Scope:    "project",
		Hub:      hubScope,
		Project:  projectScope,
		Mode:     mode,
		Preview:  options.preview,
		Sessions: make([]sessionMigrationEntry, 0, len(candidates)),
		SideEffects: sessionMigrationSideEffects{
			WritesV1:    false,
			WritesV2:    false,
			ReadsBodies: false,
		},
		Warnings: append([]sessionMigrationWarning(nil), ledgerWarnings...),
	}
	if options.publishV2 {
		report.Warnings = append(report.Warnings, sessionMigrationWarning{
			Code:    "full-publish-pending",
			Message: "full v2 Replica publish is not implemented; this command only shows the planned side effects",
		})
	}
	for _, candidate := range candidates {
		status := string(sessionhub.MigrationStatusLazy)
		published := []string{}
		if corrupt[candidate.legacyID] {
			status = string(sessionhub.MigrationStatusBlocked)
		}
		if ledger, ok := ledgers[candidate.legacyID]; ok {
			status = string(ledger.Status)
			published = append(published, ledger.PublishedReplicas...)
		}
		agents := make([]string, 0, len(candidate.sources))
		agentSeen := make(map[string]struct{}, len(candidate.sources))
		knownCount := 0
		for _, source := range candidate.sources {
			if source.known {
				knownCount++
			}
			if _, exists := agentSeen[source.agent]; !exists {
				agentSeen[source.agent] = struct{}{}
				agents = append(agents, source.agent)
			}
		}
		sort.Strings(agents)
		refs := make([]sessionMigrationRef, 0, len(candidate.refs))
		for _, ref := range candidate.refs {
			refs = append(refs, sessionMigrationRef{DeviceID: ref.DeviceID, BranchHeadDigest: ref.BranchHeadDigest, RecordCount: ref.RecordCount})
		}
		sort.Strings(published)
		entry := sessionMigrationEntry{
			LegacySessionID:    candidate.legacyID,
			SessionID:          candidate.sessionID,
			Title:              candidate.title,
			CreatedAt:          candidate.createdAt,
			UpdatedAt:          candidate.updatedAt,
			Status:             status,
			Compatibility:      migrationCompatibilityV1,
			Provenance:         migrationProvenanceRoot,
			BranchCount:        len(candidate.refs),
			RecordCount:        candidate.records,
			KnownSourceCount:   knownCount,
			UnknownSourceCount: len(candidate.sources) - knownCount,
			SourceAgents:       agents,
			LegacyRefs:         refs,
			PublishedReplicas:  published,
		}
		report.Sessions = append(report.Sessions, entry)
		report.Warnings = append(report.Warnings, candidate.warnings...)
	}
	sort.Slice(report.Sessions, func(i, j int) bool {
		return report.Sessions[i].LegacySessionID < report.Sessions[j].LegacySessionID
	})
	sort.SliceStable(report.Warnings, func(i, j int) bool {
		if report.Warnings[i].LegacySessionID != report.Warnings[j].LegacySessionID {
			return report.Warnings[i].LegacySessionID < report.Warnings[j].LegacySessionID
		}
		if report.Warnings[i].DeviceID != report.Warnings[j].DeviceID {
			return report.Warnings[i].DeviceID < report.Warnings[j].DeviceID
		}
		return report.Warnings[i].Code < report.Warnings[j].Code
	})
	return report
}

func applyLazyLegacyMigration(configDir string, collection listCollection, hubScope sessionHubScope, projectScope sessionProjectScope, registry sessionhub.Registry, candidates []legacyMigrationCandidate, ledgers map[string]sessionhub.MigrationLedger, corrupt map[string]bool) (map[string]sessionhub.MigrationLedger, bool, bool, error) {
	identityKind := sessionhub.ProjectIdentityRemote
	if collection.current.Identity.Kind == project.KindManual {
		identityKind = sessionhub.ProjectIdentityManual
	}
	if _, err := registry.EnsureProject(collection.identifierKey, identityKind, collection.current.Identity.Value, time.Now().UTC()); err != nil {
		return nil, false, false, fmt.Errorf("session migrate: create local Project mapping: %w", err)
	}
	beforeRegistry, err := registry.MarshalBinary()
	if err != nil {
		return nil, false, false, fmt.Errorf("session migrate: prepare local Session Hub mapping: %w", err)
	}
	updated := cloneMigrationLedgerMap(ledgers)
	ledgerWrites := make([]sessionhub.MigrationLedger, 0, len(candidates))
	now := time.Now().UTC().Round(0)
	for _, candidate := range candidates {
		if corrupt[candidate.legacyID] {
			continue
		}
		creator := legacyMigrationCreator(candidate)
		title := candidate.title
		if title == "encrypted session metadata" {
			// This is a display fallback, not authoritative metadata. Do not
			// overwrite a title already learned by an earlier migration.
			title = ""
		}
		createdAt := candidate.createdAt
		if !candidate.hasCreatedAt {
			// Epoch is a deterministic marker for a missing legacy timestamp;
			// it keeps the local descriptor valid without making repeated
			// migrations produce a different timestamp.
			createdAt = legacyUnknownTime
		}
		if _, err := registry.EnsureLegacySession(collection.identifierKey, projectScope.ID, candidate.legacyID, title, createdAt, creator); err != nil {
			return nil, false, false, fmt.Errorf("session migrate: create logical Session mapping: %w", err)
		}
		// The native IDs decoded from remote v1 summaries describe branches on
		// other devices. They are not local bindings and must not affect local
		// resume selection. A local adapter discovery or explicit attach owns
		// that binding decision.
		current, hasLedger := updated[candidate.legacyID]
		desired := sessionhub.MigrationLedger{
			Version:         sessionhub.MigrationLedgerVersion,
			HubID:           hubScope.ID,
			ProjectID:       projectScope.ID,
			LegacySessionID: candidate.legacyID,
			SessionID:       candidate.sessionID,
			LegacyRefs:      append([]sessionhub.LegacyMigrationRef(nil), candidate.refs...),
			Status:          sessionhub.MigrationStatusLazy,
			UpdatedAt:       now,
		}
		if hasLedger {
			desired.PublishedReplicas = append([]string(nil), current.PublishedReplicas...)
			desired.Status = current.Status
		}
		if !hasLedger || !sameMigrationRefs(current.LegacyRefs, desired.LegacyRefs) {
			ledgerWrites = append(ledgerWrites, desired)
			updated[candidate.legacyID] = desired
		}
	}
	afterRegistry, err := registry.MarshalBinary()
	if err != nil {
		return nil, false, false, fmt.Errorf("session migrate: finalize local Session Hub mapping: %w", err)
	}
	registryChanged := !bytes.Equal(beforeRegistry, afterRegistry)
	if registryChanged {
		if err := sessionhub.SaveRegistry(configDir, registry); err != nil {
			return nil, false, false, fmt.Errorf("session migrate: save local Session Hub mapping: %w", err)
		}
	}
	for _, ledger := range ledgerWrites {
		if err := sessionhub.SaveMigrationLedger(configDir, ledger); err != nil {
			return nil, registryChanged, false, fmt.Errorf("session migrate: save local migration ledger: %w", err)
		}
		effective, err := sessionhub.LoadMigrationLedger(configDir, hubScope.ID, projectScope.ID, ledger.LegacySessionID)
		if err != nil {
			return nil, registryChanged, false, fmt.Errorf("session migrate: verify local migration ledger: %w", err)
		}
		updated[ledger.LegacySessionID] = effective
	}
	return updated, registryChanged, len(ledgerWrites) != 0, nil
}

func legacyMigrationCreator(candidate legacyMigrationCandidate) sessionhub.SessionCreator {
	for _, source := range candidate.sources {
		if source.known {
			return sessionhub.SessionCreator{Agent: source.agent, DeviceID: source.deviceID}
		}
	}
	if len(candidate.refs) != 0 {
		return sessionhub.SessionCreator{Agent: "unknown", DeviceID: candidate.refs[0].DeviceID}
	}
	return sessionhub.SessionCreator{Agent: "unknown", DeviceID: "unknown"}
}

func sameMigrationRefs(left, right []sessionhub.LegacyMigrationRef) bool {
	left = append([]sessionhub.LegacyMigrationRef(nil), left...)
	right = append([]sessionhub.LegacyMigrationRef(nil), right...)
	sortLegacyMigrationRefs(left)
	sortLegacyMigrationRefs(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneMigrationLedgerMap(input map[string]sessionhub.MigrationLedger) map[string]sessionhub.MigrationLedger {
	output := make(map[string]sessionhub.MigrationLedger, len(input))
	for key, value := range input {
		value.LegacyRefs = append([]sessionhub.LegacyMigrationRef(nil), value.LegacyRefs...)
		value.PublishedReplicas = append([]string(nil), value.PublishedReplicas...)
		output[key] = value
	}
	return output
}

func sortLegacyMigrationRefs(refs []sessionhub.LegacyMigrationRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].DeviceID != refs[j].DeviceID {
			return refs[i].DeviceID < refs[j].DeviceID
		}
		if refs[i].BranchHeadDigest != refs[j].BranchHeadDigest {
			return refs[i].BranchHeadDigest < refs[j].BranchHeadDigest
		}
		return refs[i].RecordCount < refs[j].RecordCount
	})
}

func writeSessionMigrationJSON(w io.Writer, report sessionMigrationReport) error {
	if w == nil {
		return errors.New("session migrate: output is required")
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeSessionMigrationText(w io.Writer, report sessionMigrationReport) error {
	if w == nil {
		return errors.New("session migrate: output is required")
	}
	if _, err := fmt.Fprintf(w, "scope: %s\n", safeListText(report.Scope)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "mode: %s preview=%t\n", safeListText(report.Mode), report.Preview); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "hub: %s (%s)\n", safeListText(report.Hub.Name), safeListText(report.Hub.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "project: %s\n", safeListText(report.Project.ID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "sessions: %d\n", len(report.Sessions)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "side-effects: v1=%t v2=%t registry=%t ledger=%t bodies=%t\n", report.SideEffects.WritesV1, report.SideEffects.WritesV2, report.SideEffects.WritesRegistry, report.SideEffects.WritesLedger, report.SideEffects.ReadsBodies); err != nil {
		return err
	}
	for _, entry := range report.Sessions {
		if _, err := fmt.Fprintf(w, "- legacy=%s session=%s status=%s compatibility=%s branches=%d records=%d sources=%s provenance=%s\n", safeListText(entry.LegacySessionID), safeListText(entry.SessionID), safeListText(entry.Status), safeListText(entry.Compatibility), entry.BranchCount, entry.RecordCount, strings.Join(entry.SourceAgents, ","), safeListText(entry.Provenance)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "warnings: %d\n", len(report.Warnings)); err != nil {
		return err
	}
	for _, warning := range report.Warnings {
		if _, err := fmt.Fprintf(w, "! %s: %s\n", safeListText(warning.Code), safeListText(warning.Message)); err != nil {
			return err
		}
	}
	return nil
}
