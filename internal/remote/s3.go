package remote

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// s3Service is the service name in the signing scope.
const s3Service = "s3"

// defaultTimeout bounds a single request. The tool must fail fast when a
// backend is unreachable rather than hang (§11.2).
const defaultTimeout = 30 * time.Second

// maxObjectSize bounds what will be read into memory.
//
// Signing requires the payload hash up front, and streaming would mean chunked
// signing for no benefit: shard sizes are chosen by the layer above precisely
// so they stay small. The limit exists so a corrupt or hostile listing cannot
// make us allocate without bound.
const maxObjectSize = 256 << 20

// maxListPages bounds a listing so a provider that keeps returning a
// continuation token cannot loop forever.
const maxListPages = 10_000

// S3Config describes one S3-compatible bucket.
type S3Config struct {
	// Endpoint is the service URL, for example https://s3.amazonaws.com or a
	// provider's own host.
	Endpoint string
	// Region is the signing region.
	Region string
	// Bucket holds the objects.
	Bucket string
	// Prefix is prepended to every key, so one bucket can hold more than this.
	Prefix string
	// AccessKey and SecretKey authenticate requests. They are never logged.
	AccessKey string
	SecretKey string
	// PathStyle addresses the bucket as a path segment rather than a
	// subdomain. Most S3-compatible providers and MinIO require it, so it is
	// the default.
	PathStyle bool
	// HTTPClient overrides the client, for tests.
	HTTPClient *http.Client
}

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	cfg    S3Config
	base   *url.URL
	client *http.Client
	now    func() time.Time
}

// NewS3 validates a configuration and returns a ready backend.
func NewS3(cfg S3Config) (*S3, error) {
	switch {
	case strings.TrimSpace(cfg.Endpoint) == "":
		return nil, errors.New("remote: no endpoint configured")
	case strings.TrimSpace(cfg.Bucket) == "":
		return nil, errors.New("remote: no bucket configured")
	case strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "":
		return nil, errors.New("remote: no credentials configured")
	}

	base, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("remote: endpoint %q needs a scheme and a host", cfg.Endpoint)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Prefix != "" {
		if err := ValidatePrefix(cfg.Prefix); err != nil {
			return nil, fmt.Errorf("configured prefix: %w", err)
		}
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &S3{cfg: cfg, base: base, client: client, now: time.Now}, nil
}

// Name identifies this backend in configuration and diagnostics.
func (s *S3) Name() string { return "s3" }

// objectKey applies the configured prefix.
func (s *S3) objectKey(key string) string {
	if s.cfg.Prefix == "" {
		return key
	}
	return strings.TrimRight(s.cfg.Prefix, "/") + "/" + key
}

// stripPrefix reverses objectKey so callers see the keys they wrote.
func (s *S3) stripPrefix(full string) string {
	if s.cfg.Prefix == "" {
		return full
	}
	return strings.TrimPrefix(full, strings.TrimRight(s.cfg.Prefix, "/")+"/")
}

// urlFor builds the request URL for an object key.
func (s *S3) urlFor(key string) *url.URL {
	u := *s.base
	encoded := uriEncode(key, true)

	if s.cfg.PathStyle || s.base.Host == "" {
		u.Path = "/" + s.cfg.Bucket
		if key != "" {
			u.Path += "/" + key
		}
		u.RawPath = "/" + s.cfg.Bucket
		if key != "" {
			u.RawPath += "/" + encoded
		}
		return &u
	}

	u.Host = s.cfg.Bucket + "." + s.base.Host
	u.Path = "/" + key
	u.RawPath = "/" + encoded
	return &u
}

// do signs and performs a request, returning the response for the caller to
// close.
func (s *S3) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}

	payloadHash := emptyPayloadHash
	if body != nil {
		payloadHash = hashHex(body)
	}
	sign(req, credentials{AccessKey: s.cfg.AccessKey, SecretKey: s.cfg.SecretKey},
		s.cfg.Region, s3Service, payloadHash, s.now())

	resp, err := s.client.Do(req)
	if err != nil {
		// Deliberately not ErrNotFound. Reporting a transport failure as
		// absence would tell the sync layer another device pushed nothing,
		// which is how a fast-forward turns into a fork.
		return nil, fmt.Errorf("cannot reach bucket %q: check the endpoint, network and credentials with 'agentsync doctor': %w",
			s.cfg.Bucket, err)
	}
	return resp, nil
}

