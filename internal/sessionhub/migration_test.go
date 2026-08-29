package sessionhub

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDeriveLegacySessionLogicalIDKeepsExistingMapping(t *testing.T) {
	identifierKey := []byte(strings.Repeat("k", 32))
	projectKey, err := DeriveProjectKey(identifierKey, testOpaque('h'), "github.com/example/project")
	if err != nil {
		t.Fatal(err)
	}
	legacyID := testOpaque('l')

	existing, err := DeriveLegacySessionKey(identifierKey, projectKey, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := DeriveLegacySessionLogicalID(identifierKey, projectKey, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if logical != existing {
		t.Fatalf("logical ID = %q, existing key = %q", logical, existing)
	}
	if err := validateOpaqueID(logical); err != nil {
		t.Fatalf("logical ID is not an opaque key: %v", err)
	}
}

func TestMigrationLedgerRoundTripCanonicalizesOrdering(t *testing.T) {
	ledger := testMigrationLedger()
	ledger.LegacyRefs = []LegacyMigrationRef{
		{DeviceID: testOpaque('b'), BranchHeadDigest: testDigest('b'), RecordCount: 2},
		{DeviceID: testOpaque('a'), BranchHeadDigest: "sha256:" + testDigest('a'), RecordCount: 1},
	}
	ledger.PublishedReplicas = []string{testOpaque('c'), testOpaque('a'), testOpaque('c')}

	encoded, err := ledger.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	firstDevice := bytes.Index(encoded, []byte(`"deviceId":"`+testOpaque('a')+`"`))
	secondDevice := bytes.Index(encoded, []byte(`"deviceId":"`+testOpaque('b')+`"`))
	if firstDevice < 0 || secondDevice < 0 || firstDevice > secondDevice {
		t.Fatalf("legacy refs are not sorted: %s", encoded)
	}
	if bytes.Count(encoded, []byte(`"`+testOpaque('c')+`"`)) != 1 {
		t.Fatalf("published replicas were not deduplicated: %s", encoded)
	}

	parsed, err := ParseMigrationLedger(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.LegacyRefs[0].DeviceID; got != testOpaque('a') {
		t.Fatalf("first ref device = %q, want %q", got, testOpaque('a'))
	}
	if !reflect.DeepEqual(parsed.PublishedReplicas, []string{testOpaque('a'), testOpaque('c')}) {
		t.Fatalf("published replicas = %v", parsed.PublishedReplicas)
	}
	canonical, err := parsed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, canonical) {
		t.Fatalf("round trip changed canonical bytes:\n%s\n%s", encoded, canonical)
	}
}

func TestSaveMigrationLedgerMergesIdempotentlyAndUpdatesDeviceRef(t *testing.T) {
	root := t.TempDir()
	initial := testMigrationLedger()
	if err := SaveMigrationLedger(root, initial); err != nil {
		t.Fatal(err)
	}
	path, err := MigrationLedgerPath(root, initial.HubID, initial.ProjectID, initial.LegacySessionID)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SaveMigrationLedger(root, initial); err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("repeating the same ledger changed its canonical bytes")
	}

	updated := initial
	updated.UpdatedAt = initial.UpdatedAt.Add(time.Minute)
	updated.Status = MigrationStatusPartial
	updated.LegacyRefs = []LegacyMigrationRef{
		{DeviceID: initial.LegacyRefs[0].DeviceID, BranchHeadDigest: testDigest('f'), RecordCount: 99},
		{DeviceID: testOpaque('z'), BranchHeadDigest: testDigest('e'), RecordCount: 3},
	}
	updated.PublishedReplicas = []string{testOpaque('r'), testOpaque('r')}
	if err := SaveMigrationLedger(root, updated); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMigrationLedger(root, initial.HubID, initial.ProjectID, initial.LegacySessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != MigrationStatusPartial {
		t.Fatalf("merged status = %q, want partial", loaded.Status)
	}
	if len(loaded.LegacyRefs) != 2 || loaded.LegacyRefs[0].DeviceID != initial.LegacyRefs[0].DeviceID || loaded.LegacyRefs[0].BranchHeadDigest != testDigest('f') || loaded.LegacyRefs[0].RecordCount != 99 {
		t.Fatalf("merged legacy refs = %+v", loaded.LegacyRefs)
	}
	if !reflect.DeepEqual(loaded.PublishedReplicas, []string{testOpaque('q'), testOpaque('r')}) {
		t.Fatalf("merged published replicas = %v", loaded.PublishedReplicas)
	}
}

func TestPublishedMigrationLedgerCannotBeDowngraded(t *testing.T) {
	root := t.TempDir()
	published := testMigrationLedger()
	published.Status = MigrationStatusPublished
	published.PublishedReplicas = []string{testOpaque('p')}
	if err := SaveMigrationLedger(root, published); err != nil {
		t.Fatal(err)
	}

	for _, status := range []MigrationStatus{MigrationStatusLazy, MigrationStatusPartial, MigrationStatusBlocked} {
		incoming := published
		incoming.Status = status
		incoming.UpdatedAt = published.UpdatedAt.Add(time.Minute)
		incoming.PublishedReplicas = []string{testOpaque('q')}
		if err := SaveMigrationLedger(root, incoming); err != nil {
			t.Fatalf("SaveMigrationLedger(%q): %v", status, err)
		}
		loaded, err := LoadMigrationLedger(root, published.HubID, published.ProjectID, published.LegacySessionID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Status != MigrationStatusPublished {
			t.Fatalf("status after %q update = %q, want published", status, loaded.Status)
		}
	}
}

func TestMigrationLedgerScopeAndMappingConflicts(t *testing.T) {
	root := t.TempDir()
	ledger := testMigrationLedger()
	if err := SaveMigrationLedger(root, ledger); err != nil {
		t.Fatal(err)
	}

	mappingConflict := ledger
	mappingConflict.SessionID = testOpaque('m')
	if err := SaveMigrationLedger(root, mappingConflict); !errors.Is(err, ErrMigrationLedgerMappingConflict) || !errors.Is(err, ErrMigrationLedgerConflict) {
		t.Fatalf("mapping conflict error = %v", err)
	}

	scopeRoot := t.TempDir()
	scopePath, err := MigrationLedgerPath(scopeRoot, ledger.HubID, ledger.ProjectID, ledger.LegacySessionID)
	if err != nil {
		t.Fatal(err)
	}
	scoped := ledger
	scoped.HubID = testOpaque('x')
	scopedBytes, err := scoped.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepathDir(scopePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopePath, scopedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMigrationLedger(scopeRoot, ledger.HubID, ledger.ProjectID, ledger.LegacySessionID); !errors.Is(err, ErrMigrationLedgerScopeConflict) || !errors.Is(err, ErrMigrationLedgerConflict) {
		t.Fatalf("scope conflict error = %v", err)
	}
}

func TestMigrationLedgerRejectsCorruptAndUnknownVersion(t *testing.T) {
	if _, err := ParseMigrationLedger([]byte(`{"version":1`)); !errors.Is(err, ErrMigrationLedgerCorrupt) {
		t.Fatalf("corrupt parse error = %v", err)
	}

	encoded, err := testMigrationLedger().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(encoded, []byte(`{"version":1}`), []byte(`{"version":2}`), 1)
	if bytes.Equal(unknown, encoded) {
		unknown = bytes.Replace(encoded, []byte(`{"version":1,"`), []byte(`{"version":2,"`), 1)
	}
	if _, err := ParseMigrationLedger(unknown); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("unknown version error = %v", err)
	}

	root := t.TempDir()
	ledger := testMigrationLedger()
	path, err := MigrationLedgerPath(root, ledger.HubID, ledger.ProjectID, ledger.LegacySessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveMigrationLedger(root, ledger); !errors.Is(err, ErrMigrationLedgerCorrupt) {
		t.Fatalf("save over corrupt ledger error = %v", err)
	}
}

func TestMigrationLedgerContainsOnlyMetadataFields(t *testing.T) {
	ledger := testMigrationLedger()
	encoded, err := ledger.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatal(err)
	}
	wantTop := map[string]bool{
		"version": true, "hubId": true, "projectId": true, "legacySessionId": true,
		"sessionId": true, "legacyRefs": true, "publishedReplicas": true,
		"status": true, "updatedAt": true,
	}
	if len(top) != len(wantTop) {
		t.Fatalf("ledger fields = %v", top)
	}
	for field := range top {
		if !wantTop[field] {
			t.Fatalf("unexpected ledger field %q", field)
		}
	}
	var refs []map[string]json.RawMessage
	if err := json.Unmarshal(top["legacyRefs"], &refs); err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if len(ref) != 3 {
			t.Fatalf("legacy ref fields = %v", ref)
		}
		for _, forbidden := range []string{"body", "nativeId", "nativeSessionId", "path", "secret"} {
			if _, exists := ref[forbidden]; exists {
				t.Fatalf("legacy ref contains forbidden field %q", forbidden)
			}
		}
	}
	for _, forbidden := range []string{"body", "nativeId", "nativeSessionId", "path", "secret"} {
		if _, exists := top[forbidden]; exists {
			t.Fatalf("ledger contains forbidden field %q", forbidden)
		}
	}
	if strings.Contains(string(encoded), "nativeSessionId") || strings.Contains(string(encoded), "recordBody") {
		t.Fatalf("ledger payload contains prohibited field name: %s", encoded)
	}
}

func testMigrationLedger() MigrationLedger {
	return MigrationLedger{
		Version:         MigrationLedgerVersion,
		HubID:           testOpaque('h'),
		ProjectID:       testOpaque('p'),
		LegacySessionID: testOpaque('l'),
		SessionID:       testOpaque('s'),
		LegacyRefs: []LegacyMigrationRef{
			{DeviceID: testOpaque('q'), BranchHeadDigest: testDigest('a'), RecordCount: 4},
		},
		PublishedReplicas: []string{testOpaque('q')},
		Status:            MigrationStatusLazy,
		UpdatedAt:         time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	}
}

// Keep test path assertions independent of platform-specific filepath
// details while retaining the production path implementation as the source
// of truth.
func filepathDir(path string) string {
	lastSeparator := strings.LastIndexAny(path, `/\\`)
	if lastSeparator < 0 {
		return "."
	}
	return path[:lastSeparator]
}
