package gitstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

const (
	Version          = 1
	MaxStateBytes    = 256 << 10
	MaxTransferBytes = 64 << 20
	MaxStatusEntries = 4096
	MaxStashes       = 128
	MaxPathBytes     = 4096
	MaxTextBytes     = 512
)

type Mode string

const (
	ModeGit         Mode = "git"
	ModeNoGit       Mode = "no-git"
	ModeUnavailable Mode = "unavailable"
)

type State struct {
	Version            int              `json:"version"`
	ProjectIdentity    string           `json:"projectIdentity,omitempty"`
	SessionRecordCount uint64           `json:"sessionRecordCount,omitempty"`
	SessionHeadDigest  string           `json:"sessionHeadDigest,omitempty"`
	Mode               Mode             `json:"mode"`
	Repository         RepositoryState  `json:"repository,omitempty"`
	Worktree           WorktreeState    `json:"worktree,omitempty"`
	Stashes            []Stash          `json:"stashes,omitempty"`
	Transfer           TransferMetadata `json:"transfer,omitempty"`
}

type RepositoryState struct {
	Head         string `json:"head,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Detached     bool   `json:"detached,omitempty"`
	Upstream     string `json:"upstream,omitempty"`
	UpstreamHead string `json:"upstreamHead,omitempty"`
	Ahead        uint64 `json:"ahead,omitempty"`
	Behind       uint64 `json:"behind,omitempty"`
}

type WorktreeState struct {
	Clean            bool          `json:"clean"`
	Entries          []StatusEntry `json:"entries,omitempty"`
	SensitiveOmitted bool          `json:"sensitiveOmitted,omitempty"`
}

type StatusEntry struct {
	XY           string `json:"xy"`
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
}

type Stash struct {
	Ref     string `json:"ref"`
	Subject string `json:"subject,omitempty"`
}

type TransferMetadata struct {
	Requested      bool   `json:"requested,omitempty"`
	CommitRange    string `json:"commitRange,omitempty"`
	CommitTip      string `json:"commitTip,omitempty"`
	CommitBytes    int64  `json:"commitBytes,omitempty"`
	CommitDigest   string `json:"commitDigest,omitempty"`
	WorktreeBase   string `json:"worktreeBase,omitempty"`
	WorktreeTip    string `json:"worktreeTip,omitempty"`
	WorktreeBytes  int64  `json:"worktreeBytes,omitempty"`
	WorktreeDigest string `json:"worktreeDigest,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type Transfer struct {
	Version         int
	ProjectIdentity string
	CommitRange     string
	CommitTip       string
	CommitBundle    []byte
	WorktreeBase    string
	WorktreeTip     string
	WorktreeBundle  []byte
}

type stateWire struct {
	Version            int              `json:"version"`
	ProjectIdentity    string           `json:"projectIdentity,omitempty"`
	SessionRecordCount uint64           `json:"sessionRecordCount,omitempty"`
	SessionHeadDigest  string           `json:"sessionHeadDigest,omitempty"`
	Mode               Mode             `json:"mode"`
	Repository         RepositoryState  `json:"repository,omitempty"`
	Worktree           WorktreeState    `json:"worktree,omitempty"`
	Stashes            []Stash          `json:"stashes,omitempty"`
	Transfer           TransferMetadata `json:"transfer,omitempty"`
}

type transferWire struct {
	Version         int    `json:"version"`
	ProjectIdentity string `json:"projectIdentity,omitempty"`
	CommitRange     string `json:"commitRange,omitempty"`
	CommitTip       string `json:"commitTip,omitempty"`
	CommitBundle    []byte `json:"commitBundle,omitempty"`
	WorktreeBase    string `json:"worktreeBase,omitempty"`
	WorktreeTip     string `json:"worktreeTip,omitempty"`
	WorktreeBundle  []byte `json:"worktreeBundle,omitempty"`
}

