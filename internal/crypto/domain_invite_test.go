package crypto

import (
	"errors"
	"testing"
)

func TestDomainInviteProofIsStableAndKeyed(t *testing.T) {
	key := []byte("identifier-key")
	payload := []byte(`{"domain":"example"}`)

	first, err := DomainInviteProof(key, payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DomainInviteProof(key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same payload produced different proofs: %q and %q", first, second)
	}
	if err := VerifyDomainInviteProof(key, payload, first); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if err := VerifyDomainInviteProof([]byte("other-key"), payload, first); !errors.Is(err, ErrInvalidDomainInviteProof) {
		t.Fatalf("wrong key error = %v, want ErrInvalidDomainInviteProof", err)
	}
	if err := VerifyDomainInviteProof(key, []byte(`{"domain":"other"}`), first); !errors.Is(err, ErrInvalidDomainInviteProof) {
		t.Fatalf("wrong payload error = %v, want ErrInvalidDomainInviteProof", err)
	}
}

func TestDomainInviteProofRejectsMalformedInputs(t *testing.T) {
	if _, err := DomainInviteProof(nil, []byte("payload")); err == nil {
		t.Fatal("empty key was accepted")
	}
	if _, err := DomainInviteProof([]byte("key"), nil); err == nil {
		t.Fatal("empty payload was accepted")
	}
	if err := VerifyDomainInviteProof([]byte("key"), []byte("payload"), "not-base64"); !errors.Is(err, ErrInvalidDomainInviteProof) {
		t.Fatalf("malformed proof error = %v, want ErrInvalidDomainInviteProof", err)
	}
}
