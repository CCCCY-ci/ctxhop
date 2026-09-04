package crypto

import (
	"strings"
	"testing"
)

func TestDeriveIdentifierIsStableAndDomainSeparated(t *testing.T) {
	key := strings.Repeat("k", 32)

	first, err := DeriveIdentifier([]byte(key), "hub-v2", "default")
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveIdentifier([]byte(key), "hub-v2", "default")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same input produced %q and %q", first, second)
	}

	otherDomain, err := DeriveIdentifier([]byte(key), "project-v2", "default")
	if err != nil {
		t.Fatal(err)
	}
	if first == otherDomain {
		t.Fatal("different identifier domains collided")
	}

	otherPart, err := DeriveIdentifier([]byte(key), "hub-v2", "work")
	if err != nil {
		t.Fatal(err)
	}
	if first == otherPart {
		t.Fatal("different identifier inputs collided")
	}

	for _, r := range first {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			t.Fatalf("identifier %q contains unsafe character %q", first, r)
		}
	}
}

func TestDeriveIdentifierRejectsUnsafeDomainInputs(t *testing.T) {
	key := strings.Repeat("k", 32)
	for name, domain := range map[string]string{
		"blank": " ",
		"nul":   "hub\x00v2",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DeriveIdentifier([]byte(key), domain, "value"); err == nil {
				t.Fatal("unsafe domain was accepted")
			}
		})
	}

	if _, err := DeriveIdentifier([]byte(key), "hub-v2", "a\x00b"); err == nil {
		t.Fatal("NUL-containing identifier part was accepted")
	}
	if _, err := DeriveIdentifier([]byte("short"), "hub-v2", "value"); err == nil {
		t.Fatal("short identifier key was accepted")
	}
	if _, err := DeriveIdentifier(nil, "hub-v2", "value"); err == nil {
		t.Fatal("empty identifier key was accepted")
	}
}
