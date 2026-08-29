package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

func TestBuildLegacyMigrationCandidatesUsesStableLogicalIDAndUnknownSource(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Identity: project.Identity{Kind: project.KindRemote, Value: "github.com/example/app"}}
	_, _, v2ProjectID, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	knownPayload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		Agent: "codex", NativeID: "native-codex", Title: "known title", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	known, err := syncer.NewMetadata(7, [32]byte{1}, knownPayload)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := syncer.NewMetadata(3, [32]byte{2}, []byte(`{"version":1,"nativeId":"legacy-native","title":"old","createdAt":"2026-08-29T09:00:00Z","updatedAt":"2026-08-29T09:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	collection := listCollection{
		current:       current,
		identifierKey: key,
		remoteSessions: []syncer.ProjectMetadataRef{{
			SessionID: "legacysession",
			Devices: []syncer.MetadataRef{
				{DeviceID: "deviceb", Metadata: unknown},
				{DeviceID: "devicea", Metadata: known},
			},
		}},
	}

	candidates, err := buildLegacyMigrationCandidates(collection, v2ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %+v", candidates)
	}
	candidate := candidates[0]
	wantID, err := sessionhub.DeriveLegacySessionLogicalID(key, v2ProjectID, "legacysession")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.sessionID != wantID || candidate.records != 7 || len(candidate.refs) != 2 {
		t.Fatalf("candidate = %+v, want session=%q records=7 two refs", candidate, wantID)
	}
	if candidate.refs[0].DeviceID != "devicea" || candidate.refs[1].DeviceID != "deviceb" {
		t.Fatalf("refs are not deterministic: %+v", candidate.refs)
	}
	if len(candidate.sources) != 2 || !candidate.sources[0].known || candidate.sources[1].known {
		t.Fatalf("sources = %+v, want one known and one unknown", candidate.sources)
	}

	report := buildSessionMigrationReport(
		sessionHubScope{ID: "hub01", Name: "default"},
		sessionProjectScope{ID: v2ProjectID},
		candidates,
		map[string]sessionhub.MigrationLedger{},
		map[string]bool{},
		nil,
		sessionOptions{action: sessionActionMigrate, preview: true},
	)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`nativeId`)) || bytes.Contains(encoded, []byte(`nativeSessionId`)) {
		t.Fatalf("migration report exposed native identity: %s", encoded)
	}
	if report.Sessions[0].KnownSourceCount != 1 || report.Sessions[0].UnknownSourceCount != 1 || len(report.Warnings) != 1 || report.Warnings[0].Code != migrationUnknownSourceCode {
		t.Fatalf("report = %+v", report)
	}
}

func TestApplyLazyLegacyMigrationOnlyWritesLocalMetadataAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Identity: project.Identity{Kind: project.KindManual, Value: "manual:app"}}
	_, projectScope, v2ProjectID, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	payload, err := syncflow.EncodeSessionSummary(adapter.SessionRef{
		Agent: "claude-code", NativeID: "native-claude", Title: "migrated", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(4, [32]byte{3}, payload)
	if err != nil {
		t.Fatal(err)
	}
	collection := listCollection{
		current:       current,
		identifierKey: key,
		localDeviceID: "localdevice",
		remoteSessions: []syncer.ProjectMetadataRef{{
			SessionID: "legacysession",
			Devices:   []syncer.MetadataRef{{DeviceID: "remotedevice", Metadata: metadata}},
		}},
	}
	hubScope, _, _, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := sessionhub.NewDefaultRegistry(key, now)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := buildLegacyMigrationCandidates(collection, v2ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	updated, registryChanged, ledgerChanged, err := applyLazyLegacyMigration(root, collection, hubScope, projectScope, registry, candidates, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !registryChanged || !ledgerChanged || len(updated) != 1 {
		t.Fatalf("first migration changes = registry:%t ledger:%t ledgers:%v", registryChanged, ledgerChanged, updated)
	}

	firstRegistry, err := os.ReadFile(root + "/" + sessionhub.RegistryFileName)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := sessionhub.LoadMigrationLedger(root, hubScope.ID, projectScope.ID, candidates[0].legacyID)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath, err := sessionhub.MigrationLedgerPath(root, hubScope.ID, projectScope.ID, candidates[0].legacyID)
	if err != nil {
		t.Fatal(err)
	}
	firstLedger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(firstLedger, []byte(`nativeSessionId`)) || bytes.Contains(firstLedger, []byte(`body`)) {
		t.Fatalf("ledger contains prohibited data: %s", firstLedger)
	}
	projectRecord, ok := registry.Project(projectScope.ID)
	if !ok || len(projectRecord.Sessions) != 1 || projectRecord.Sessions[0].Descriptor.SessionID != candidates[0].sessionID {
		t.Fatalf("registry did not create logical Session mapping: %+v, ok=%t", projectRecord, ok)
	}
	if len(projectRecord.Sessions[0].Sources) != 0 {
		// Remote summary native IDs are provenance only. They must not become
		// local source bindings until this device discovers/attaches its file.
		t.Fatalf("remote source was incorrectly recorded as local: %+v", projectRecord.Sessions[0].Sources)
	}

	secondRegistry, err := sessionhub.LoadRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	_, secondRegistryChanged, secondLedgerChanged, err := applyLazyLegacyMigration(root, collection, hubScope, projectScope, secondRegistry, candidates, updated, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondRegistryChanged || secondLedgerChanged {
		t.Fatalf("repeat migration was not idempotent: registry:%t ledger:%t", secondRegistryChanged, secondLedgerChanged)
	}
	secondRegistryBytes, err := os.ReadFile(root + "/" + sessionhub.RegistryFileName)
	if err != nil {
		t.Fatal(err)
	}
	secondLedgerBytes, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstRegistry, secondRegistryBytes) || !bytes.Equal(firstLedger, secondLedgerBytes) {
		t.Fatal("repeat migration changed local metadata")
	}
	if ledger.Status != sessionhub.MigrationStatusLazy {
		t.Fatalf("ledger status = %q", ledger.Status)
	}
}

func TestLoadLegacyMigrationLedgersTreatsCorruptLedgerAsReadOnly(t *testing.T) {
	root := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	current := project.Project{Identity: project.Identity{Kind: project.KindRemote, Value: "github.com/example/app"}}
	_, projectScope, _, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	legacyID := "legacysession"
	hubScope, _, _, err := sessionHubAndProject(key, current)
	if err != nil {
		t.Fatal(err)
	}
	candidate := legacyMigrationCandidate{legacyID: legacyID, sessionID: "session01", refs: []sessionhub.LegacyMigrationRef{{DeviceID: "device01", BranchHeadDigest: "sha256:" + strings.Repeat("a", 64)}}}
	path, err := sessionhub.MigrationLedgerPath(root, hubScope.ID, projectScope.ID, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path[:strings.LastIndexAny(path, `/\\`)], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledgers, corrupt, warnings, err := loadLegacyMigrationLedgers(root, hubScope.ID, projectScope.ID, []legacyMigrationCandidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledgers) != 0 || !corrupt[legacyID] || len(warnings) != 1 || warnings[0].Code != "migration-ledger-corrupt" {
		t.Fatalf("ledgers=%v corrupt=%v warnings=%v", ledgers, corrupt, warnings)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("corrupt ledger was removed or replaced: %v", err)
	}
}

func TestParseSessionOptionsSupportsMigrationPreview(t *testing.T) {
	options, err := parseSessionOptions([]string{"migrate", "legacysession", "--preview", "--publish-v2", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if options.action != sessionActionMigrate || options.sessionID != "legacysession" || !options.preview || !options.publishV2 || !options.json {
		t.Fatalf("options = %+v", options)
	}
	allOptions, err := parseSessionOptions([]string{"migrate", "--preview"})
	if err != nil || allOptions.sessionID != "" {
		t.Fatalf("all-session migration options = %+v, err=%v", allOptions, err)
	}
	if _, err := parseSessionOptions([]string{"migrate", "one", "two"}); err == nil {
		t.Fatal("migrate accepted two session selectors")
	}
	rollback, err := parseSessionOptions([]string{"migrate", "legacysession", "--rollback", "--yes", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !rollback.rollback || !rollback.yes || rollback.publishV2 || !rollback.json {
		t.Fatalf("rollback options = %+v", rollback)
	}
	invalid := [][]string{
		{"migrate", "one", "--publish-v2", "--rollback"},
		{"migrate", "one", "--yes"},
		{"migrate", "one", "--preview", "--yes", "--rollback"},
	}
	for _, args := range invalid {
		if _, err := parseSessionOptions(args); err == nil {
			t.Fatalf("parseSessionOptions(%v) unexpectedly succeeded", args)
		}
	}
}
