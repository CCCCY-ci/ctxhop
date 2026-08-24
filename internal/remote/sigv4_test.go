package remote

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// AWS publishes worked examples for S3 request signing. Asserting against them
// is the only way to know the implementation is right without a live account -
// and a signature that is merely plausible is rejected with a 403 that says
// nothing about which part was wrong.
const (
	exampleAccessKey = "AKIAIOSFODNN7EXAMPLE"
	exampleSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	exampleRegion    = "us-east-1"
)

func exampleTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2013-05-24T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestSignMatchesTheAWSExample(t *testing.T) {
	// GET Object with a Range header, from AWS's "Examples: Signature
	// Calculations" documentation.
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")

	sign(req, credentials{exampleAccessKey, exampleSecretKey},
		exampleRegion, s3Service, emptyPayloadHash, exampleTime(t))

	auth := req.Header.Get("Authorization")
	const wantSig = "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if !strings.Contains(auth, wantSig) {
		t.Errorf("Authorization = %q\nwant it to contain %q", auth, wantSig)
	}

	const wantScope = "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request"
	if !strings.Contains(auth, wantScope) {
		t.Errorf("Authorization = %q\nwant it to contain %q", auth, wantScope)
	}
	if !strings.Contains(auth, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date") {
		t.Errorf("signed headers wrong: %q", auth)
	}
}

func TestSignSetsTheHeadersS3Requires(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "https://example.com/bucket/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := hashHex([]byte("body"))

	sign(req, credentials{exampleAccessKey, exampleSecretKey}, "eu-west-1", s3Service, payload, exampleTime(t))

	// S3 verifies the body against this header, so it has to carry the hash of
	// the exact bytes sent, not of an empty payload.
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != payload {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", got, payload)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	if req.Header.Get("Host") == "" {
		t.Error("Host must be signed")
	}
}

func TestSigningKeyIsScoped(t *testing.T) {
	// The chain is what makes a derived key useless outside its date, region
	// and service. Any two differing inputs must produce different keys.
	base := signingKey(exampleSecretKey, "20130524", "us-east-1", "s3")
	for _, other := range [][]byte{
		signingKey(exampleSecretKey, "20130525", "us-east-1", "s3"),
		signingKey(exampleSecretKey, "20130524", "eu-west-1", "s3"),
		signingKey(exampleSecretKey, "20130524", "us-east-1", "s3x"),
		signingKey("other", "20130524", "us-east-1", "s3"),
	} {
		if string(base) == string(other) {
			t.Error("two different scopes produced the same signing key")
		}
	}
}

func TestURIEncode(t *testing.T) {
	tests := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"simple", true, "simple"},
		{"a/b", true, "a/b"},
		{"a/b", false, "a%2Fb"},
		{"a b", true, "a%20b"},
		{"a+b", true, "a%2Bb"},
		{"~-._", true, "~-._"},
		{"ä", true, "%C3%A4"},
		{"100%", true, "100%25"},
		{"a=b&c", false, "a%3Db%26c"},
	}
	for _, tt := range tests {
		if got := uriEncode(tt.in, tt.keepSlash); got != tt.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", tt.in, tt.keepSlash, got, tt.want)
		}
	}
}

func TestCanonicalQuerySortsAndEncodes(t *testing.T) {
	u, err := url.Parse("https://example.com/?prefix=v1/a&list-type=2&continuation-token=a+b")
	if err != nil {
		t.Fatal(err)
	}
	got := canonicalQuery(u)
	want := "continuation-token=a%20b&list-type=2&prefix=v1%2Fa"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestCanonicalURIKeepsTheEncodedPath(t *testing.T) {
	// S3 does not double-encode the path. Encoding it a second time yields a
	// signature the service rejects for every key needing an escape.
	u, err := url.Parse("https://example.com/bucket/a%20b/c")
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalURI(u); got != "/bucket/a%20b/c" {
		t.Errorf("got %q", got)
	}

	empty, err := url.Parse("https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalURI(empty); got != "/" {
		t.Errorf("empty path = %q, want /", got)
	}
}

func TestCanonicalHeadersAreNormalized(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Meta", "  spaced   out  ")
	req.Header.Set("Content-Length", "123")
	req.Header.Set("User-Agent", "should-not-be-signed")
	req.Header.Set("Host", "example.com")

	block, signed := canonicalizeHeaders(req)

	if !strings.Contains(block, "x-amz-meta:spaced out\n") {
		t.Errorf("whitespace not folded: %q", block)
	}
	// Go rewrites Content-Length after signing and adds a User-Agent, so
	// signing them would produce a signature that never matches what is sent.
	if strings.Contains(signed, "content-length") || strings.Contains(signed, "user-agent") {
		t.Errorf("signed headers include ones we do not control: %q", signed)
	}
	if !strings.HasPrefix(signed, "host;") {
		t.Errorf("host must be signed and sorted first here: %q", signed)
	}
}
