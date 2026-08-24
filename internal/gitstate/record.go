package gitstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/atomicfile"
)

type ApplyRecord struct {
	Version               int       `json:"version"`
	AppliedAt             time.Time `json:"appliedAt"`
	ProjectID             string    `json:"projectId"`
	SessionID             string    `json:"sessionId"`
	ProjectIdentity       string    `json:"projectIdentity,omitempty"`
	SourceHead            string    `json:"sourceHead,omitempty"`
	SourceBase            string    `json:"sourceBase,omitempty"`
	SourceBranch          string    `json:"sourceBranch,omitempty"`
	CurrentHead           string    `json:"currentHead,omitempty"`
	Branch                string    `json:"branch,omitempty"`
	CommitDigest          string    `json:"commitDigest,omitempty"`
	WorktreeDigest        string    `json:"worktreeDigest,omitempty"`
	WorktreeStashRef      string    `json:"worktreeStashRef,omitempty"`
	CommitRef             string    `json:"commitRef,omitempty"`
	WorktreeRef           string    `json:"worktreeRef,omitempty"`
	WorktreeApplyStarted  bool      `json:"worktreeApplyStarted,omitempty"`
	WorktreeApplied       bool      `json:"worktreeApplied,omitempty"`
	ManualCleanupRequired bool      `json:"manualCleanupRequired,omitempty"`
	Status                string    `json:"status"`
}

func (r ApplyRecord) Validate() error {
	if r.Version != Version {
		return fmt.Errorf("gitstate: unsupported apply record version %d", r.Version)
	}
	if r.AppliedAt.IsZero() {
		return errors.New("gitstate: apply record time is empty")
	}
	if err := validateIdentifier(r.ProjectID, "project ID"); err != nil {
		return err
	}
	if err := validateIdentifier(r.SessionID, "session ID"); err != nil {
		return err
	}
	if err := validateText(r.ProjectIdentity, MaxPathBytes, "project identity"); err != nil {
		return err
	}
	if err := validateHexOptional(r.SourceHead, "source head"); err != nil {
		return err
	}
	if err := validateHexOptional(r.SourceBase, "source base"); err != nil {
		return err
	}
	if err := validateRef(r.SourceBranch, "source branch"); err != nil {
		return err
	}
	if err := validateHexOptional(r.CurrentHead, "current head"); err != nil {
		return err
	}
	if err := validateRef(r.Branch, "branch"); err != nil {
		return err
	}
	for name, value := range map[string]string{"commit digest": r.CommitDigest, "worktree digest": r.WorktreeDigest} {
		if value == "" {
			continue
		}
		if len(value) != 64 {
			return fmt.Errorf("gitstate: invalid %s", name)
		}
		if err := validateHexOptional(value, name); err != nil {
			return err
		}
	}
	if err := validateStashRef(r.WorktreeStashRef); err != nil {
		return err
	}
	if err := validateRef(r.CommitRef, "commit ref"); err != nil {
		return err
	}
	if err := validateRef(r.WorktreeRef, "worktree ref"); err != nil {
		return err
	}
	if r.WorktreeApplied && !r.WorktreeApplyStarted {
		return errors.New("gitstate: applied worktree record must record that apply started")
	}
	if r.ManualCleanupRequired && !r.WorktreeApplyStarted {
		return errors.New("gitstate: manual cleanup record must record that apply started")
	}
	if r.Status != ApplyNoChange && r.Status != ApplyApplied && r.Status != ApplyAlreadyApplied && r.Status != ApplyPartial && r.Status != ApplyConflict {
		return errors.New("gitstate: invalid apply record status")
	}
	return nil
}

func (r ApplyRecord) MatchesTransfer(source State) bool {
	if r.CommitDigest == "" && r.WorktreeDigest == "" {
		return false
	}
	return r.ProjectIdentity == source.ProjectIdentity &&
		r.SourceHead == source.Repository.Head &&
		r.CommitDigest == source.Transfer.CommitDigest &&
		r.WorktreeDigest == source.Transfer.WorktreeDigest
}

func WriteApplyRecord(configDir string, record ApplyRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("gitstate: encode apply record: %w", err)
	}
	root := filepath.Join(configDir, "state", "git-applies", record.ProjectID, record.SessionID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("gitstate: create apply record directory: %w", err)
	}
	name := record.AppliedAt.UTC().Format("20060102T150405.000000000Z") + ".json"
	return atomicfile.WriteBytes(filepath.Join(root, name), data)
}

func FindMatchingApplyRecord(configDir, projectID, sessionID string, source State) (ApplyRecord, bool, error) {
	if err := validateIdentifier(projectID, "project ID"); err != nil {
		return ApplyRecord{}, false, err
	}
	if err := validateIdentifier(sessionID, "session ID"); err != nil {
		return ApplyRecord{}, false, err
	}
	if err := source.Validate(); err != nil {
		return ApplyRecord{}, false, err
	}
	root := filepath.Join(configDir, "state", "git-applies", projectID, sessionID)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return ApplyRecord{}, false, nil
	}
	if err != nil {
		return ApplyRecord{}, false, fmt.Errorf("gitstate: read apply records: %w", err)
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return ApplyRecord{}, false, fmt.Errorf("gitstate: read apply record: %w", readErr)
		}
		if len(data) == 0 || len(data) > MaxStateBytes {
			return ApplyRecord{}, false, errors.New("gitstate: apply record size is invalid")
		}
		record, parseErr := parseApplyRecord(data)
		if parseErr != nil {
			return ApplyRecord{}, false, fmt.Errorf("gitstate: parse apply record: %w", parseErr)
		}
		if record.MatchesTransfer(source) {
			return record, true, nil
		}
	}
	return ApplyRecord{}, false, nil
}

func parseApplyRecord(data []byte) (ApplyRecord, error) {
	var record ApplyRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return ApplyRecord{}, fmt.Errorf("decode apply record: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return ApplyRecord{}, errors.New("apply record contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return ApplyRecord{}, fmt.Errorf("trailing apply record data: %v", err)
	}
	if err := record.Validate(); err != nil {
		return ApplyRecord{}, err
	}
	return record, nil
}

func validateIdentifier(value, name string) error {
	if value == "" || len(value) > MaxPathBytes {
		return fmt.Errorf("gitstate: invalid %s", name)
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		return fmt.Errorf("gitstate: invalid %s", name)
	}
	return nil
}