// s3Error is the error document S3 returns alongside a failing status.
type s3Error struct {
	Code string `xml:"Code"`
}

// errorCode reads the provider's error code, or "" if there is none. Only a
// bounded amount is read, and only the code is used - the document also carries
// the key and a request id, which must not reach diagnostics.
func errorCode(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return ""
	}
	var doc s3Error
	if xml.Unmarshal(body, &doc) != nil {
		return ""
	}
	return doc.Code
}

// checkStatus turns a response status into an error.
//
// Only a missing *object* becomes ErrNotFound. A 404 also answers a missing or
// misnamed bucket and a wrong endpoint, and reporting those as "object not
// found" would tell the sync layer the other device pushed nothing - which is
// how a fast-forward turns into a fork - while telling the user nothing about
// the configuration that is actually wrong.
//
// A HEAD request carries no body, so a bucket-level 404 cannot be told apart
// there. That is acceptable: the bucket is verified by Probe during setup, and
// Stat is only reached afterwards.
func (s *S3) checkStatus(resp *http.Response, key string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	code := errorCode(resp)

	switch {
	case resp.StatusCode == http.StatusNotFound && (code == "" || code == "NoSuchKey"):
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("storage rejected the request with %s: check the bucket name and endpoint with 'agentsync doctor'", code)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return errors.New("access denied: check the credentials and the bucket policy with 'agentsync doctor'")
	default:
		return fmt.Errorf("storage returned %s: retry, or check the bucket with 'agentsync doctor'", resp.Status)
	}
}

// Get returns the object body.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}

	resp, err := s.do(ctx, http.MethodGet, s.urlFor(s.objectKey(key)), nil)
	if err != nil {
		return nil, err
	}
	if err := s.checkStatus(resp, key); err != nil {
		resp.Body.Close() //nolint:errcheck // already failing
		return nil, err
	}
	return resp.Body, nil
}

// Put stores size bytes read from r.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if size > maxObjectSize {
		return fmt.Errorf("remote: object %q is %d bytes, larger than the %d byte limit", key, size, maxObjectSize)
	}

	body, err := io.ReadAll(io.LimitReader(r, maxObjectSize+1))
	if err != nil {
		return fmt.Errorf("read object body: %w", err)
	}
	if int64(len(body)) > maxObjectSize {
		return fmt.Errorf("remote: object %q exceeds the %d byte limit", key, maxObjectSize)
	}
	// A body shorter than promised would be stored as a complete object. For a
	// shard that means silently losing its tail.
	if size >= 0 && int64(len(body)) != size {
		return fmt.Errorf("remote: object %q is %d bytes, expected %d", key, len(body), size)
	}

	resp, err := s.do(ctx, http.MethodPut, s.urlFor(s.objectKey(key)), body)
	if err != nil {
		return err
	}
	defer drain(resp)
	return s.checkStatus(resp, key)
}

// Delete removes the object. A key that is not there is not an error, so
// cleanup is idempotent.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}

	resp, err := s.do(ctx, http.MethodDelete, s.urlFor(s.objectKey(key)), nil)
	if err != nil {
		return err
	}
	defer drain(resp)

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return s.checkStatus(resp, key)
}

// Stat returns metadata without transferring the object.
func (s *S3) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}

	resp, err := s.do(ctx, http.MethodHead, s.urlFor(s.objectKey(key)), nil)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer drain(resp)

	if err := s.checkStatus(resp, key); err != nil {
		return ObjectInfo{}, err
	}

	info := ObjectInfo{Key: key, Size: resp.ContentLength}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		info.ModTime = t
	}
	return info, nil
}

