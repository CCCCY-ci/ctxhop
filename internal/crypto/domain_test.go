package crypto

import (
	"strings"
	"testing"
)

func TestDomainFingerprintIsStableAndNamespaceScoped(t *testing.T) {
	public := []byte("public-identity")
	first, err := DomainFingerprint("s3/https://storage.example/bucket/prefix", public)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DomainFingerprint("s3/https://storage.example/bucket/prefix", public)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same domain produced %q and %q", first, second)
	}
	if len(first) != 26 {
		t.Fatalf("fingerprint length = %d, want 26", len(first))
	}
	if strings.Contains(first, "storage") || strings.Contains(first, "bucket") {
		t.Fatalf("fingerprint leaks namespace text: %q", first)
	}

	otherNamespace, err := DomainFingerprint("s3/https://storage.example/bucket/other", public)
	if err != nil {
		t.Fatal(err)
	}
	if first == otherNamespace {
		t.Fatal("different namespaces produced the same fingerprint")
	}
	otherPublic, err := DomainFingerprint("s3/https://storage.example/bucket/prefix", []byte("other-public"))
	if err != nil {
		t.Fatal(err)
	}
	if first == otherPublic {
		t.Fatal("different public identities produced the same fingerprint")
	}
}

func TestDomainFingerprintValidatesInputs(t *testing.T) {
	for name, namespace := range map[string]string{
		"blank namespace": "",
		"nul namespace":   "namespace\x00part",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DomainFingerprint(namespace, []byte("public")); err == nil {
				t.Fatal("invalid namespace was accepted")
			}
		})
	}
	if _, err := DomainFingerprint("namespace", nil); err == nil {
		t.Fatal("empty public identity was accepted")
	}
}
