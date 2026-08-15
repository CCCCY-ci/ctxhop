package syncflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
)

const sessionSummaryVersion = 1

var (
	// ErrInvalidSessionSummary reports a payload that is not a supported
	// encrypted session listing summary.
	ErrInvalidSessionSummary = errors.New("syncflow: invalid session summary")
)

// SessionSummary is the small, encrypted payload used by list and resume.
//
// It intentionally excludes ProjectPath and Size. The project path is local
// machine state and must never cross the encryption boundary; the remote
// metadata envelope already carries the durable record count and digest.
type SessionSummary struct {
	NativeID  string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EncodeSessionSummary creates the compact JSON payload published beside a
// session's encrypted branch metadata.
func EncodeSessionSummary(ref adapter.SessionRef) ([]byte, error) {
	summary := SessionSummary{
		NativeID:  ref.NativeID,
		Title:     ref.Title,
		CreatedAt: ref.CreatedAt,
		UpdatedAt: ref.UpdatedAt,
	}
	if err := summary.validate(); err != nil {
		return nil, err
	}
	wire := sessionSummaryWire{
		Version:   sessionSummaryVersion,
		NativeID:  summary.NativeID,
		Title:     summary.Title,
		CreatedAt: formatSummaryTime(summary.CreatedAt),
		UpdatedAt: formatSummaryTime(summary.UpdatedAt),
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: encode payload: %v", ErrInvalidSessionSummary, err)
	}
	return payload, nil
}

// DecodeSessionSummary parses and validates an encrypted session listing
// payload. It never accepts unknown fields or trailing JSON values.
func DecodeSessionSummary(payload []byte) (SessionSummary, error) {
	if len(payload) == 0 {
		return SessionSummary{}, fmt.Errorf("%w: payload is empty", ErrInvalidSessionSummary)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return SessionSummary{}, fmt.Errorf("%w: payload is not valid JSON: %v", ErrInvalidSessionSummary, err)
	}
	if !bytes.Equal(compact.Bytes(), payload) {
		return SessionSummary{}, fmt.Errorf("%w: payload is not compact", ErrInvalidSessionSummary)
	}

	var wire sessionSummaryWire
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return SessionSummary{}, fmt.Errorf("%w: decode payload: %v", ErrInvalidSessionSummary, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return SessionSummary{}, fmt.Errorf("%w: payload contains trailing JSON", ErrInvalidSessionSummary)
	} else if !errors.Is(err, io.EOF) {
		return SessionSummary{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidSessionSummary, err)
	}

	created, err := parseSummaryTime(wire.CreatedAt)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("%w: created time: %v", ErrInvalidSessionSummary, err)
	}
	updated, err := parseSummaryTime(wire.UpdatedAt)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("%w: updated time: %v", ErrInvalidSessionSummary, err)
	}
	summary := SessionSummary{
		NativeID:  wire.NativeID,
		Title:     wire.Title,
		CreatedAt: created,
		UpdatedAt: updated,
	}
	if wire.Version != sessionSummaryVersion {
		return SessionSummary{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidSessionSummary, wire.Version)
	}
	if err := summary.validate(); err != nil {
		return SessionSummary{}, err
	}
	return summary, nil
}

type sessionSummaryWire struct {
	Version   int    `json:"version"`
	NativeID  string `json:"nativeId"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

func (s SessionSummary) validate() error {
	if strings.TrimSpace(s.NativeID) == "" {
		return fmt.Errorf("%w: native session ID is empty", ErrInvalidSessionSummary)
	}
	if !utf8.ValidString(s.NativeID) || strings.ContainsRune(s.NativeID, 0) {
		return fmt.Errorf("%w: native session ID is not valid text", ErrInvalidSessionSummary)
	}
	for _, r := range s.NativeID {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return fmt.Errorf("%w: native session ID contains an unsafe character", ErrInvalidSessionSummary)
		}
	}
	if !utf8.ValidString(s.Title) {
		return fmt.Errorf("%w: title is not valid UTF-8", ErrInvalidSessionSummary)
	}
	if len([]rune(s.Title)) > 256 {
		return fmt.Errorf("%w: title is too long", ErrInvalidSessionSummary)
	}
	if s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: timestamps are required", ErrInvalidSessionSummary)
	}
	return nil
}

func formatSummaryTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseSummaryTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, errors.New("time is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
