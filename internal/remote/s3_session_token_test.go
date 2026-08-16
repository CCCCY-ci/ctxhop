package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3SignsSessionToken(t *testing.T) {
	const token = "temporary-session-token"
	var gotToken string
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Amz-Security-Token")
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := NewS3(S3Config{
		Endpoint:     server.URL,
		Bucket:       "bucket",
		AccessKey:    "access",
		SecretKey:    "secret",
		SessionToken: token,
		PathStyle:    true,
		HTTPClient:   server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	body, err := store.Get(context.Background(), "v1/object")
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	if gotToken != token {
		t.Fatalf("session token = %q, want %q", gotToken, token)
	}
	if !strings.Contains(gotAuthorization, "x-amz-security-token") {
		t.Fatalf("authorization did not sign the session token header: %q", gotAuthorization)
	}
}
