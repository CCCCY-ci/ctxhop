package syncflow

import (
	"testing"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
)

func TestSessionSummaryCarriesAgentForCodexAndReadsLegacyPayloads(t *testing.T) {
	created := time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC)
	payload, err := EncodeSessionSummary(adapter.SessionRef{
		Agent:     "codex",
		NativeID:  "native-codex",
		Title:     "codex session",
		CreatedAt: created,
		UpdatedAt: created.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := DecodeSessionSummary(payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Agent != "codex" || parsed.NativeID != "native-codex" {
		t.Fatalf("parsed = %+v", parsed)
	}

	legacy, err := EncodeSessionSummary(adapter.SessionRef{
		NativeID:  "legacy",
		Title:     "legacy session",
		CreatedAt: created,
		UpdatedAt: created.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyParsed, err := DecodeSessionSummary(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacyParsed.Agent != "" {
		t.Fatalf("legacy agent = %q, want empty", legacyParsed.Agent)
	}
}