func (s State) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("gitstate: unsupported state version %d", s.Version)
	}
	if s.Mode != ModeGit && s.Mode != ModeNoGit && s.Mode != ModeUnavailable {
		return fmt.Errorf("gitstate: unsupported mode %q", s.Mode)
	}
	if err := validateText(s.ProjectIdentity, MaxPathBytes, "project identity"); err != nil {
		return err
	}
	if s.SessionRecordCount == 0 && s.SessionHeadDigest != "" {
		return errors.New("gitstate: empty session count has a head digest")
	}
	if s.SessionHeadDigest != "" {
		if err := validateHexOptional(s.SessionHeadDigest, "session head digest"); err != nil {
			return err
		}
	}
	if s.Mode == ModeGit {
		if err := validateHexOptional(s.Repository.Head, "head"); err != nil {
			return err
		}
		if err := validateHexOptional(s.Repository.UpstreamHead, "upstream head"); err != nil {
			return err
		}
		if err := validateRef(s.Repository.Branch, "branch"); err != nil {
			return err
		}
		if err := validateRef(s.Repository.Upstream, "upstream"); err != nil {
			return err
		}
	}
	if len(s.Worktree.Entries) > MaxStatusEntries {
		return errors.New("gitstate: too many worktree entries")
	}
	seen := make(map[string]struct{}, len(s.Worktree.Entries))
	for _, entry := range s.Worktree.Entries {
		if len(entry.XY) != 2 {
			return errors.New("gitstate: invalid worktree status")
		}
		if err := validateRelativePath(entry.Path); err != nil {
			return err
		}
		if err := validateRelativePathOptional(entry.OriginalPath); err != nil {
			return err
		}
		key := entry.XY + "\x00" + entry.Path + "\x00" + entry.OriginalPath
		if _, ok := seen[key]; ok {
			return errors.New("gitstate: duplicate worktree entry")
		}
		seen[key] = struct{}{}
	}
	if len(s.Stashes) > MaxStashes {
		return errors.New("gitstate: too many stashes")
	}
	for _, stash := range s.Stashes {
		if !strings.HasPrefix(stash.Ref, "stash@{") || !strings.HasSuffix(stash.Ref, "}") {
			return errors.New("gitstate: invalid stash reference")
		}
		if err := validateText(stash.Subject, MaxTextBytes, "stash subject"); err != nil {
			return err
		}
	}
	if err := validateTransferMetadata(s.Transfer); err != nil {
		return err
	}
	return nil
}

func (s State) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	wire := stateWire{
		Version: s.Version, ProjectIdentity: s.ProjectIdentity, SessionRecordCount: s.SessionRecordCount, SessionHeadDigest: s.SessionHeadDigest, Mode: s.Mode,
		Repository: s.Repository, Worktree: s.Worktree,
		Stashes: append([]Stash(nil), s.Stashes...), Transfer: s.Transfer,
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("gitstate: encode state: %w", err)
	}
	if len(data) > MaxStateBytes {
		return nil, errors.New("gitstate: state is too large")
	}
	return data, nil
}

