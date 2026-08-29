package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

func TestPublishLegacyMigrationReplicaCopiesBranchWithoutReplacingSessionDescriptor(t *testing.T) {
	store, public, private, identifierKey := newMigrationPublishFixture(t)
	configDir := t.TempDir()
	createdAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	records := [][]byte{[]byte(`{"type":"user","message":{"role":"user","content":"legacy body"}}`), []byte(`{"type":"assistant","message":{"role":"assistant","content":"reply"}}`)}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(uint64(len(records)), digest, []byte(`{"version":2,"agent":"claude-code","nativeId":"native-one","title":"legacy title","createdAt":"2026-08-29T12:00:00Z","updatedAt":"2026-08-29T12:01:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	current := project.Project{Identity: project.Identity{Kind: project.KindManual, Value: "manual:app"}}
	hubScope, projectScope, _, err := sessionHubAndProject(identifierKey, current)
	if err != nil {
		t.Fatal(err)
	}
	source := legacyMigrationSource{
		deviceID:  "deviceone",
		agent:     "claude-code",
		nativeID:  "native-one",
		known:     true,
		title:     "legacy title",
		createdAt: createdAt,
		updatedAt: createdAt.Add(time.Minute),
	}
	candidate := legacyMigrationCandidate{
		legacyID:     "legacyone",
		sessionID:    "sessionlogical",
		title:        "legacy title",
		createdAt:    createdAt,
		hasCreatedAt: true,
		updatedAt:    createdAt.Add(time.Minute),
		refs: []sessionhub.LegacyMigrationRef{{
			DeviceID:         source.deviceID,
			BranchHeadDigest: legacyMigrationDigest(digest),
			RecordCount:      uint64(len(records)),
		}},
		sources: []legacyMigrationSource{source},
	}
	legacy := syncer.LegacyReplica{
		LegacySessionID: candidate.legacyID,
		DeviceID:        source.deviceID,
		Metadata:        metadata,
		Branch:          syncer.Branch{DeviceID: source.deviceID, Records: records, HeadDigest: digest},
	}

	// A descriptor created by a different local v2 path is user-visible state.
	// Migration must reuse it instead of replacing its title or creator.
	sessionLayout, err := syncer.NewSessionHubLayout(hubScope.ID, projectScope.ID, candidate.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	existing := sessionhub.SessionDescriptor{
		Version:   sessionhub.ModelVersion,
		SessionID: candidate.sessionID,
		ProjectID: projectScope.ID,
		Title:     "user chosen title",
		CreatedAt: createdAt,
		CreatedBy: sessionhub.SessionCreator{Agent: "codex", DeviceID: source.deviceID},
		Lifecycle: sessionhub.SessionActive,
	}
	if err := syncer.PutSessionDescriptorForDevice(context.Background(), store, public, sessionLayout, source.deviceID, existing); err != nil {
		t.Fatalf("seed Session descriptor: %v", err)
	}

	access := &domainAccess{Store: store, Public: public, Identities: []*ecdh.PrivateKey{private}}
	collection := listCollection{
		current:       current,
		identifierKey: identifierKey,
		projectID:     "legacyproject",
		localDeviceID: source.deviceID,
	}
	result, err := publishLegacyMigrationReplica(context.Background(), configDir, access, collection, hubScope, projectScope, candidate, source, legacy)
	if err != nil {
		t.Fatalf("publishLegacyMigrationReplica: %v", err)
	}
	if !result.Complete || result.ReplicaID == "" || !result.WritesV2 {
		t.Fatalf("publish result = %+v", result)
	}

	descriptor, err := syncer.FetchSessionDescriptorForDevice(context.Background(), store, sessionLayout, source.deviceID, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchSessionDescriptorForDevice: %v", err)
	}
	if descriptor.Title != existing.Title || descriptor.CreatedBy != existing.CreatedBy {
		t.Fatalf("existing Session descriptor was replaced: %+v", descriptor)
	}
	layout, err := syncer.NewReplicaLayout(hubScope.ID, projectScope.ID, candidate.sessionID, result.ReplicaID, source.deviceID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := syncer.FetchCompleteReplica(context.Background(), store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica: %v", err)
	}
	if len(snapshot.Records) != len(records) || snapshot.HeadDigest != digest || !bytes.Equal(snapshot.Records[0], records[0]) {
		t.Fatalf("migrated snapshot = count:%d digest:%x records:%q", len(snapshot.Records), snapshot.HeadDigest, snapshot.Records)
	}
	if snapshot.Descriptor.Source.Agent != source.agent || snapshot.Descriptor.Source.NativeFormat != "legacy-v1" {
		t.Fatalf("migrated descriptor = %+v", snapshot.Descriptor)
	}
	v1Objects, err := store.List(context.Background(), "v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(v1Objects) != 0 {
		t.Fatalf("migration unexpectedly wrote legacy objects: %+v", v1Objects)
	}

	// The same request resumes from the durable cursor and verifies the
	// immutable descriptor instead of replacing it.
	second, err := publishLegacyMigrationReplica(context.Background(), configDir, access, collection, hubScope, projectScope, candidate, source, legacy)
	if err != nil {
		t.Fatalf("idempotent publishLegacyMigrationReplica: %v", err)
	}
	if !second.Complete || second.ReplicaID != result.ReplicaID || second.PublishedShards != 0 {
		t.Fatalf("idempotent publish result = %+v", second)
	}

}

func TestPublishLegacyMigrationReplicaResumesAfterShardFailure(t *testing.T) {
	base, public, private, identifierKey := newMigrationPublishFixture(t)
	configDir := t.TempDir()
	createdAt := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	records := make([][]byte, 300)
	for index := range records {
		records[index] = []byte(fmt.Sprintf(`{"n":%d}`, index))
	}
	digest, err := syncer.DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(uint64(len(records)), digest, []byte(`{"version":2,"agent":"codex","nativeId":"native-large","title":"large legacy","createdAt":"2026-08-29T13:00:00Z","updatedAt":"2026-08-29T13:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	current := project.Project{Identity: project.Identity{Kind: project.KindManual, Value: "manual:large"}}
	hubScope, projectScope, _, err := sessionHubAndProject(identifierKey, current)
	if err != nil {
		t.Fatal(err)
	}
	source := legacyMigrationSource{deviceID: "deviceone", agent: "codex", nativeID: "native-large", known: true, createdAt: createdAt, updatedAt: createdAt}
	candidate := legacyMigrationCandidate{
		legacyID: "legacylarge", sessionID: "sessionlarge", title: "large legacy", createdAt: createdAt, hasCreatedAt: true,
		updatedAt: createdAt, refs: []sessionhub.LegacyMigrationRef{{DeviceID: source.deviceID, BranchHeadDigest: legacyMigrationDigest(digest), RecordCount: uint64(len(records))}}, sources: []legacyMigrationSource{source},
	}
	legacy := syncer.LegacyReplica{LegacySessionID: candidate.legacyID, DeviceID: source.deviceID, Metadata: metadata, Branch: syncer.Branch{DeviceID: source.deviceID, Records: records, HeadDigest: digest}}
	failing := &failAfterPutRemote{Remote: base, failAt: 6}
	access := &domainAccess{Store: failing, Public: public, Identities: []*ecdh.PrivateKey{private}}
	collection := listCollection{current: current, identifierKey: identifierKey, projectID: "legacyproject", localDeviceID: source.deviceID}
	first, err := publishLegacyMigrationReplica(context.Background(), configDir, access, collection, hubScope, projectScope, candidate, source, legacy)
	if err == nil {
		t.Fatal("interrupted publish unexpectedly succeeded")
	}
	if first.Complete || first.PublishedShards != 1 || first.ReplicaID == "" {
		t.Fatalf("interrupted publish result = %+v, err=%v", first, err)
	}
	layout, err := syncer.NewReplicaLayout(hubScope.ID, projectScope.ID, candidate.sessionID, first.ReplicaID, source.deviceID)
	if err != nil {
		t.Fatal(err)
	}
	cursorStore, err := syncer.NewReplicaCursorStore(configDir, layout)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := cursorStore.Load(context.Background())
	if err != nil {
		t.Fatalf("load interrupted cursor: %v", err)
	}
	if cursor.NextShard != 2 || cursor.RecordCount != 256 {
		t.Fatalf("interrupted cursor = %+v", cursor)
	}

	access.Store = base
	second, err := publishLegacyMigrationReplica(context.Background(), configDir, access, collection, hubScope, projectScope, candidate, source, legacy)
	if err != nil {
		t.Fatalf("resume after interruption: %v", err)
	}
	if !second.Complete || second.ReplicaID != first.ReplicaID || second.PublishedShards != 1 {
		t.Fatalf("resume result = %+v", second)
	}
	snapshot, err := syncer.FetchCompleteReplica(context.Background(), base, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("FetchCompleteReplica after resume: %v", err)
	}
	if len(snapshot.Records) != len(records) || snapshot.HeadDigest != digest {
		t.Fatalf("resumed snapshot = count:%d digest:%x", len(snapshot.Records), snapshot.HeadDigest)
	}
}

func TestSelectLegacyMigrationPublishSourceRequiresLocalKnownBranch(t *testing.T) {
	base := legacyMigrationCandidate{sources: []legacyMigrationSource{
		{deviceID: "remoteone", agent: "codex", nativeID: "native", known: true},
		{deviceID: "localone", agent: "unknown", known: false},
	}}
	if _, err := selectLegacyMigrationPublishSource(base, "localone"); err == nil {
		t.Fatal("unknown local source was accepted")
	}
	if _, err := selectLegacyMigrationPublishSource(base, "otherone"); err == nil {
		t.Fatal("foreign source was accepted for local publish")
	}
}

func TestSelectCompleteLegacyBranchRevalidatesLiveSource(t *testing.T) {
	store, public, private, identifierKey := newMigrationPublishFixture(t)
	records := [][]byte{[]byte(`{"type":"user","message":{"role":"user","content":"hello"}}`)}
	layout, err := syncer.NewObjectLayout("legacyproject", "legacyone", "deviceone")
	if err != nil {
		t.Fatal(err)
	}
	shard, err := syncer.NewShard(0, syncer.EmptyDigest(), records)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := syncer.PutShard(context.Background(), store, public, layout, syncer.NewPushCursor(), syncer.ShardPart{Number: 1, Shard: shard})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := syncer.NewMetadata(1, cursor.HeadDigest, []byte(`{"version":2,"agent":"codex","nativeId":"native-one","title":"legacy","createdAt":"2026-08-29T14:00:00Z","updatedAt":"2026-08-29T14:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.PutMetadata(context.Background(), store, public, layout, metadata); err != nil {
		t.Fatal(err)
	}
	current := project.Project{Identity: project.Identity{Kind: project.KindManual, Value: "manual:app"}}
	collection := listCollection{identifierKey: identifierKey, projectID: "legacyproject", localDeviceID: "deviceone"}
	candidate := legacyMigrationCandidate{
		legacyID: "legacyone",
		refs:     []sessionhub.LegacyMigrationRef{{DeviceID: "deviceone", BranchHeadDigest: legacyMigrationDigest(cursor.HeadDigest), RecordCount: 1}},
		sources:  []legacyMigrationSource{{deviceID: "deviceone", agent: "codex", nativeID: "native-one", known: true}},
	}
	access := &domainAccess{Store: store, Public: public, Identities: []*ecdh.PrivateKey{private}}
	legacy, live, err := selectCompleteLegacyBranch(context.Background(), access, collection, candidate, candidate.sources[0])
	if err != nil {
		t.Fatalf("selectCompleteLegacyBranch: %v", err)
	}
	if legacy.DeviceID != "deviceone" || len(legacy.Branch.Records) != 1 || live.agent != "codex" || live.nativeID != "native-one" {
		t.Fatalf("legacy=%+v live=%+v", legacy, live)
	}
	reader, streamedMetadata, streamedSource, err := selectLegacyMigrationReader(context.Background(), access, collection, candidate, candidate.sources[0])
	if err != nil {
		t.Fatalf("selectLegacyMigrationReader: %v", err)
	}
	defer reader.Close()
	if streamedMetadata.RecordCount != metadata.RecordCount || streamedMetadata.HeadDigest != metadata.HeadDigest || streamedSource != live {
		t.Fatalf("stream metadata/source = %+v/%+v, want %+v/%+v", streamedMetadata, streamedSource, metadata, live)
	}
	var streamed [][]byte
	for {
		record, nextErr := reader.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("stream reader.Next: %v", nextErr)
		}
		streamed = append(streamed, record)
	}
	if len(streamed) != 1 || !bytes.Equal(streamed[0], records[0]) {
		t.Fatalf("streamed legacy records = %q", streamed)
	}
	if reader.Close() != nil {
		t.Fatal("close legacy stream reader")
	}

	candidate.sources[0].nativeID = "stale-native"
	if _, _, err := selectCompleteLegacyBranch(context.Background(), access, collection, candidate, candidate.sources[0]); err == nil {
		t.Fatal("stale source identity was accepted")
	}
	_ = current
}

func TestRecordLegacyMigrationPublishProgressIsIdempotent(t *testing.T) {
	root := t.TempDir()
	candidate := legacyMigrationCandidate{
		legacyID:  "legacyone",
		sessionID: "sessionone",
		refs:      []sessionhub.LegacyMigrationRef{{DeviceID: "deviceone", BranchHeadDigest: "sha256:" + strings.Repeat("a", 64), RecordCount: 1}},
	}
	hubScope := sessionHubScope{ID: "hubone", Name: "default"}
	projectScope := sessionProjectScope{ID: "projectone"}
	updated, changed, err := recordLegacyMigrationPublishProgress(root, hubScope, projectScope, candidate, nil, "replicaone", sessionhub.MigrationStatusPublished)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || updated[candidate.legacyID].Status != sessionhub.MigrationStatusPublished {
		t.Fatalf("first progress = changed:%t ledgers:%v", changed, updated)
	}
	path, err := sessionhub.MigrationLedgerPath(root, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secondUpdated, secondChanged, err := recordLegacyMigrationPublishProgress(root, hubScope, projectScope, candidate, updated, "replicaone", sessionhub.MigrationStatusPublished)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if secondChanged || !bytes.Equal(first, second) || secondUpdated[candidate.legacyID].Status != sessionhub.MigrationStatusPublished {
		t.Fatalf("repeat progress changed:%t first:%s second:%s", secondChanged, first, second)
	}
}

func TestMigrationConfirmationRequiresAffirmativeAnswer(t *testing.T) {
	candidate := legacyMigrationCandidate{sessionID: "sessionone", records: 4, refs: []sessionhub.LegacyMigrationRef{{DeviceID: "deviceone"}}}
	var prompt bytes.Buffer
	confirmed, err := confirmLegacyMigrationPublish(bufio.NewReader(strings.NewReader("yes\n")), &prompt, candidate, legacyMigrationSource{deviceID: "deviceone"})
	if err != nil || !confirmed {
		t.Fatalf("positive confirmation = %t, err=%v", confirmed, err)
	}
	if !strings.Contains(prompt.String(), "sessionone") || !strings.Contains(prompt.String(), "v1 data will remain unchanged") {
		t.Fatalf("publish prompt = %q", prompt.String())
	}

	prompt.Reset()
	confirmed, err = confirmLegacyMigrationRollback(bufio.NewReader(strings.NewReader("no\n")), &prompt, candidate)
	if err != nil || confirmed {
		t.Fatalf("negative rollback confirmation = %t, err=%v", confirmed, err)
	}
	if !strings.Contains(prompt.String(), "v1/v2 remote objects will be kept") {
		t.Fatalf("rollback prompt = %q", prompt.String())
	}
}

func TestSetLegacyMigrationReadModePreservesProgressAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	candidate := legacyMigrationCandidate{
		legacyID:  "legacyone",
		sessionID: "sessionone",
		refs:      []sessionhub.LegacyMigrationRef{{DeviceID: "deviceone", BranchHeadDigest: "sha256:" + strings.Repeat("a", 64), RecordCount: 4}},
	}
	hubScope := sessionHubScope{ID: "hubone", Name: "default"}
	projectScope := sessionProjectScope{ID: "projectone"}
	initial := sessionhub.MigrationLedger{
		Version:           sessionhub.MigrationLedgerVersion,
		HubID:             hubScope.ID,
		ProjectID:         projectScope.ID,
		LegacySessionID:   candidate.legacyID,
		SessionID:         candidate.sessionID,
		LegacyRefs:        append([]sessionhub.LegacyMigrationRef(nil), candidate.refs...),
		PublishedReplicas: []string{"replicaone"},
		Status:            sessionhub.MigrationStatusPublished,
		ReadMode:          sessionhub.MigrationReadModeV2,
		UpdatedAt:         time.Date(2026, 8, 29, 15, 0, 0, 0, time.UTC),
	}
	if err := sessionhub.SaveMigrationLedger(root, initial); err != nil {
		t.Fatal(err)
	}
	ledgers := map[string]sessionhub.MigrationLedger{candidate.legacyID: initial}
	updated, changed, err := setLegacyMigrationReadMode(root, hubScope, projectScope, candidate, ledgers, sessionhub.MigrationReadModeLegacy)
	if err != nil || !changed {
		t.Fatalf("rollback mode change = %t, err=%v", changed, err)
	}
	loaded, err := sessionhub.LoadMigrationLedger(root, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReadMode != sessionhub.MigrationReadModeLegacy || loaded.Status != initial.Status || !reflect.DeepEqual(loaded.PublishedReplicas, initial.PublishedReplicas) {
		t.Fatalf("rollback ledger = %+v", loaded)
	}
	path, err := sessionhub.MigrationLedgerPath(root, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, changed, err = setLegacyMigrationReadMode(root, hubScope, projectScope, candidate, updated, sessionhub.MigrationReadModeLegacy)
	if err != nil || changed {
		t.Fatalf("repeated rollback mode change = %t, err=%v", changed, err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || updated[candidate.legacyID].ReadMode != sessionhub.MigrationReadModeLegacy {
		t.Fatalf("repeated rollback changed local state: first=%s second=%s", first, second)
	}

	newRoot := t.TempDir()
	created, changed, err := setLegacyMigrationReadMode(newRoot, hubScope, projectScope, candidate, nil, sessionhub.MigrationReadModeLegacy)
	if err != nil || !changed {
		t.Fatalf("rollback mode creation = %t, err=%v", changed, err)
	}
	createdLedger, err := sessionhub.LoadMigrationLedger(newRoot, hubScope.ID, projectScope.ID, candidate.legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if createdLedger.ReadMode != sessionhub.MigrationReadModeLegacy || created[candidate.legacyID].ReadMode != sessionhub.MigrationReadModeLegacy {
		t.Fatalf("created rollback ledger = %+v", createdLedger)
	}
}

func TestLoadLegacyMigrationReadModeDefaultsToV2AndHonoursRollback(t *testing.T) {
	root := t.TempDir()
	if mode, err := loadLegacyMigrationReadMode(root, "hubone", "projectone", "legacyone"); err != nil || mode != sessionhub.MigrationReadModeV2 {
		t.Fatalf("missing migration read mode = %q, err=%v", mode, err)
	}
	ledger := sessionhub.MigrationLedger{
		Version:         sessionhub.MigrationLedgerVersion,
		HubID:           "hubone",
		ProjectID:       "projectone",
		LegacySessionID: "legacyone",
		SessionID:       "sessionone",
		Status:          sessionhub.MigrationStatusPublished,
		ReadMode:        sessionhub.MigrationReadModeLegacy,
		UpdatedAt:       time.Date(2026, 8, 29, 16, 0, 0, 0, time.UTC),
	}
	if err := sessionhub.SaveMigrationLedger(root, ledger); err != nil {
		t.Fatal(err)
	}
	if mode, err := loadLegacyMigrationReadMode(root, ledger.HubID, ledger.ProjectID, ledger.LegacySessionID); err != nil || mode != sessionhub.MigrationReadModeLegacy {
		t.Fatalf("rollback migration read mode = %q, err=%v", mode, err)
	}
}

type failAfterPutRemote struct {
	remote.Remote
	failAt int
	puts   int
}

func (r *failAfterPutRemote) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	r.puts++
	if r.failAt > 0 && r.puts == r.failAt {
		return errors.New("injected migration publish interruption")
	}
	return r.Remote.Put(ctx, key, body, size)
}

func newMigrationPublishFixture(t *testing.T) (remote.Remote, *ecdh.PublicKey, *ecdh.PrivateKey, []byte) {
	t.Helper()
	store, err := remote.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataKey := crypto.NewDataKey()
	t.Cleanup(dataKey.Close)
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}
	identifierKey, err := dataKey.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}
	return store, public, private, identifierKey
}
