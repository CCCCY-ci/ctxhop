package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
)

// TestS3Integration is opt-in so ordinary unit and CI runs never contact an
// external service. It uses only synthetic bytes and records each key it
// writes, so cleanup deletes only objects created by this test invocation.
func TestS3Integration(t *testing.T) {
	if os.Getenv("CTXHOP_S3_INTEGRATION") != "1" {
		t.Skip("set CTXHOP_S3_INTEGRATION=1 to run the external S3/R2 acceptance test")
	}

	s3Config := integrationS3Config(t)
	prefix := strings.TrimRight(strings.TrimSpace(s3Config.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	s3Config.Prefix = prefix + fmt.Sprintf("ctxhop-integration/%d-%d", time.Now().UnixNano(), os.Getpid())

	store, err := NewS3(s3Config)
	if err != nil {
		t.Fatalf("create S3 Remote: %v", err)
	}

	createdKeys := make([]string, 0, 1)
	var createdKeysMu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancelCleanup()
		createdKeysMu.Lock()
		keys := append([]string(nil), createdKeys...)
		createdKeysMu.Unlock()
		if cleanupErr := runS3IntegrationOperations(cleanupCtx, len(keys), false, func(ctx context.Context, index int) error {
			return store.Delete(ctx, keys[index])
		}); cleanupErr != nil {
			t.Logf("S3 integration cleanup failed: %v", cleanupErr)
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

	if os.Getenv("CTXHOP_S3_INTEGRATION_PAGINATION") != "1" {
		return
	}

	const paginationCount = 1001
	if err := runS3IntegrationOperations(ctx, paginationCount, true, func(ctx context.Context, index int) error {
		paginationKey := fmt.Sprintf("v1/pagination/%04d", index)
		if err := store.Put(ctx, paginationKey, strings.NewReader("x"), 1); err != nil {
			return fmt.Errorf("S3 pagination put %d: %w", index, err)
		}
		createdKeysMu.Lock()
		createdKeys = append(createdKeys, paginationKey)
		createdKeysMu.Unlock()
		return nil
	}); err != nil {
		t.Fatalf("S3 pagination puts: %v", err)
	}
	objects, err = store.List(ctx, "v1/pagination/")
	if err != nil {
		t.Fatalf("S3 pagination list: %v", err)
	}
	if len(objects) != paginationCount {
		t.Fatalf("S3 pagination returned %d objects, want %d", len(objects), paginationCount)
	}
}

const s3IntegrationWorkers = 16

func runS3IntegrationOperations(ctx context.Context, count int, stopOnError bool, operation func(context.Context, int) error) error {
	if count == 0 {
		return nil
	}
	workers := s3IntegrationWorkers
	if count < workers {
		workers = count
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	recordError := func(err error) {
		if err == nil {
			return
		}
		firstErrOnce.Do(func() {
			firstErr = err
			if stopOnError {
				cancel()
			}
		})
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if err := workCtx.Err(); err != nil {
					if ctx.Err() != nil {
						recordError(err)
					}
					continue
				}
				recordError(operation(workCtx, index))
			}
		}()
	}

dispatch:
	for index := 0; index < count; index++ {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func TestS3IntegrationLoadsPersistedSettings(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CTXHOP_CONFIG_DIR", configDir)
	t.Setenv("CTXHOP_ACCESS_KEY_ID", "")
	t.Setenv("CTXHOP_SECRET_ACCESS_KEY", "")
	t.Setenv("CTXHOP_SESSION_TOKEN", "")

	c := config.New()
	c.Remote = config.Remote{
		Type:      "s3",
		Endpoint:  "https://account.r2.cloudflarestorage.com",
		Bucket:    "acceptance",
		Region:    "auto",
		Prefix:    "acceptance/r2",
		PathStyle: true,
	}
	if err := c.Save(configDir); err != nil {
		t.Fatal(err)
	}
	secrets := &config.Secrets{
		Credentials: config.Credentials{
			AccessKeyID:     "access-key",
			SecretAccessKey: "secret-key",
			SessionToken:    "session-token",
		},
	}
	if err := config.SaveSecrets(configDir, secrets); err != nil {
		t.Fatal(err)
	}

	got := integrationS3Config(t)
	if got.Endpoint != c.Remote.Endpoint ||
		got.Bucket != c.Remote.Bucket ||
		got.Region != c.Remote.Region ||
		got.Prefix != c.Remote.Prefix ||
		!got.PathStyle ||
		got.AccessKey != secrets.Credentials.AccessKeyID ||
		got.SecretKey != secrets.Credentials.SecretAccessKey ||
		got.SessionToken != secrets.Credentials.SessionToken {
		t.Fatal("S3 integration did not load persisted settings")
	}
}

func TestS3IntegrationConfigMapping(t *testing.T) {
	c := config.New()
	c.Remote = config.Remote{
		Type:      "s3",
		Endpoint:  "https://account.r2.cloudflarestorage.com",
		Bucket:    "acceptance",
		Region:    "auto",
		Prefix:    "acceptance/r2",
		PathStyle: true,
	}
	secrets := &config.Secrets{
		Credentials: config.Credentials{
			AccessKeyID:     "access-key",
			SecretAccessKey: "secret-key",
			SessionToken:    "session-token",
		},
	}

	got, err := s3ConfigFromCtxHopConfig(c, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != c.Remote.Endpoint ||
		got.Bucket != c.Remote.Bucket ||
		got.Region != c.Remote.Region ||
		got.Prefix != c.Remote.Prefix ||
		!got.PathStyle ||
		got.AccessKey != secrets.Credentials.AccessKeyID ||
		got.SecretKey != secrets.Credentials.SecretAccessKey ||
		got.SessionToken != secrets.Credentials.SessionToken {
		t.Fatal("S3 config mapping did not preserve persisted values")
	}
}

func integrationS3Config(t *testing.T) S3Config {
	t.Helper()

	configDir, err := config.Dir()
	if err == nil {
		c, loadErr := config.Load(configDir)
		switch {
		case loadErr == nil && strings.EqualFold(strings.TrimSpace(c.Remote.Type), "s3"):
			secrets, secretsErr := config.LoadSecrets(configDir)
			if secretsErr != nil {
				t.Fatalf("load S3 credentials from CtxHop secrets: %v", secretsErr)
			}
			settings, settingsErr := s3ConfigFromCtxHopConfig(c, secrets)
			if settingsErr != nil {
				t.Fatalf("load S3 settings from CtxHop config: %v", settingsErr)
			}
			return settings
		case loadErr != nil && !errors.Is(loadErr, config.ErrNotInitialised):
			t.Fatalf("load CtxHop config: %v", loadErr)
		}
	}

	return integrationS3ConfigFromEnvironment(t)
}

func s3ConfigFromCtxHopConfig(c *config.Config, secrets *config.Secrets) (S3Config, error) {
	if c == nil {
		return S3Config{}, errors.New("CtxHop configuration is unavailable")
	}
	if !strings.EqualFold(strings.TrimSpace(c.Remote.Type), "s3") {
		return S3Config{}, errors.New("CtxHop configuration does not select the S3 backend")
	}
	if secrets == nil {
		return S3Config{}, errors.New("CtxHop secrets are unavailable")
	}
	if strings.TrimSpace(secrets.Credentials.AccessKeyID) == "" ||
		strings.TrimSpace(secrets.Credentials.SecretAccessKey) == "" {
		return S3Config{}, errors.New("CtxHop S3 credentials are incomplete")
	}
	return S3Config{
		Endpoint:     c.Remote.Endpoint,
		Region:       c.Remote.Region,
		Bucket:       c.Remote.Bucket,
		Prefix:       c.Remote.Prefix,
		AccessKey:    secrets.Credentials.AccessKeyID,
		SecretKey:    secrets.Credentials.SecretAccessKey,
		SessionToken: secrets.Credentials.SessionToken,
		PathStyle:    c.Remote.PathStyle,
	}, nil
}

func integrationS3ConfigFromEnvironment(t *testing.T) S3Config {
	t.Helper()

	pathStyle := false
	if value := strings.TrimSpace(os.Getenv("CTXHOP_S3_PATH_STYLE")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("CTXHOP_S3_PATH_STYLE: %v", err)
		}
		pathStyle = parsed
	}

	return S3Config{
		Endpoint:     integrationEnv(t, "CTXHOP_S3_ENDPOINT"),
		Region:       integrationEnv(t, "CTXHOP_S3_REGION"),
		Bucket:       integrationEnv(t, "CTXHOP_S3_BUCKET"),
		Prefix:       strings.TrimRight(strings.TrimSpace(os.Getenv("CTXHOP_S3_PREFIX")), "/"),
		AccessKey:    integrationEnv(t, "CTXHOP_S3_ACCESS_KEY_ID"),
		SecretKey:    integrationEnv(t, "CTXHOP_S3_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("CTXHOP_S3_SESSION_TOKEN"),
		PathStyle:    pathStyle,
	}
}

func integrationEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		t.Fatalf("%s must be set when S3 integration is enabled and no S3 config is initialized", name)
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
