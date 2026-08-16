package remote

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 implements enough of the API to run the shared contract suite, so the
// S3 driver is held to exactly the same behaviour as the directory one without
// needing an account.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	bucket  string

	// maxKeys forces pagination when set, so the driver's handling of
	// continuation tokens is exercised rather than assumed.
	maxKeys int
	// requests counts calls, letting a test assert that pagination happened.
	requests int
	// failList makes only listing fail, so a probe that stops midway can be
	// observed.
	failList bool
	// status, when set, replaces the response for every request.
	status int
}

func newFakeS3(t *testing.T) (*fakeS3, *S3) {
	t.Helper()

	f := &fakeS3{objects: map[string][]byte{}, bucket: "test-bucket"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	s, err := NewS3(S3Config{
		Endpoint:   srv.URL,
		Region:     "us-east-1",
		Bucket:     f.bucket,
		AccessKey:  exampleAccessKey,
		SecretKey:  exampleSecretKey,
		PathStyle:  true,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++

	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}

	// Verify the signature rather than merely noting one is present. This
	// turns the whole contract suite into a signing test: any request whose
	// canonical form we build differently from what we send is rejected here,
	// which is exactly how the real service would answer.
	if err := verifySignature(r); err != nil {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>SignatureDoesNotMatch</Code></Error>"))
		return
	}

	path := strings.TrimPrefix(r.URL.EscapedPath(), "/"+f.bucket)
	key, err := url.PathUnescape(strings.TrimPrefix(path, "/"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		if f.failList {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.list(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	case http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	token := r.URL.Query().Get("continuation-token")

	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	start := 0
	if token != "" {
		for i, k := range keys {
			if k == token {
				start = i
				break
			}
		}
	}

	limit := len(keys)
	if f.maxKeys > 0 && start+f.maxKeys < limit {
		limit = start + f.maxKeys
	}
	page := keys[start:limit]
	truncated := limit < len(keys)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	fmt.Fprintf(&b, "<IsTruncated>%v</IsTruncated>", truncated)
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", keys[limit])
	}
	for _, k := range page {
		fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-08-11T00:00:00.000Z</LastModified></Contents>",
			k, len(f.objects[k]))
	}
	b.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

// verifySignature recomputes the signature the way the service does: from the
// headers the request says it signed, the path and query as received, and the
// payload hash it declared.
func verifySignature(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, sigAlgorithm+" ") {
		return fmt.Errorf("no %s authorization", sigAlgorithm)
	}

	var credential, signedHeaders, signature string
	for _, part := range strings.Split(strings.TrimPrefix(auth, sigAlgorithm+" "), ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			credential = strings.TrimPrefix(part, "Credential=")
		case strings.HasPrefix(part, "SignedHeaders="):
			signedHeaders = strings.TrimPrefix(part, "SignedHeaders=")
		case strings.HasPrefix(part, "Signature="):
			signature = strings.TrimPrefix(part, "Signature=")
		}
	}

	scopeParts := strings.SplitN(credential, "/", 2)
	if len(scopeParts) != 2 {
		return fmt.Errorf("malformed credential %q", credential)
	}
	scope := scopeParts[1]
	fields := strings.Split(scope, "/")
	if len(fields) != 4 {
		return fmt.Errorf("malformed scope %q", scope)
	}
	dateStamp, region, service := fields[0], fields[1], fields[2]

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		return errors.New("no payload hash")
	}

	// Only the headers the request claims to have signed, in the order given.
	var block strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		block.WriteString(name)
		block.WriteByte(':')
		block.WriteString(strings.Join(strings.Fields(value), " "))
		block.WriteByte('\n')
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQuery(r.URL),
		block.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		sigAlgorithm,
		r.Header.Get("X-Amz-Date"),
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	want := hex.EncodeToString(hmacSHA256(signingKey(exampleSecretKey, dateStamp, region, service), stringToSign))
	if want != signature {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

func TestS3Contract(t *testing.T) {
	runContract(t, func(t *testing.T) Remote {
		t.Helper()
		_, s := newFakeS3(t)
		return s
	})
}

func TestS3FollowsPagination(t *testing.T) {
	f, s := newFakeS3(t)
	f.maxKeys = 2

	for i := 0; i < 7; i++ {
		key := fmt.Sprintf("v1/projects/p/sessions/s/dev/%06d", i)
		if err := s.Put(context.Background(), key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List(context.Background(), "v1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Stopping at the first page would hide every shard past it, which the
	// sync layer reads as a gap in the session.
	if len(got) != 7 {
		t.Errorf("got %d objects, want 7 - pagination was not followed", len(got))
	}
}

func TestS3MapsOnly404ToNotFound(t *testing.T) {
	// Reporting a transport or server failure as absence tells the sync layer
	// another device pushed nothing, which turns a fast-forward into a fork.
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusForbidden,
		http.StatusUnauthorized,
		http.StatusBadGateway,
		http.StatusTooManyRequests,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f, s := newFakeS3(t)
			f.status = status

			_, err := s.Get(context.Background(), "v1/a")
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("status %d was reported as absence: %v", status, err)
			}
		})
	}

	f, s := newFakeS3(t)
	f.status = http.StatusNotFound
	if _, err := s.Get(context.Background(), "v1/a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("404 should map to ErrNotFound, got %v", err)
	}
}

func TestS3TransportFailureIsNotAbsence(t *testing.T) {
	s, err := NewS3(S3Config{
		Endpoint:  "http://127.0.0.1:1",
		Bucket:    "b",
		AccessKey: "a",
		SecretKey: "s",
		PathStyle: true,
		HTTPClient: &http.Client{
			Timeout: 500 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, getErr := s.Get(context.Background(), "v1/a")
	if getErr == nil {
		t.Fatal("expected an error, got none")
	}
	if errors.Is(getErr, ErrNotFound) {
		t.Errorf("an unreachable endpoint was reported as absence: %v", getErr)
	}
	// The message has to tell the user what to do next (code_style §2.3).
	if !strings.Contains(getErr.Error(), "doctor") {
		t.Errorf("error should point at a next step, got %v", getErr)
	}
	// And it must not leak the secret.
	if strings.Contains(getErr.Error(), "s") && strings.Contains(getErr.Error(), "SecretKey") {
		t.Errorf("error mentions credentials: %v", getErr)
	}
}

func TestS3DeleteIsIdempotentWhenAbsent(t *testing.T) {
	f, s := newFakeS3(t)
	f.status = http.StatusNotFound

	if err := s.Delete(context.Background(), "v1/never-existed"); err != nil {
		t.Errorf("deleting an absent object should succeed, got %v", err)
	}
}

func TestS3AppliesAndStripsThePrefix(t *testing.T) {
	f, s := newFakeS3(t)
	s.cfg.Prefix = "team/alice"

	if err := s.Put(context.Background(), "v1/a", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}

	// Stored under the configured prefix so one bucket can hold more than us.
	f.mu.Lock()
	_, stored := f.objects["team/alice/v1/a"]
	f.mu.Unlock()
	if !stored {
		t.Errorf("object was not stored under the prefix: %v", f.objects)
	}

	// But callers see the keys they wrote.
	got := keysOf(t, s, "v1/")
	if len(got) != 1 || got[0] != "v1/a" {
		t.Errorf("got %v, want the unprefixed key", got)
	}
}

func TestS3URLConstruction(t *testing.T) {
	tests := []struct {
		name      string
		pathStyle bool
		key       string
		want      string
	}{
		{
			name:      "path style puts the bucket in the path",
			pathStyle: true,
			key:       "v1/a",
			want:      "https://s3.example.com/bucket/v1/a",
		},
		{
			name:      "virtual host puts the bucket in the host",
			pathStyle: false,
			key:       "v1/a",
			want:      "https://bucket.s3.example.com/v1/a",
		},
		{
			name:      "characters needing escapes are encoded, separators are not",
			pathStyle: true,
			key:       "v1/a b/c",
			want:      "https://s3.example.com/bucket/v1/a%20b/c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewS3(S3Config{
				Endpoint:  "https://s3.example.com",
				Bucket:    "bucket",
				AccessKey: "a",
				SecretKey: "s",
				PathStyle: tt.pathStyle,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := s.urlFor(tt.key).String(); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestNewS3Validates(t *testing.T) {
	valid := S3Config{Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "a", SecretKey: "s"}

	tests := map[string]func(c *S3Config){
		"no endpoint":      func(c *S3Config) { c.Endpoint = "  " },
		"no bucket":        func(c *S3Config) { c.Bucket = "" },
		"no access key":    func(c *S3Config) { c.AccessKey = "" },
		"no secret key":    func(c *S3Config) { c.SecretKey = "" },
		"endpoint no host": func(c *S3Config) { c.Endpoint = "not-a-url" },
		"unsafe prefix":    func(c *S3Config) { c.Prefix = "../escape" },
	}
	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			break_(&cfg)
			if _, err := NewS3(cfg); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}

	// A missing region defaults rather than failing, since most S3-compatible
	// providers ignore it.
	s, err := NewS3(valid)
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.Region == "" {
		t.Error("region should have a default")
	}
}

func TestS3RefusesAShortBody(t *testing.T) {
	// A body shorter than promised would be stored as a complete object. For a
	// shard that means silently losing its tail.
	_, s := newFakeS3(t)
	if err := s.Put(context.Background(), "v1/short", strings.NewReader("abc"), 10); err == nil {
		t.Fatal("expected an error, got none")
	}
	if _, err := s.Stat(context.Background(), "v1/short"); !errors.Is(err, ErrNotFound) {
		t.Error("a refused write stored an object")
	}
}

func TestS3RefusesAnOversizeObject(t *testing.T) {
	_, s := newFakeS3(t)
	if err := s.Put(context.Background(), "v1/big", strings.NewReader("x"), maxObjectSize+1); err == nil {
		t.Error("expected an error for an oversize object")
	}
}

func TestS3Probe(t *testing.T) {
	f, s := newFakeS3(t)

	if err := s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// The probe cleans up after itself.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objects) != 0 {
		t.Errorf("probe left objects behind: %v", f.objects)
	}
}

func TestS3ProbeReportsWhatFailed(t *testing.T) {
	f, s := newFakeS3(t)
	f.status = http.StatusForbidden

	err := s.Probe(context.Background())
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "write to the bucket") {
		t.Errorf("error should name the failing step, got %v", err)
	}
}

func TestS3DistinguishesAMissingBucketFromAMissingObject(t *testing.T) {
	// Both answer 404. Reporting a misnamed bucket as "object not found" tells
	// the sync layer the other device pushed nothing, and tells the user
	// nothing about the configuration that is actually wrong.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<Error><Code>NoSuchBucket</Code><Message>no</Message></Error>"))
	}))
	defer srv.Close()

	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "typo", AccessKey: "a", SecretKey: "s",
		PathStyle: true, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, getErr := s.Get(context.Background(), "v1/a")
	if getErr == nil {
		t.Fatal("expected an error, got none")
	}
	if errors.Is(getErr, ErrNotFound) {
		t.Errorf("a missing bucket was reported as a missing object: %v", getErr)
	}
	if !strings.Contains(getErr.Error(), "bucket name") {
		t.Errorf("error should point at the configuration, got %v", getErr)
	}
}

func TestS3TruncatedListingWithoutATokenIsAnError(t *testing.T) {
	// Returning the short page would look exactly like the end of the bucket,
	// and the sync layer would read the missing shards as a gap in the session
	// rather than as a listing that was cut short.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated>` +
			`<Contents><Key>v1/a</Key><Size>1</Size></Contents></ListBucketResult>`))
	}))
	defer srv.Close()

	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		PathStyle: true, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.List(context.Background(), "v1/"); err == nil {
		t.Fatal("a truncated listing was returned as if complete")
	}
}

