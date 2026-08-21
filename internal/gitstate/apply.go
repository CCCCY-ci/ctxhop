package gitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	ApplyReady    = "ready"
	ApplyConflict = "conflict"
	ApplyApplied  = "applied"
	ApplyPartial  = "partial"
	ApplyNoChange = "no-changes"
)

type ApplyPreview struct {
	CurrentHead       string
	CurrentBranch     string
	CurrentClean      bool
	CommitAvailable   bool
	CommitReady       bool
	WorktreeAvailable bool
	WorktreeReady     bool
	Status            string
	Notes             []string
}

type ApplyResult struct {
	Status          string
	CurrentHead     string
	CurrentBranch   string
	CommitRef       string
	WorktreeRef     string
	WorktreeApplied bool
	Notes           []string
}

// PreviewTransfer checks the target repository without importing objects or
// changing the worktree. It intentionally treats a different HEAD as a
// conflict for worktree application; the caller must integrate commits by
// normal Git operations before applying a dependent worktree snapshot.
func PreviewTransfer(ctx context.Context, root string, source State, transfer *Transfer) (ApplyPreview, error) {
	if ctx == nil {
		return ApplyPreview{}, errors.New("gitstate: context is required")
	}
	if err := source.Validate(); err != nil {
		return ApplyPreview{}, err
	}
	if source.Mode != ModeGit {
		return ApplyPreview{}, ErrTransferUnavailable
	}
	current, err := Capture(ctx, root, source.ProjectIdentity)
	if err != nil {
		return ApplyPreview{}, err
	}
	if current.Mode != ModeGit {
		return ApplyPreview{}, errors.New("gitstate: target is not a Git worktree")
	}
	preview := ApplyPreview{
		CurrentHead:   current.Repository.Head,
		CurrentBranch: current.Repository.Branch,
		CurrentClean:  current.Worktree.Clean,
		Status:        ApplyReady,
	}
	if transfer != nil {
		if err := transfer.Validate(); err != nil {
			return ApplyPreview{}, err
		}
		preview.CommitAvailable = len(transfer.CommitBundle) != 0
		preview.WorktreeAvailable = len(transfer.WorktreeBundle) != 0
	}
	if preview.CommitAvailable {
		preview.CommitReady = true
		preview.Notes = append(preview.Notes, "the commit bundle will be imported into a hidden refs/agentsync reference")
	}
	if preview.WorktreeAvailable {
		preview.WorktreeReady = current.Worktree.Clean && transfer.WorktreeBase != "" && transfer.WorktreeBase == current.Repository.Head
		switch {
		case !current.Worktree.Clean:
			preview.Status = ApplyConflict
			preview.Notes = append(preview.Notes, "the target worktree is not clean; no worktree files will be changed")
		case transfer.WorktreeBase == "" || transfer.WorktreeBase != current.Repository.Head:
			preview.Status = ApplyConflict
			preview.Notes = append(preview.Notes, "the target HEAD differs from the worktree snapshot base; integrate the source commits first")
		default:
			preview.Notes = append(preview.Notes, "the worktree snapshot can be applied on the current HEAD")
		}
	}
	if !preview.CommitAvailable && !preview.WorktreeAvailable {
		preview.Status = ApplyNoChange
		preview.Notes = append(preview.Notes, "the source contains no explicit Git transfer body")
	}
	return preview, nil
}

