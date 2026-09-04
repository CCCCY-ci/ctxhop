package syncer

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/remote"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
)

// TestLegacyReplicaStreamingS3Integration exercises the real object-store
// boundary for the v1-to-v2 migration path. It is opt-in so ordinary unit and
// CI runs never contact an external service.
func TestLegacyReplicaStreamingS3Integration(t *testing.T) {
	if os.Getenv("CTXHOP_S3_INTEGRATION") != "1" {
		t.Skip("set CTXHOP_S3_INTEGRATION=1 to run the external S3/R2 stream acceptance test")
	}

	s3Config := syncerS3IntegrationConfig(t)
	prefix := strings.TrimRight(strings.TrimSpace(s3Config.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	s3Config.Prefix = prefix + fmt.Sprintf("ctxhop-stream-integration/%d-%d", time.Now().UnixNano(), os.Getpid())

	store, err := remote.NewS3(s3Config)
	if err != nil {
		t.Fatalf("create S3 Remote: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancelCleanup()
		objects, listErr := store.List(cleanupCtx, "")
		if listErr != nil {
			t.Logf("S3 stream integration cleanup listing failed: %v", listErr)
			return
		}
		for _, object := range objects {
			if deleteErr := store.Delete(cleanupCtx, object.Key); deleteErr != nil {
				t.Logf("S3 stream integration cleanup failed for object: %v", deleteErr)
			}
		}
	})

	if err := store.Probe(ctx); err != nil {
		t.Fatalf("S3 stream integration probe: %v", err)
	}

	dataKey := crypto.NewDataKey()
	defer dataKey.Close()
	public, err := dataKey.IdentityPublic()
	if err != nil {
		t.Fatal(err)
	}
	private, err := dataKey.IdentityPrivate()
	if err != nil {
		t.Fatal(err)
	}

	const (
		projectID = "r2project"
		sessionID = "r2session"
		deviceID  = "r2device"
	)
	records := make([][]byte, 130)
	for index := range records {
		records[index] = []byte(fmt.Sprintf(`{"n":%d,"source":"claude-code"}`, index))
	}
	expectedDigest, err := DigestRecords(records)
	if err != nil {
		t.Fatal(err)
	}

	legacyLayout, err := NewObjectLayout(projectID, sessionID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan, err := PlanAppend(NewPushCursor(), records, PlanOptions{
		MaxRecords:      31,
		MaxEncodedBytes: maxShardBytes,
	})
	if err != nil {
		t.Fatalf("plan legacy shards: %v", err)
	}
	legacyCursor := NewPushCursor()
	for _, part := range legacyPlan.Parts {
		legacyCursor, err = PutShard(ctx, store, public, legacyLayout, legacyCursor, part)
		if err != nil {
			t.Fatalf("publish legacy shard %d: %v", part.Number, err)
		}
	}
	if legacyCursor != legacyPlan.Next {
		t.Fatalf("legacy cursor = %+v, plan next = %+v", legacyCursor, legacyPlan.Next)
	}
	metadata, err := NewMetadata(uint64(len(records)), expectedDigest, []byte(`{"agent":"claude-code","nativeSession":"legacy-r2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := PutMetadata(ctx, store, public, legacyLayout, metadata); err != nil {
		t.Fatalf("publish legacy metadata: %v", err)
	}

	legacyReader, err := OpenLegacyReplicaReader(ctx, store, projectID, sessionID, deviceID, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("open legacy R2 reader: %v", err)
	}
	legacyGot := readAllRecordStream(t, ctx, legacyReader)
	if !recordsEqual(legacyGot, records) {
		t.Fatal("R2 legacy reader changed the canonical record sequence")
	}
	if got := legacyReader.Metadata(); got.RecordCount != metadata.RecordCount || got.HeadDigest != metadata.HeadDigest || !bytes.Equal(got.Payload, metadata.Payload) {
		t.Fatalf("legacy R2 metadata = %+v, want %+v", got, metadata)
	}
	if err := legacyReader.Close(); err != nil {
		t.Fatalf("close legacy R2 reader: %v", err)
	}

	layout, err := NewReplicaLayout("r2hub", projectID, sessionID, "r2replica", deviceID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := sessionhub.NativeReplicaDescriptor{
		Version:   sessionhub.ModelVersion,
		ReplicaID: layout.ReplicaKey(),
		SessionID: layout.SessionKey(),
		Source: sessionhub.NativeSource{
			Agent:            "codex",
			NativeSessionKey: "nativer2codex",
			DeviceID:         layout.DeviceID(),
			Generation:       1,
			NativeFormat:     "codex-jsonl",
			AgentVersion:     "integration-test",
		},
		Origin:    sessionhub.ReplicaOrigin{Kind: sessionhub.ReplicaOriginNative},
		CreatedAt: time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}
	state, err := NewReplicaCursorStore(t.TempDir(), layout)
	if err != nil {
		t.Fatal(err)
	}
	streamReader, err := OpenLegacyReplicaReader(ctx, store, projectID, sessionID, deviceID, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("reopen legacy R2 reader for migration: %v", err)
	}
	result, err := PushReplicaStreamWithCursorStore(ctx, store, public, layout, descriptor, state, streamReader, ReplicaStreamOptions{
		ReplicaPushOptions: ReplicaPushOptions{
			Plan:       PlanOptions{MaxRecords: 29, MaxEncodedBytes: maxShardBytes},
			Identities: []*ecdh.PrivateKey{private},
			Now:        time.Date(2026, 8, 29, 0, 1, 0, 0, time.UTC),
		},
		VerifyExpected:      true,
		ExpectedRecordCount: uint64(len(records)),
		ExpectedHeadDigest:  expectedDigest,
	})
	if err != nil {
		t.Fatalf("stream legacy R2 Replica into v2: %v", err)
	}
	if result.Cursor.RecordCount != uint64(len(records)) || result.Cursor.HeadDigest != expectedDigest || result.PublishedShards != 5 || result.Tip.ReplicaID != layout.ReplicaKey() {
		t.Fatalf("R2 stream result = %+v", result)
	}

	snapshot, err := FetchCompleteReplica(ctx, store, layout, []*ecdh.PrivateKey{private})
	if err != nil {
		t.Fatalf("fetch complete v2 Replica from R2: %v", err)
	}
	if snapshot.Descriptor.Source.Agent != "codex" || snapshot.Descriptor.Source.NativeSessionKey != "nativer2codex" || !recordsEqual(snapshot.Records, records) || snapshot.HeadDigest != expectedDigest {
		t.Fatalf("R2 v2 snapshot does not preserve the migrated stream: descriptor=%+v count=%d digest=%x", snapshot.Descriptor, len(snapshot.Records), snapshot.HeadDigest)
	}

	legacyReplicas, err := FetchCompleteLegacyReplicas(ctx, store, projectID, sessionID, private)
	if err != nil {
		t.Fatalf("re-fetch unchanged v1 Replica from R2: %v", err)
	}
	if len(legacyReplicas) != 1 || !recordsEqual(legacyReplicas[0].Branch.Records, records) || legacyReplicas[0].Metadata.HeadDigest != expectedDigest {
		t.Fatalf("v1 Replica changed after v2 migration: %+v", legacyReplicas)
	}
}

func readAllRecordStream(t *testing.T, ctx context.Context, reader RecordReader) [][]byte {
	t.Helper()
	defer reader.Close()
	var records [][]byte
	for {
		record, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatalf("read record stream: %v", err)
		}
		records = append(records, record)
	}
}

func syncerS3IntegrationConfig(t *testing.T) remote.S3Config {
	t.Helper()
	pathStyle := false
	if value := strings.TrimSpace(os.Getenv("CTXHOP_S3_PATH_STYLE")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("CTXHOP_S3_PATH_STYLE: %v", err)
		}
		pathStyle = parsed
	}
	return remote.S3Config{
		Endpoint:     requiredSyncerS3Env(t, "CTXHOP_S3_ENDPOINT"),
		Region:       requiredSyncerS3Env(t, "CTXHOP_S3_REGION"),
		Bucket:       requiredSyncerS3Env(t, "CTXHOP_S3_BUCKET"),
		Prefix:       strings.TrimRight(strings.TrimSpace(os.Getenv("CTXHOP_S3_PREFIX")), "/"),
		AccessKey:    requiredSyncerS3Env(t, "CTXHOP_S3_ACCESS_KEY_ID"),
		SecretKey:    requiredSyncerS3Env(t, "CTXHOP_S3_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("CTXHOP_S3_SESSION_TOKEN"),
		PathStyle:    pathStyle,
	}
}

func requiredSyncerS3Env(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		t.Fatalf("%s must be set when S3 integration is enabled", name)
	}
	return value
}