// listResult is the subset of ListObjectsV2 output that matters here.
type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
}

// List returns every object under prefix, following pagination to the end.
func (s *S3) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}

	var out []ObjectInfo
	token := ""

	for page := 0; ; page++ {
		// A provider that keeps handing back a token would otherwise loop
		// forever, growing the result without bound; the client timeout
		// applies per request, not to the loop.
		if page >= maxListPages {
			return nil, fmt.Errorf("listing %q did not finish after %d pages: check the bucket with 'agentsync doctor'",
				prefix, maxListPages)
		}

		u := s.urlFor("")
		params := [][2]string{
			{"list-type", "2"},
			{"prefix", s.objectKey(prefix)},
		}
		if token != "" {
			params = append(params, [2]string{"continuation-token", token})
		}
		// Built with the same encoding the signature uses. url.Values.Encode
		// writes a space as "+" while the canonical form uses "%20", so a
		// prefix containing a space would be signed differently from how it is
		// sent and rejected with SignatureDoesNotMatch.
		u.RawQuery = encodeQuery(params)

		resp, err := s.do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if err := s.checkStatus(resp, prefix); err != nil {
			drain(resp)
			return nil, err
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectSize))
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("read listing: %w", err)
		}

		var result listResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse listing: %w", err)
		}
		for _, item := range result.Contents {
			key := s.stripPrefix(item.Key)
			// A listing is another place an external string becomes a key.
			// Ours are always valid, so anything else was written by something
			// other than us and is not ours to hand upwards.
			if ValidateKey(key) != nil {
				continue
			}
			out = append(out, ObjectInfo{Key: key, Size: item.Size, ModTime: item.LastModified})
		}

		if !result.IsTruncated {
			return out, nil
		}
		// Truncated with nowhere to continue from. Returning what arrived would
		// look exactly like the end of the bucket, and the sync layer would
		// read the missing shards as a gap in the session rather than as a
		// listing that was cut short.
		if result.NextContinuationToken == "" {
			return nil, fmt.Errorf("listing %q was truncated without a continuation token: the storage provider's response is incomplete", prefix)
		}
		token = result.NextContinuationToken
	}
}

// encodeQuery renders parameters the way the signature expects them.
func encodeQuery(params [][2]string) string {
	sorted := make([][2]string, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0] < sorted[j][0] })

	parts := make([]string, len(sorted))
	for i, p := range sorted {
		parts[i] = uriEncode(p[0], false) + "=" + uriEncode(p[1], false)
	}
	return strings.Join(parts, "&")
}

// Probe verifies the bucket is reachable and writable, so a misconfiguration
// surfaces during setup rather than during the first sync (§9.1).
func (s *S3) Probe(ctx context.Context) (err error) {
	const body = "probe"

	if putErr := s.Put(ctx, probeKey, strings.NewReader(body), int64(len(body))); putErr != nil {
		return fmt.Errorf("cannot write to the bucket: %w", putErr)
	}

	// Once the object exists it must be removed whatever happens next.
	// Returning early on a failed read would leave our probe in the user's
	// bucket permanently, and Prober's contract says it cleans up after itself.
	defer func() {
		if delErr := s.Delete(ctx, probeKey); delErr != nil && err == nil {
			err = fmt.Errorf("cannot delete from the bucket: %w", delErr)
		}
	}()

	r, getErr := s.Get(ctx, probeKey)
	if getErr != nil {
		return fmt.Errorf("cannot read back from the bucket: %w", getErr)
	}
	if closeErr := r.Close(); closeErr != nil {
		return fmt.Errorf("cannot read back from the bucket: %w", closeErr)
	}
	if _, listErr := s.List(ctx, ""); listErr != nil {
		return fmt.Errorf("cannot list the bucket: %w", listErr)
	}
	return nil
}

// drain closes a response body after reading what remains, so the connection
// can be reused rather than dropped.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
