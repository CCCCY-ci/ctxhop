// Package logging provides CtxHop's local diagnostic log.
//
// Logs are deliberately local-only. They contain operation metadata and
// redacted errors, never session contents, workspace files, credentials or
// environment values.
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	RetentionDays = 7
	dateLayout    = "2006-01-02"
	filePrefix    = "ctxhop-"
	fileSuffix    = ".log"
	logDirName    = "logs"
	maxErrorBytes = 1024
)

var sensitiveValuePattern = regexp.MustCompile(`(?i)\b(access[_ -]?key(?:[_ -]?id)?|secret[_ -]?access[_ -]?key|session[_ -]?token|passphrase|password|authorization|x-amz-security-token)\b(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
var quotedLocalPathPattern = regexp.MustCompile(`(?i)(?:"(?:[a-z]:[\\/]|\\\\|/)[^"]*"|'(?:[a-z]:[\\/]|\\\\|/)[^']*')`)
var remoteURLPattern = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
var windowsPathPattern = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)[^\s"'<>]+`)
var unixPathPattern = regexp.MustCompile(`(?i)(^|[\s("'=:/])/(?:[^/\s"'<>]+/)*[^/\s"'<>]+`)

// Logger writes one-line structured records to the current local-day file.
// Logging is best effort: a filesystem problem must never change the result
// of the command being diagnosed.
type Logger struct {
	logger *slog.Logger
	writer *rotatingWriter
}

// New creates a local logger rooted at configDir. The log directory is created
// lazily on the first record, so simply constructing a logger has no side
// effects. An empty configDir disables logging.
func New(configDir string) *Logger {
	if strings.TrimSpace(configDir) == "" {
		return &Logger{}
	}

	writer := &rotatingWriter{
		dir: Directory(configDir),
		now: time.Now,
	}
	return &Logger{
		logger: slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})),
		writer: writer,
	}
}

// Directory returns the local directory that contains CtxHop logs.
func Directory(configDir string) string {
	if strings.TrimSpace(configDir) == "" {
		return ""
	}
	return filepath.Join(configDir, logDirName)
}

// CurrentPath returns the log file for the local date without creating it.
func CurrentPath(configDir string, now time.Time) string {
	dir := Directory(configDir)
	if dir == "" {
		return ""
	}
	if now.IsZero() {
		now = time.Now()
	}
	return filepath.Join(dir, filePrefix+now.Format(dateLayout)+fileSuffix)
}

// Info records an informational event.
func (l *Logger) Info(event string, args ...any) {
	l.log(slog.LevelInfo, event, args...)
}

// Warn records a warning event.
func (l *Logger) Warn(event string, args ...any) {
	l.log(slog.LevelWarn, event, args...)
}

// Error records an error event.
func (l *Logger) Error(event string, args ...any) {
	l.log(slog.LevelError, event, args...)
}

func (l *Logger) log(level slog.Level, event string, args ...any) {
	if l == nil || l.logger == nil || strings.TrimSpace(event) == "" {
		return
	}
	l.logger.Log(context.Background(), level, event, args...)
}

// Path returns the file that receives the next record.
func (l *Logger) Path() string {
	if l == nil || l.writer == nil {
		return ""
	}
	now := l.writer.now()
	if now.IsZero() {
		now = time.Now()
	}
	return filepath.Join(l.writer.dir, filePrefix+now.Format(dateLayout)+fileSuffix)
}

// SanitizeError turns an error into a bounded, single-line diagnostic value.
// It is intended for logs and must be used whenever an error is recorded.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	value = sensitiveValuePattern.ReplaceAllString(value, `${1}${2}<redacted>`)
	value = quotedLocalPathPattern.ReplaceAllString(value, "<redacted-path>")
	value = remoteURLPattern.ReplaceAllString(value, "<redacted-url>")
	value = windowsPathPattern.ReplaceAllString(value, "<redacted-path>")
	value = unixPathPattern.ReplaceAllString(value, `${1}<redacted-path>`)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > maxErrorBytes {
		value = value[:maxErrorBytes] + "..."
	}
	return value
}

type rotatingWriter struct {
	mu          sync.Mutex
	dir         string
	now         func() time.Time
	cleanupDate string
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if now.IsZero() {
		now = time.Now()
	}
	date := now.Format(dateLayout)
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return 0, fmt.Errorf("create log directory: %w", err)
	}
	if w.cleanupDate != date {
		w.cleanup(now)
		w.cleanupDate = date
	}

	path := filepath.Join(w.dir, filePrefix+date+fileSuffix)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open log file: %w", err)
	}
	written, writeErr := file.Write(p)
	closeErr := file.Close()
	if writeErr != nil {
		return written, writeErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	return written, nil
}

func (w *rotatingWriter) cleanup(now time.Time) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cutoff := today.AddDate(0, 0, -(RetentionDays - 1))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileDate, ok := parseLogDate(entry.Name(), now.Location())
		if !ok || fileDate.Before(cutoff) {
			if !ok {
				continue
			}
			_ = os.Remove(filepath.Join(w.dir, entry.Name()))
		}
	}
}

func parseLogDate(name string, location *time.Location) (time.Time, bool) {
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return time.Time{}, false
	}
	dateText := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	if len(dateText) != len(dateLayout) {
		return time.Time{}, false
	}
	date, err := time.ParseInLocation(dateLayout, dateText, location)
	if err != nil {
		return time.Time{}, false
	}
	return date, true
}
