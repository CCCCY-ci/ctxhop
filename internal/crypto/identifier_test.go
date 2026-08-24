package crypto

import (
	"strings"
	"testing"
)

func testIDKey(t *testing.T) []byte {
	t.Helper()
	dk := NewDataKey()
	key, err := dk.IdentifierKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestIdentifiersAreStableAcrossDevices(t *testing.T) {
	// Every device must derive the same identifier for the same project, or
	// two machines would push the same project to two different locations and
	// never see each other's sessions.
	key := testIDKey(t)
	const remote = "github.com/example/app"

	first, err := ProjectID(key, remote)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectID(key, remote)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same project produced %q and %q", first, second)
	}
}

func TestIdentifiersRevealNothing(t *testing.T) {
	// An object listing alone must not show what somebody works on.
	key := testIDKey(t)
	const remote = "github.com/acme/secret-payments-service"

	id, err := ProjectID(key, remote)
	if err != nil {
		t.Fatal(err)
	}

	for _, fragment := range []string{"acme", "secret", "payments", "github"} {
		if strings.Contains(id, fragment) {
			t.Errorf("the identifier %q leaks %q", id, fragment)
		}
	}
	if len(id) < 20 {
		t.Errorf("identifier %q is too short to be collision-free", id)
	}
}

func TestIdentifiersDifferPerKey(t *testing.T) {
	// Two users storing the same open-source project in the same bucket must
	// not produce the same path.
	const remote = "github.com/example/app"

	a, err := ProjectID(testIDKey(t), remote)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProjectID(testIDKey(t), remote)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two different keys produced the same identifier")
	}
}

func TestDomainsDoNotCollide(t *testing.T) {
	// Without domain separation the same input under two meanings could yield
	// the same identifier, and one could stand in for the other.
	key := testIDKey(t)
	const value = "same-input"

	project, err := ProjectID(key, value)
	if err != nil {
		t.Fatal(err)
	}
	device, err := DeviceID(key, value)
	if err != nil {
		t.Fatal(err)
	}
	if project == device {
		t.Error("a project and a device with the same name share an identifier")
	}
}

func TestSessionIDsAreScopedToTheirProject(t *testing.T) {
	// The agent's native ids are only unique within a project.
	key := testIDKey(t)
	const native = "1ec04445-8626-4962-bded-d17fe30a8128"

	inA, err := SessionID(key, "project-a", native)
	if err != nil {
		t.Fatal(err)
	}
	inB, err := SessionID(key, "project-b", native)
	if err != nil {
		t.Fatal(err)
	}
	if inA == inB {
		t.Error("the same session id in two projects produced one identifier")
	}
}

func TestIdentifierPartsCannotBeRearranged(t *testing.T) {
	// Joining parts without a separator would let ("ab", "c") and ("a", "bc")
	// hash to the same thing.
	key := testIDKey(t)

	first, err := SessionID(key, "ab", "c")
	if err != nil {
		t.Fatal(err)
	}
	second, err := SessionID(key, "a", "bc")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("differently split inputs produced the same identifier")
	}
}

func TestIdentifiersAreUsableAsKeys(t *testing.T) {
	// They become path segments, so they must survive the remote layer's key
	// rules: no separators, no case ambiguity, nothing needing an escape.
	key := testIDKey(t)
	id, err := ProjectID(key, "github.com/example/app")
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range id {
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLower && !isDigit {
			t.Errorf("identifier %q contains %q, which is not safe in a key", id, r)
		}
	}
}

func TestIdentifiersValidateTheirInputs(t *testing.T) {
	key := testIDKey(t)

	if _, err := ProjectID(key, "   "); err == nil {
		t.Error("a blank project identity was accepted")
	}
	if _, err := DeviceID(key, ""); err == nil {
		t.Error("a blank device identity was accepted")
	}
	if _, err := SessionID(key, "p", ""); err == nil {
		t.Error("a blank session identity was accepted")
	}
	if _, err := ProjectID(key[:8], "x"); err == nil {
		t.Error("a short key was accepted")
	}
}

// TestIdentifiersRefuseTheSeparatorItself keeps the domain separation honest.
// Go strings may contain NUL, so joining with one only disambiguates if nothing
// else can produce it - otherwise ("a\x00b", "c") and ("a", "b\x00c") digest
// identically and two different sessions share a remote path.
func TestIdentifiersRefuseTheSeparatorItself(t *testing.T) {
	key := testIDKey(t)

	if _, err := ProjectID(key, "github.com/example/a\x00b"); err == nil {
		t.Error("a project identity containing NUL was accepted")
	}
	if _, err := DeviceID(key, "laptop\x00"); err == nil {
		t.Error("a device identity containing NUL was accepted")
	}

	// And the collision it would have caused is gone in both directions.
	first, errFirst := SessionID(key, "a\x00b", "c")
	second, errSecond := SessionID(key, "a", "b\x00c")
	if errFirst == nil && errSecond == nil && first == second {
		t.Error("differently split inputs still collide")
	}
}
