package sessionhub

import (
	"errors"
	"strings"
	"testing"
)

func TestV2KeysAreStableAndScoped(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))

	hub, err := DeriveHubKey(key, DefaultHubLogicalID)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := DeriveHubKey(key, DefaultHubLogicalID); err != nil || again != hub {
		t.Fatalf("hub key is not stable: %q, %v", again, err)
	}
	assertOpaqueKey(t, hub)

	project, err := DeriveProjectKey(key, hub, "github.com/example/project")
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := DeriveProjectKey(key, hub, "github.com/example/other")
	if err != nil {
		t.Fatal(err)
	}
	if project == otherProject {
		t.Fatal("different project identities produced one key")
	}

	session, err := DeriveSessionKey(key, project, "logical-session-a")
	if err != nil {
		t.Fatal(err)
	}
	otherProjectSession, err := DeriveSessionKey(key, otherProject, "logical-session-a")
	if err != nil {
		t.Fatal(err)
	}
	if session == otherProjectSession {
		t.Fatal("the same logical session crossed project scopes")
	}

	claudeNative, err := DeriveNativeSessionKey(key, "claude-code", "native-session-1")
	if err != nil {
		t.Fatal(err)
	}
	codexNative, err := DeriveNativeSessionKey(key, "codex", "native-session-1")
	if err != nil {
		t.Fatal(err)
	}
	if claudeNative == codexNative {
		t.Fatal("the same native ID crossed Agent scopes")
	}

	replicaOne, err := DeriveReplicaKey(key, session, "claude-code", claudeNative, testOpaque('d'), 1)
	if err != nil {
		t.Fatal(err)
	}
	replicaTwo, err := DeriveReplicaKey(key, session, "claude-code", claudeNative, testOpaque('d'), 2)
	if err != nil {
		t.Fatal(err)
	}
	if replicaOne == replicaTwo {
		t.Fatal("Replica generations share one key")
	}

	contributionOne, err := DeriveContributionKey(key, session, DigestBytes([]byte("one")))
	if err != nil {
		t.Fatal(err)
	}
	contributionTwo, err := DeriveContributionKey(key, session, DigestBytes([]byte("two")))
	if err != nil {
		t.Fatal(err)
	}
	if contributionOne == contributionTwo {
		t.Fatal("different contribution identities share one key")
	}

	environment, err := DeriveEnvironmentKey(key, hub, testDigest('0'))
	if err != nil {
		t.Fatal(err)
	}
	assertOpaqueKey(t, environment)
}

func TestV2KeyDerivationRejectsUnsafeInputs(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	validHub, err := DeriveHubKey(key, "work")
	if err != nil {
		t.Fatal(err)
	}
	validProject, err := DeriveProjectKey(key, validHub, "manual:project")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		call func() (string, error)
	}{
		{"blank hub identity", func() (string, error) { return DeriveHubKey(key, " ") }},
		{"nul project identity", func() (string, error) { return DeriveProjectKey(key, validHub, "project\x00name") }},
		{"invalid project key", func() (string, error) { return DeriveProjectKey(key, "not-a-key", "project") }},
		{"invalid session key", func() (string, error) { return DeriveSessionKey(key, validProject, "") }},
		{"invalid Agent", func() (string, error) { return DeriveNativeSessionKey(key, "Claude Code", "native") }},
		{"invalid native ID", func() (string, error) { return DeriveNativeSessionKey(key, "codex", "native/session") }},
		{"zero generation", func() (string, error) {
			return DeriveReplicaKey(key, validProject, "codex", testOpaque('a'), testOpaque('b'), 0)
		}},
		{"invalid fingerprint", func() (string, error) {
			return DeriveEnvironmentKey(key, validHub, "not-a-digest")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.call(); !errors.Is(err, ErrInvalidIdentity) {
				t.Fatalf("error = %v, want ErrInvalidIdentity", err)
			}
		})
	}
}

func assertOpaqueKey(t *testing.T, value string) {
	t.Helper()
	if err := validateOpaqueID(value); err != nil {
		t.Fatalf("derived key %q is not opaque-key safe: %v", value, err)
	}
}
