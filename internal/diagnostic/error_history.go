package diagnostic

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

const (
	errorHistoryFile    = "error-history.json"
	errorHistoryVersion = 1
	maxRecentErrors     = 20
	maxHistoryBytes     = 64 << 10
	maxTokenLength      = 64
)

// ErrorEvent is a redacted record of a failed CLI command.
//
// It deliberately stores only a UTC timestamp, a command name and a stable
// error class. It never stores the underlying error text, arguments, paths or
// session content.
type ErrorEvent struct {
	Time    time.Time `json:"time"`
	Command string    `json:"command"`
	Class   string    `json:"class"`
}

type errorHistory struct {
	Version int          `json:"version"`
	Events  []ErrorEvent `json:"events,omitempty"`
}

// Record appends one redacted failure to the bounded local history.
func Record(dir, command, class string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("diagnostic: history directory is required")
	}
	history, err := load(dir)
	if err != nil {
		return err
	}
	event := ErrorEvent{
		Time:    time.Now().UTC(),
		Command: normalizeToken(command),
		Class:   normalizeToken(class),
	}
	history.Events = append(history.Events, event)
	if len(history.Events) > maxRecentErrors {
		history.Events = append([]ErrorEvent(nil), history.Events[len(history.Events)-maxRecentErrors:]...)
	}
	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return fmt.Errorf("diagnostic: encode error history: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("diagnostic: create history directory: %w", err)
	}
	return atomicfile.WriteBytes(filepath.Join(dir, errorHistoryFile), append(data, '\n'))
}

// Recent returns the bounded, redacted local failure history in chronological
// order. A missing history file is the same as an empty history.
func Recent(dir string) ([]ErrorEvent, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("diagnostic: history directory is required")
	}
	history, err := load(dir)
	if err != nil {
		return nil, err
	}
	return append([]ErrorEvent(nil), history.Events...), nil
}

func load(dir string) (errorHistory, error) {
	path := filepath.Join(dir, errorHistoryFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return errorHistory{Version: errorHistoryVersion}, nil
	}
	if err != nil {
		return errorHistory{}, fmt.Errorf("diagnostic: read error history: %w", err)
	}
	if len(data) > maxHistoryBytes {
		return errorHistory{}, errors.New("diagnostic: error history is too large")
	}
	var history errorHistory
	if err := json.Unmarshal(data, &history); err != nil {
		return errorHistory{}, fmt.Errorf("diagnostic: error history is invalid: %w", err)
	}
	if history.Version != errorHistoryVersion {
		return errorHistory{}, fmt.Errorf("diagnostic: unsupported error history version %d", history.Version)
	}
	if len(history.Events) > maxRecentErrors {
		history.Events = history.Events[len(history.Events)-maxRecentErrors:]
	}
	for i := range history.Events {
		history.Events[i].Command = normalizeToken(history.Events[i].Command)
		history.Events[i].Class = normalizeToken(history.Events[i].Class)
		if history.Events[i].Time.IsZero() {
			history.Events[i].Time = time.Unix(0, 0).UTC()
		} else {
			history.Events[i].Time = history.Events[i].Time.UTC()
		}
	}
	return history, nil
}

func normalizeToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxTokenLength {
		return "unknown"
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			continue
		}
		return "unknown"
	}
	return value
}
