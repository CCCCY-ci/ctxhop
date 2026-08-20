package syncflow

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/project"
)

const (
	sessionSummaryVersion      = 1
	sessionSummaryAgentVersion = 2
	maxFingerprintPaths        = 10000
)

var (
	// ErrInvalidSessionSummary reports a payload that is not a supported
	// encrypted session listing summary.
	ErrInvalidSessionSummary = errors.New("syncflow: invalid session summary")
)

// SessionSummary is the small, encrypted payload used by list and resume.
//
// It intentionally excludes ProjectPath and Size. The project path is local
// machine state and must never cross the encryption boundary; the optional
// fingerprint contains only relative paths and digests for the restore safety
// check.
type SessionSummary struct {
	// Agent is the stable adapter name. It was added in summary version 2.
	// Version 1 payloads decode with an empty value for compatibility.
	Agent       string
	NativeID    string
	Title       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Fingerprint *project.Fingerprint
}

// EncodeSessionSummary creates the compact JSON payload published beside a
// session's encrypted branch metadata without workspace evidence.
func EncodeSessionSummary(ref adapter.SessionRef) ([]byte, error) {
	return EncodeSessionSummaryWithFingerprint(ref, nil)
}

// EncodeSessionSummaryWithFingerprint creates the compact JSON payload used by
// push. The fingerprint is copied so a caller cannot mutate an accepted
// payload through its maps or slices after encoding begins.
func EncodeSessionSummaryWithFingerprint(ref adapter.SessionRef, fingerprint *project.Fingerprint) ([]byte, error) {
	summary := SessionSummary{
		Agent:       ref.Agent,
		NativeID:    ref.NativeID,
		Title:       ref.Title,
		CreatedAt:   ref.CreatedAt,
		UpdatedAt:   ref.UpdatedAt,
		Fingerprint: cloneFingerprint(fingerprint),
	}
	if err := summary.validate(); err != nil {
		return nil, err
	}
	version := sessionSummaryVersion
	if summary.Agent != "" {
		version = sessionSummaryAgentVersion
	}
	wire := sessionSummaryWire{
		Version:     version,
		Agent:       summary.Agent,
		NativeID:    summary.NativeID,
		Title:       summary.Title,
		CreatedAt:   formatSummaryTime(summary.CreatedAt),
		UpdatedAt:   formatSummaryTime(summary.UpdatedAt),
		Fingerprint: cloneFingerprint(summary.Fingerprint),
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
		Agent:       wire.Agent,
		NativeID:    wire.NativeID,
		Title:       wire.Title,
		CreatedAt:   created,
		UpdatedAt:   updated,
		Fingerprint: cloneFingerprint(wire.Fingerprint),
	}
	if wire.Version != sessionSummaryVersion && wire.Version != sessionSummaryAgentVersion {
		return SessionSummary{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidSessionSummary, wire.Version)
	}
	if err := summary.validate(); err != nil {
		return SessionSummary{}, err
	}
	return summary, nil
}

type sessionSummaryWire struct {
	Version     int                  `json:"version"`
	Agent       string               `json:"agent,omitempty"`
	NativeID    string               `json:"nativeId"`
	Title       string               `json:"title"`
	CreatedAt   string               `json:"createdAt"`
	UpdatedAt   string               `json:"updatedAt"`
	Fingerprint *project.Fingerprint `json:"fingerprint,omitempty"`
}

func (s SessionSummary) validate() error {
	if s.Agent != "" {
		if len(s.Agent) > 64 || !utf8.ValidString(s.Agent) {
			return fmt.Errorf("%w: agent is invalid", ErrInvalidSessionSummary)
		}
		for _, r := range s.Agent {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
				continue
			}
			return fmt.Errorf("%w: agent contains an unsafe character", ErrInvalidSessionSummary)
		}
	}
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
	if err := validateFingerprint(s.Fingerprint); err != nil {
		return fmt.Errorf("%w: fingerprint: %v", ErrInvalidSessionSummary, err)
	}
	return nil
}

func validateFingerprint(fingerprint *project.Fingerprint) error {
	if fingerprint == nil {
		return nil
	}
	if len(fingerprint.Head) > 128 || !validFingerprintText(fingerprint.Head) {
		return errors.New("head is not valid text")
	}
	if len(fingerprint.Branch) > 256 || !validFingerprintText(fingerprint.Branch) {
		return errors.New("branch is not valid text")
	}
	if len(fingerprint.Dirty) > maxFingerprintPaths || len(fingerprint.Files) > maxFingerprintPaths {
		return errors.New("too many fingerprint paths")
	}
	for _, path := range fingerprint.Dirty {
		if !validFingerprintPath(path) {
			return errors.New("dirty path is unsafe")
		}
	}
	for path, digest := range fingerprint.Files {
		if !validFingerprintPath(path) {
			return errors.New("file path is unsafe")
		}
		if !validFingerprintDigest(digest) {
			return errors.New("file digest is invalid")
		}
	}
	return nil
}

func validFingerprintText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0) && !strings.ContainsAny(value, "\r\n")
}

func validFingerprintPath(value string) bool {
	if value == "" || strings.ContainsRune(value, 0) {
		return false
	}
	normalized := strings.ReplaceAll(value, `\`, "/")
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, ":") {
		return false
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return utf8.ValidString(value)
}

func validFingerprintDigest(value string) bool {
	if value == "<absent>" || value == "<directory>" {
		return true
	}
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneFingerprint(fingerprint *project.Fingerprint) *project.Fingerprint {
	if fingerprint == nil {
		return nil
	}
	clone := &project.Fingerprint{
		Head:   fingerprint.Head,
		Branch: fingerprint.Branch,
		Dirty:  append([]string(nil), fingerprint.Dirty...),
		Files:  make(map[string]string, len(fingerprint.Files)),
	}
	for path, digest := range fingerprint.Files {
		clone.Files[path] = digest
	}
	return clone
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
