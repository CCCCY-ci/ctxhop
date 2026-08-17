package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestS3Integration is opt-in so ordinary unit and CI runs never contact an
// external service. It uses only synthetic bytes and records each key it
// writes, so cleanup deletes only objects created by this test invocation.
func TestS3Integration(t *testing.T) {
	if os.Getenv("AGENTSYNC_S3_INTEGRATION") != "1" {
		t.Skip("set AGENTSYNC_S3_INTEGRATION=1 to run the external S3/R2 acceptance test")
	}

	endpoint := integrationEnv(t, "AGENTSYNC_S3_ENDPOINT")
	bucket := integrationEnv(t, "AGENTSYNC_S3_BUCKET")
	region := integrationEnv(t, "AGENTSYNC_S3_REGION")
	accessKey := integrationEnv(t, "AGENTSYNC_S3_ACCESS_KEY_ID")
	secretKey := integrationEnv(t, "AGENTSYNC_S3_SECRET_ACCESS_KEY")

	pathStyle := false
	if value := strings.TrimSpace(os.Getenv("AGENTSYNC_S3_PATH_STYLE")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("AGENTSYNC_S3_PATH_STYLE: %v", err)
		}
		pathStyle = parsed
	}

	prefix := strings.TrimRight(strings.TrimSpace(os.Getenv("AGENTSYNC_S3_PREFIX")), "/")
	if prefix != "" {
		prefix += "/"
	}
	prefix += fmt.Sprintf("agentsync-integration/%d-%d", time.Now().UnixNano(), os.Getpid())

	store, err := NewS3(S3Config{
		Endpoint:     endpoint,
		Region:       region,
		Bucket:       bucket,
		Prefix:       prefix,
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		SessionToken: os.Getenv("AGENTSYNC_S3_SESSION_TOKEN"),
		PathStyle:    pathStyle,
	})
	if err != nil {
		t.Fatalf("create S3 Remote: %v", err)
	}

	createdKeys := make([]string, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancelCleanup()
		for _, createdKey := range createdKeys {
			if deleteErr := store.Delete(cleanupCtx, createdKey); deleteErr != nil {
				t.Logf("S3 integration cleanup delete %q failed: %v", createdKey, deleteErr)
			}
		}
	})

	if err := store.Probe(ctx); err != nil {
		t.Fatalf("S3 probe: %v", err)
	}

	key := "v1/integration/binary"
	want := []byte{0x00, 0x01, 0x7f, 0x80, 0xfe, 0xff}
	if err := store.Put(ctx, key, strings.NewReader(string(want)), int64(len(want))); err != nil {
		t.Fatalf("S3 put: %v", err)
	}
	createdKeys = append(createdKeys, key)

	reader, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("S3 get: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("S3 get body: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("S3 close body: %v", closeErr)
	}
	if string(got) != string(want) {
		t.Fatalf("S3 round trip changed bytes: got %x, want %x", got, want)
	}

	info, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatalf("S3 stat: %v", err)
	}
	if info.Size != int64(len(want)) {
		t.Fatalf("S3 stat size = %d, want %d", info.Size, len(want))
	}

	objects, err := store.List(ctx, "v1/integration/")
	if err != nil {
		t.Fatalf("S3 list: %v", err)
	}
	if !containsObject(objects, key) {
		t.Fatalf("S3 list did not return %q: %+v", key, objects)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("S3 delete: %v", err)
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("S3 deleted object get = %v, want ErrNotFound", err)
	}

	if os.Getenv("AGENTSYNC_S3_INTEGRATION_PAGINATION") != "1" {
		return
	}

	const paginationCount = 1001
	for i := 0; i < paginationCount; i++ {
		paginationKey := fmt.Sprintf("v1/pagination/%04d", i)
		if err := store.Put(ctx, paginationKey, strings.NewReader("x"), 1); err != nil {
			t.Fatalf("S3 pagination put %d: %v", i, err)
		}
		createdKeys = append(createdKeys, paginationKey)
	}
	objects, err = store.List(ctx, "v1/pagination/")
	if err != nil {
		t.Fatalf("S3 pagination list: %v", err)
	}
	if len(objects) != paginationCount {
		t.Fatalf("S3 pagination returned %d objects, want %d", len(objects), paginationCount)
	}
}

func integrationEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		t.Fatalf("%s must be set when S3 integration is enabled", name)
	}
	return value
}

func containsObject(objects []ObjectInfo, key string) bool {
	for _, object := range objects {
		if object.Key == key {
			return true
		}
	}
	return false
}
