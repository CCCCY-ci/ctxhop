package syncflow

import (
	"errors"
	"testing"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
)

func TestSessionSummaryRoundTripExcludesProjectPath(t *testing.T) {
	created := time.Date(2026, 8, 15, 1, 2, 3, 0, time.FixedZone("test", 8*60*60))
	updated := created.Add(5 * time.Minute)
	payload, err := EncodeSessionSummary(adapter.SessionRef{
		NativeID:    "native-session-1",
		ProjectPath: `C:\Users\secret\project`,
		Title:       "continue the sync work",
		CreatedAt:   created,
		UpdatedAt:   updated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"version":1,"nativeId":"native-session-1","title":"continue the sync work","createdAt":"2026-08-14T17:02:03Z","updatedAt":"2026-08-14T17:07:03Z"}` {
		t.Fatalf("payload = %s", payload)
	}
	if string(payload) == `C:\Users\secret\project` {
		t.Fatal("project path appeared in summary payload")
	}

	parsed, err := DecodeSessionSummary(payload)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.NativeID != "native-session-1" || parsed.Title != "continue the sync work" || !parsed.CreatedAt.Equal(created.UTC()) || !parsed.UpdatedAt.Equal(updated.UTC()) {
		t.Fatalf("parsed = %+v", parsed)
	}
}

func TestDecodeSessionSummaryRejectsUnknownAndNonCompactPayload(t *testing.T) {
	for _, payload := range []string{
		`{"version":1,"nativeId":"session","title":"title","createdAt":"2026-08-14T17:02:03Z","updatedAt":"2026-08-14T17:07:03Z","path":"secret"}`,
		` {"version":1,"nativeId":"session","title":"title","createdAt":"2026-08-14T17:02:03Z","updatedAt":"2026-08-14T17:07:03Z"}`,
	} {
		if _, err := DecodeSessionSummary([]byte(payload)); !errors.Is(err, ErrInvalidSessionSummary) {
			t.Fatalf("payload %q error = %v, want ErrInvalidSessionSummary", payload, err)
		}
	}
}
