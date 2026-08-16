package diagnostic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordBoundsAndRedactsTokens(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxRecentErrors+3; i++ {
		if err := Record(dir, "resume", "command-failed"); err != nil {
			t.Fatalf("Record(%d): %v", i, err)
		}
	}
	if err := Record(dir, `C:\private\session`, `raw error with path`); err != nil {
		t.Fatalf("Record unsafe values: %v", err)
	}
	events, err := Recent(dir)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(events) != maxRecentErrors {
		t.Fatalf("events = %d, want %d", len(events), maxRecentErrors)
	}
	last := events[len(events)-1]
	if last.Command != "unknown" || last.Class != "unknown" {
		t.Fatalf("last event = %+v", last)
	}
	data, err := os.ReadFile(filepath.Join(dir, errorHistoryFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "raw error") {
		t.Fatalf("history leaked input: %s", data)
	}
}

func TestRecentTreatsMissingHistoryAsEmpty(t *testing.T) {
	events, err := Recent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if events != nil {
		t.Fatalf("events = %+v, want nil", events)
	}
}