// ApplyTransfer imports explicit Git bundles and, when the target is exactly
// on the recorded worktree base, applies the worktree stash. It never changes
// branches and never creates commits, merges, rebases, pushes, or deletes.
func ApplyTransfer(ctx context.Context, root string, source State, transfer Transfer) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, errors.New("gitstate: context is required")
	}
	if err := transfer.Validate(); err != nil {
		return ApplyResult{}, err
	}
	preview, err := PreviewTransfer(ctx, root, source, &transfer)
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{
		Status:        ApplyNoChange,
		CurrentHead:   preview.CurrentHead,
		CurrentBranch: preview.CurrentBranch,
		Notes:         append([]string(nil), preview.Notes...),
	}
	if preview.Status == ApplyConflict {
		return ApplyResult{Status: ApplyConflict, CurrentHead: preview.CurrentHead, CurrentBranch: preview.CurrentBranch, Notes: preview.Notes}, errors.New("gitstate: target is not safe for worktree application")
	}
	if len(transfer.CommitBundle) != 0 {
		ref, importErr := importBundle(ctx, root, transfer.CommitBundle, transfer.CommitTip, "commit")
		if importErr != nil {
			return ApplyResult{Status: ApplyConflict, CurrentHead: preview.CurrentHead, CurrentBranch: preview.CurrentBranch, Notes: preview.Notes}, importErr
		}
		result.CommitRef = ref
		result.Status = ApplyApplied
		result.Notes = append(result.Notes, "the source commits were imported into a hidden AgentSync ref; the current branch was not changed")
	}
	if len(transfer.WorktreeBundle) != 0 {
		ref, importErr := importBundle(ctx, root, transfer.WorktreeBundle, transfer.WorktreeTip, "worktree")
		if importErr != nil {
			result.Status = ApplyPartial
			return result, importErr
		}
		result.WorktreeRef = ref
		if _, applyErr := runGit(ctx, root, "stash", "apply", "--index", ref); applyErr != nil {
			result.Status = ApplyPartial
			return result, errors.New("gitstate: apply worktree snapshot failed")
		}
		result.WorktreeApplied = true
		result.Status = ApplyApplied
		result.Notes = append(result.Notes, "the worktree snapshot was applied; no commit was created")
	}
	if result.Status == ApplyNoChange {
		result.Notes = append(result.Notes, "no Git object was changed")
	}
	return result, nil
}

func importBundle(ctx context.Context, root string, bundle []byte, tip, kind string) (string, error) {
	if len(bundle) == 0 || tip == "" {
		return "", ErrTransferUnavailable
	}
	if err := validateHexOptional(tip, kind+" tip"); err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "agentsync-import-*.bundle")
	if err != nil {
		return "", errors.New("gitstate: prepare import bundle failed")
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", errors.New("gitstate: protect import bundle failed")
	}
	if _, err := file.Write(bundle); err != nil {
		_ = file.Close()
		return "", errors.New("gitstate: write import bundle failed")
	}
	if err := file.Close(); err != nil {
		return "", errors.New("gitstate: close import bundle failed")
	}
	digest := sha256.Sum256(bundle)
	name := hex.EncodeToString(digest[:8])
	prefix := "import"
	if kind == "worktree" {
		prefix = "worktree"
	}
	ref := "refs/agentsync/" + prefix + "/" + name
	if _, err := runGit(ctx, root, "fetch", "--no-tags", path, "+"+tip+":"+ref); err != nil {
		return "", errors.New("gitstate: import bundle failed")
	}
	return ref, nil
}

func (r ApplyResult) Validate() error {
	if r.Status != ApplyNoChange && r.Status != ApplyApplied && r.Status != ApplyPartial && r.Status != ApplyConflict {
		return fmt.Errorf("gitstate: invalid apply result status %q", r.Status)
	}
	for name, value := range map[string]string{"current head": r.CurrentHead} {
		if value != "" {
			if err := validateHexOptional(value, name); err != nil {
				return err
			}
		}
	}
	if strings.ContainsAny(r.CurrentBranch, "\x00\r\n") {
		return errors.New("gitstate: invalid current branch")
	}
	return nil
}

func (p ApplyPreview) Validate() error {
	if p.Status != ApplyReady && p.Status != ApplyConflict && p.Status != ApplyNoChange {
		return fmt.Errorf("gitstate: invalid apply preview status %q", p.Status)
	}
	return nil
}