func TestS3ListStopsInsteadOfLoopingForever(t *testing.T) {
	// A provider that keeps handing back a token would otherwise never
	// terminate, and the per-request timeout does not bound the loop.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>true</IsTruncated>` +
			`<NextContinuationToken>always-more</NextContinuationToken></ListBucketResult>`))
	}))
	defer srv.Close()

	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		PathStyle: true, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.List(context.Background(), "v1/")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("an endless listing returned success")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("List did not terminate")
	}
}

func TestS3QueryIsEncodedTheWayItIsSigned(t *testing.T) {
	// url.Values.Encode writes a space as "+" while the canonical form uses
	// "%20". Signing one and sending the other is rejected with
	// SignatureDoesNotMatch, and a configured prefix may legitimately contain
	// a space.
	got := encodeQuery([][2]string{
		{"prefix", "team alice/v1/"},
		{"list-type", "2"},
	})
	if strings.Contains(got, "+") {
		t.Errorf("a space was encoded as +, which is signed differently: %q", got)
	}
	if !strings.Contains(got, "team%20alice%2Fv1%2F") {
		t.Errorf("got %q", got)
	}
	// Parameters are sorted, as the signature requires.
	if !strings.HasPrefix(got, "list-type=") {
		t.Errorf("parameters not sorted: %q", got)
	}
}