func ParseState(data []byte) (State, error) {
	if len(data) == 0 || len(data) > MaxStateBytes {
		return State{}, errors.New("gitstate: state size is invalid")
	}
	var wire stateWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return State{}, fmt.Errorf("gitstate: decode state: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return State{}, errors.New("gitstate: state contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return State{}, fmt.Errorf("gitstate: trailing state data: %v", err)
	}
	state := State{
		Version: wire.Version, ProjectIdentity: wire.ProjectIdentity, SessionRecordCount: wire.SessionRecordCount, SessionHeadDigest: wire.SessionHeadDigest, Mode: wire.Mode,
		Repository: wire.Repository, Worktree: wire.Worktree,
		Stashes: wire.Stashes, Transfer: wire.Transfer,
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

func (t Transfer) Validate() error {
	if t.Version != Version {
		return fmt.Errorf("gitstate: unsupported transfer version %d", t.Version)
	}
	if err := validateText(t.ProjectIdentity, MaxPathBytes, "project identity"); err != nil {
		return err
	}
	if err := validateCommitRange(t.CommitRange); err != nil {
		return err
	}
	if err := validateHexOptional(t.CommitTip, "commit tip"); err != nil {
		return err
	}
	if err := validateHexOptional(t.WorktreeBase, "worktree base"); err != nil {
		return err
	}
	if err := validateHexOptional(t.WorktreeTip, "worktree tip"); err != nil {
		return err
	}
	if len(t.CommitBundle) > MaxTransferBytes || len(t.WorktreeBundle) > MaxTransferBytes {
		return errors.New("gitstate: transfer is too large")
	}
	if len(t.CommitBundle) == 0 && len(t.WorktreeBundle) == 0 {
		return errors.New("gitstate: transfer is empty")
	}
	return nil
}

func (t Transfer) MarshalBinary() ([]byte, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(transferWire{
		Version: t.Version, ProjectIdentity: t.ProjectIdentity,
		CommitRange: t.CommitRange, CommitTip: t.CommitTip, CommitBundle: t.CommitBundle,
		WorktreeBase: t.WorktreeBase, WorktreeTip: t.WorktreeTip, WorktreeBundle: t.WorktreeBundle,
	})
	if err != nil {
		return nil, fmt.Errorf("gitstate: encode transfer: %w", err)
	}
	if len(data) > MaxTransferBytes+MaxTransferBytes/2 {
		return nil, errors.New("gitstate: encoded transfer is too large")
	}
	return data, nil
}

func ParseTransfer(data []byte) (Transfer, error) {
	if len(data) == 0 || len(data) > MaxTransferBytes+MaxTransferBytes/2 {
		return Transfer{}, errors.New("gitstate: transfer size is invalid")
	}
	var wire transferWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Transfer{}, fmt.Errorf("gitstate: decode transfer: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Transfer{}, errors.New("gitstate: transfer contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return Transfer{}, fmt.Errorf("gitstate: trailing transfer data: %v", err)
	}
	transfer := Transfer{
		Version: wire.Version, ProjectIdentity: wire.ProjectIdentity,
		CommitRange: wire.CommitRange, CommitTip: wire.CommitTip, CommitBundle: wire.CommitBundle,
		WorktreeBase: wire.WorktreeBase, WorktreeTip: wire.WorktreeTip, WorktreeBundle: wire.WorktreeBundle,
	}
	if err := transfer.Validate(); err != nil {
		return Transfer{}, err
	}
	return transfer, nil
}

func TransferMetadataFor(transfer Transfer, requested bool, reason string) TransferMetadata {
	metadata := TransferMetadata{Requested: requested, Reason: reason}
	metadata.CommitRange = transfer.CommitRange
	metadata.CommitTip = transfer.CommitTip
	metadata.WorktreeBase = transfer.WorktreeBase
	metadata.WorktreeTip = transfer.WorktreeTip
	if len(transfer.CommitBundle) != 0 {
		metadata.CommitBytes = int64(len(transfer.CommitBundle))
		metadata.CommitDigest = digest(transfer.CommitBundle)
	}
	if len(transfer.WorktreeBundle) != 0 {
		metadata.WorktreeBytes = int64(len(transfer.WorktreeBundle))
		metadata.WorktreeDigest = digest(transfer.WorktreeBundle)
	}
	return metadata
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateTransferMetadata(metadata TransferMetadata) error {
	if metadata.CommitBytes < 0 || metadata.WorktreeBytes < 0 || metadata.CommitBytes > MaxTransferBytes || metadata.WorktreeBytes > MaxTransferBytes {
		return errors.New("gitstate: invalid transfer size")
	}
	if err := validateCommitRange(metadata.CommitRange); err != nil {
		return err
	}
	if err := validateHexOptional(metadata.CommitTip, "commit tip"); err != nil {
		return err
	}
	if err := validateHexOptional(metadata.WorktreeBase, "worktree base"); err != nil {
		return err
	}
	if err := validateHexOptional(metadata.WorktreeTip, "worktree tip"); err != nil {
		return err
	}
	for name, value := range map[string]string{"commit digest": metadata.CommitDigest, "worktree digest": metadata.WorktreeDigest} {
		if value != "" && len(value) != 64 {
			return fmt.Errorf("gitstate: invalid %s", name)
		}
	}
	return validateText(metadata.Reason, MaxTextBytes, "transfer reason")
}

func validateText(value string, limit int, name string) error {
	if len(value) > limit || strings.ContainsRune(value, 0) {
		return fmt.Errorf("gitstate: invalid %s", name)
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" || len(value) > MaxPathBytes || strings.ContainsRune(value, 0) {
		return errors.New("gitstate: invalid relative path")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") {
		return errors.New("gitstate: invalid relative path")
	}
	return nil
}

func validateRelativePathOptional(value string) error {
	if value == "" {
		return nil
	}
	return validateRelativePath(value)
}

func validateRef(value, name string) error {
	if value == "" {
		return nil
	}
	if len(value) > MaxPathBytes || strings.ContainsAny(value, "\x00\r\n") || strings.HasPrefix(value, "-") {
		return fmt.Errorf("gitstate: invalid %s", name)
	}
	return nil
}

func validateHex(value, name string) error {
	if value == "" {
		return fmt.Errorf("gitstate: %s is empty", name)
	}
	return validateHexOptional(value, name)
}

func validateHexOptional(value, name string) error {
	if value == "" {
		return nil
	}
	if len(value) < 7 || len(value) > 128 {
		return fmt.Errorf("gitstate: invalid %s", name)
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return fmt.Errorf("gitstate: invalid %s", name)
		}
	}
	return nil
}

func validateCommitRange(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 2*MaxPathBytes || strings.ContainsAny(value, "\x00\r\n") || strings.ContainsAny(value, " ;|&()") {
		return errors.New("gitstate: invalid commit range")
	}
	return nil
}

func SortEntries(entries []StatusEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		if entries[i].OriginalPath != entries[j].OriginalPath {
			return entries[i].OriginalPath < entries[j].OriginalPath
		}
		return entries[i].XY < entries[j].XY
	})
}
