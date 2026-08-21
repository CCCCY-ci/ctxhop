package gitstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/atomicfile"
)

type ApplyRecord struct {
	Version         int       `json:"version"`
	AppliedAt       time.Time `json:"appliedAt"`
	ProjectID       string    `json:"projectId"`
	SessionID       string    `json:"sessionId"`
	ProjectIdentity string    `json:"projectIdentity,omitempty"`
	SourceHead      string    `json:"sourceHead,omitempty"`
	CurrentHead     string    `json:"currentHead,omitempty"`
	Branch          string    `json:"branch,omitempty"`
	CommitRef       string    `json:"commitRef,omitempty"`
	WorktreeRef     string    `json:"worktreeRef,omitempty"`
	WorktreeApplied bool      `json:"worktreeApplied,omitempty"`
	Status          string    `json:"status"`
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
	if err := validateHexOptional(r.CurrentHead, "current head"); err != nil {
		return err
	}
	if err := validateRef(r.Branch, "branch"); err != nil {
		return err
	}
	if err := validateRef(r.CommitRef, "commit ref"); err != nil {
		return err
	}
	if err := validateRef(r.WorktreeRef, "worktree ref"); err != nil {
		return err
	}
	if r.Status != ApplyNoChange && r.Status != ApplyApplied && r.Status != ApplyPartial && r.Status != ApplyConflict {
		return errors.New("gitstate: invalid apply record status")
	}
	return nil
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