func TestS3ListWithASpaceInThePrefixIsAccepted(t *testing.T) {
	// End to end through the fake, which verifies the signature: a mismatch
	// between the sent and signed query would be rejected here.
	f, s := newFakeS3(t)
	s.cfg.Prefix = "team alice"

	if err := s.Put(context.Background(), "v1/a", strings.NewReader("x"), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.List(context.Background(), "v1/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Key != "v1/a" {
		t.Errorf("got %v (stored: %v)", got, f.objects)
	}
}

func TestS3ProbeCleansUpWhenAStepFails(t *testing.T) {
	// A probe that leaves its own object behind has not verified cleanliness,
	// and it would sit in the user's bucket permanently.
	f, s := newFakeS3(t)
	f.failList = true

	if err := s.Probe(context.Background()); err == nil {
		t.Fatal("expected an error, got none")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.objects) != 0 {
		t.Errorf("the probe left objects behind: %v", f.objects)
	}
}

func TestS3ListDropsKeysThatAreNotOurs(t *testing.T) {
	// A shared bucket can hold keys we would never write. Handing them upwards
	// would put an unvalidated string where a path is later built.
	f, s := newFakeS3(t)
	f.mu.Lock()
	f.objects["v1/good"] = []byte("x")
	f.objects["v1/../escape"] = []byte("x")
	f.objects[`v1\backslash`] = []byte("x")
	f.mu.Unlock()

	got := keysOf(t, s, "")
	if len(got) != 1 || got[0] != "v1/good" {
		t.Errorf("got %v, want only the valid key", got)
	}
}

func TestS3ListRejectsMalformedXML(t *testing.T) {
	// A truncated or corrupt listing must be an error, not an empty store -
	// an empty store means "the other device pushed nothing".
	f := &fakeS3{objects: map[string][]byte{}, bucket: "b"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<ListBucketResult><Contents>"))
	}))
	defer srv.Close()
	_ = f

	s, err := NewS3(S3Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s",
		PathStyle: true, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.List(context.Background(), "v1/"); err == nil {
		t.Fatal("expected an error, got none")
	}
}
