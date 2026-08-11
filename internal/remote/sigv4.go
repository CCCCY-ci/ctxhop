package remote

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, implemented here rather than taken as a dependency.
//
// We use five plain REST operations, so the vast majority of an SDK would be
// unused, and the project ships one small static binary with no third-party
// code. The failure mode also favours it: a signing mistake is rejected with a
// 403, loudly and immediately, unlike the silent corruption the adapter layer
// has to defend against.
//
// The algorithm has not changed since 2012 and AWS publishes test vectors,
// which the tests assert against.

const (
	sigAlgorithm  = "AWS4-HMAC-SHA256"
	sigTerminator = "aws4_request"

	// emptyPayloadHash is SHA-256 of the empty string, needed for requests
	// with no body.
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// credentials are the secrets used to sign a request.
type credentials struct {
	AccessKey string
	SecretKey string
}

// sign adds the headers a request needs to be accepted.
//
// payloadHash must be the hex SHA-256 of the exact bytes being sent. S3
// requires it as a header as well as inside the signature, which is what lets
// it verify the body was not altered in transit.
func sign(req *http.Request, creds credentials, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	canonicalHeaders, signedHeaders := canonicalizeHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, sigTerminator}, "/")
	stringToSign := strings.Join([]string{
		sigAlgorithm,
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretKey, dateStamp, region, service), stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, creds.AccessKey, scope, signedHeaders, signature))
}

// signingKey derives the request-specific key by chaining HMACs over the date,
// region and service, so a leaked signing key is useless outside that scope.
func signingKey(secret, dateStamp, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, sigTerminator)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalizeHeaders returns the signed headers block and the list of header
// names it covers, both in the lowercase sorted form the algorithm requires.
func canonicalizeHeaders(req *http.Request) (string, string) {
	names := make([]string, 0, len(req.Header))
	values := make(map[string]string, len(req.Header))

	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		// Content-Length is not signed: Go sets it after signing, and S3 does
		// not require it in the signature.
		if lower == "content-length" || lower == "authorization" || lower == "user-agent" {
			continue
		}
		names = append(names, lower)

		trimmed := make([]string, len(vals))
		for i, v := range vals {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		values[lower] = strings.Join(trimmed, ",")
	}
	sort.Strings(names)

	var block strings.Builder
	for _, name := range names {
		block.WriteString(name)
		block.WriteByte(':')
		block.WriteString(values[name])
		block.WriteByte('\n')
	}
	return block.String(), strings.Join(names, ";")
}

// canonicalURI encodes the path one segment at a time, keeping separators.
//
// S3 is the exception among AWS services: it does not double-encode the path,
// so the already-encoded form is what gets signed. Encoding it twice produces a
// signature the service will reject for every key containing a character that
// needs escaping.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery sorts parameters and encodes them with the same rules as the
// path, which are stricter than Go's default query encoding.
func canonicalQuery(u *url.URL) string {
	values := u.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vals := values[k]
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes per RFC 3986, which differs from Go's url escaping
// in its treatment of several characters. keepSlash leaves `/` alone, which is
// correct for paths and wrong for query values.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
