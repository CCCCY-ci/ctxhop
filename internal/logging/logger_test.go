package logging

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerRotatesByLocalDateAndRetainsSevenDays(t *testing.T) {
	root := t.TempDir()
	current := time.Date(2026, 8, 24, 20, 0, 0, 0, time.FixedZone("test", 8*60*60))
	logger := newLoggerForTest(root, func() time.Time { return current })

	if got, want := logger.Path(), filepath.Join(root, "logs", "ctxhop-2026-08-24.log"); got != want {
		t.Fatalf("initial log path = %q, want %q", got, want)
	}
	logger.Info("first_event", "result", "ok")

	oldPath := filepath.Join(root, "logs", "ctxhop-2026-08-16.log")
	keepPath := filepath.Join(root, "logs", "ctxhop-2026-08-19.log")
	if err := os.WriteFile(oldPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keepPath, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	current = current.AddDate(0, 0, 1)
	logger.Info("second_event", "result", "ok")
	logger.Info("cleanup_event")
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old log still exists, stat error = %v", err)
	}
	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("retained log is missing: %v", err)
	}

	first, err := os.ReadFile(filepath.Join(root, "logs", "ctxhop-2026-08-24.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "first_event") {
		t.Fatalf("first log does not contain the first event: %s", first)
	}
	second, err := os.ReadFile(filepath.Join(root, "logs", "ctxhop-2026-08-25.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "second_event") || !strings.Contains(string(second), "cleanup_event") {
		t.Fatalf("rotated log does not contain expected events: %s", second)
	}
}

func TestLoggerDoesNotCreateFilesBeforeFirstRecord(t *testing.T) {
	root := t.TempDir()
	logger := New(root)
	if _, err := os.Stat(filepath.Join(root, "logs")); !os.IsNotExist(err) {
		t.Fatalf("log directory was created during construction, stat error = %v", err)
	}
	logger.Info("created_on_write")
	if _, err := os.Stat(logger.Path()); err != nil {
		t.Fatalf("log file was not created on first record: %v", err)
	}
}

func TestSanitizeErrorRedactsSensitiveValuesAndBoundsOutput(t *testing.T) {
	err := &testError{message: `request failed Access key ID=abc SecretAccessKey: "def" Session-Token=ghi password=secret Delete "https://secret.example.test/a/b": path D:\\private\\project\\file quoted "C:\\private folder\\file" /home/private/project/file`}
	got := SanitizeError(err)
	for _, secret := range []string{"abc", "def", "ghi", "secret", "secret.example.test", "D:\\private\\project", "private folder", "/home/private/project"} {
		if strings.Contains(got, secret) {
			t.Errorf("sanitized error contains secret %q: %s", secret, got)
		}
	}
	for _, marker := range []string{"<redacted>", "<redacted-url>", "<redacted-path>"} {
		if !strings.Contains(got, marker) {
			t.Errorf("sanitized error does not show %s: %s", marker, got)
		}
	}
	if strings.Contains(got, "https://") || strings.Contains(got, "http://") {
		t.Fatalf("sanitized error does not show redaction: %s", got)
	}
	if len(got) > maxErrorBytes+3 {
		t.Fatalf("sanitized error length = %d, want at most %d", len(got), maxErrorBytes+3)
	}

	long := &testError{message: strings.Repeat("x", maxErrorBytes+100)}
	if got := SanitizeError(long); len(got) > maxErrorBytes+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long sanitized error = %q", got)
	}
}

func TestCurrentPathUsesConfigDirectory(t *testing.T) {
	now := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	want := filepath.Join("config", "logs", "ctxhop-2026-08-24.log")
	if got := CurrentPath("config", now); got != want {
		t.Fatalf("CurrentPath = %q, want %q", got, want)
	}
}

func newLoggerForTest(configDir string, now func() time.Time) *Logger {
	writer := &rotatingWriter{dir: Directory(configDir), now: now}
	return &Logger{
		logger: slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})),
		writer: writer,
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
