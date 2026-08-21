package gitstate

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxInspectionBytes     = 8 << 20
	maxInspectionFileBytes = 8 << 20
)

var (
	ErrSensitiveContent    = errors.New("gitstate: transfer contains sensitive content")
	ErrInspectionTooLarge  = errors.New("gitstate: transfer could not be inspected within the safety limit")
	ErrTransferUnavailable = errors.New("gitstate: requested Git transfer is unavailable")
)

type gitCommandError struct {
	command string
	stderr  string
	cause   error
}

func (e *gitCommandError) Error() string {
	if e == nil || e.command == "" {
		return "git command failed"
	}
	return "git " + e.command + " failed"
}

func (e *gitCommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	data, err := runGitRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func runGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &gitCommandError{command: firstCommandArg(args), stderr: stderr.String(), cause: err}
	}
	return stdout.Bytes(), nil
}

func firstCommandArg(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	return args[0]
}

func Capture(ctx context.Context, root, projectIdentity string) (State, error) {
	if ctx == nil {
		return State{}, errors.New("gitstate: context is required")
	}
	if strings.TrimSpace(root) == "" {
		return State{}, errors.New("gitstate: project root is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return State{}, fmt.Errorf("gitstate: locate project root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return State{}, errors.New("gitstate: project root is not a directory")
	}
	state := State{Version: Version, ProjectIdentity: projectIdentity}
	inside, err := runGit(ctx, absolute, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctx.Err() != nil {
			return State{}, ctx.Err()
		}
		if gitUnavailable(err) {
			state.Mode = ModeUnavailable
			return state, nil
		}
		state.Mode = ModeNoGit
		return state, nil
	}
	if strings.TrimSpace(inside) != "true" {
		state.Mode = ModeNoGit
		return state, nil
	}
	state.Mode = ModeGit

	if head, headErr := runGit(ctx, absolute, "rev-parse", "HEAD"); headErr == nil {
		state.Repository.Head = strings.TrimSpace(head)
	} else if ctx.Err() != nil {
		return State{}, ctx.Err()
	}
	if branch, branchErr := runGit(ctx, absolute, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil {
		state.Repository.Branch = strings.TrimSpace(branch)
	} else if ctx.Err() != nil {
		return State{}, ctx.Err()
	} else {
		state.Repository.Detached = true
	}
	if upstream, upstreamErr := runGit(ctx, absolute, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); upstreamErr == nil {
		state.Repository.Upstream = strings.TrimSpace(upstream)
		if upstreamHead, err := runGit(ctx, absolute, "rev-parse", state.Repository.Upstream); err == nil {
			state.Repository.UpstreamHead = strings.TrimSpace(upstreamHead)
		}
		if counts, err := runGit(ctx, absolute, "rev-list", "--left-right", "--count", state.Repository.Upstream+"...HEAD"); err == nil {
			behind, ahead, parseErr := parseAheadBehind(counts)
			if parseErr == nil {
				state.Repository.Behind = behind
				state.Repository.Ahead = ahead
			}
		}
	} else if ctx.Err() != nil {
		return State{}, ctx.Err()
	}
	status, err := captureStatus(ctx, absolute)
	if err != nil {
		return State{}, err
	}
	state.Worktree = status
	state.Stashes = captureStashes(ctx, absolute)
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

func CaptureTransfer(ctx context.Context, root string, state State) (State, Transfer, error) {
	return CaptureTransferWithOptions(ctx, root, state, TransferOptions{})
}

func CaptureTransferWithOptions(ctx context.Context, root string, state State, options TransferOptions) (State, Transfer, error) {
	if ctx == nil {
		return State{}, Transfer{}, errors.New("gitstate: context is required")
	}
	if err := state.Validate(); err != nil {
		return State{}, Transfer{}, err
	}
	if err := validateStashRef(options.StashRef); err != nil {
		return State{}, Transfer{}, err
	}
	state.Transfer = TransferMetadata{Requested: true}
	transfer := Transfer{Version: Version, ProjectIdentity: state.ProjectIdentity}
	if state.Mode != ModeGit {
		if options.StashRef != "" {
			return State{}, Transfer{}, errors.New("gitstate: selected stash requires a Git project")
		}
		state.Transfer.Reason = "Git is not available for this project"
		return state, transfer, nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return State{}, Transfer{}, fmt.Errorf("gitstate: locate project root: %w", err)
	}
	if options.StashRef == "" {
		if state.Worktree.SensitiveOmitted {
			return State{}, Transfer{}, ErrSensitiveContent
		}
		if err := inspectWorkingTree(ctx, absolute, state); err != nil {
			return State{}, Transfer{}, err
		}
	}
	if state.Repository.Ahead > 0 {
		if state.Repository.Upstream == "" || state.Repository.Head == "" {
			state.Transfer.Reason = "local commits have no usable upstream"
		} else {
			transfer.CommitRange = state.Repository.Upstream + "..HEAD"
			transfer.CommitTip = state.Repository.Head
			if err := inspectRevision(ctx, absolute, transfer.CommitRange); err != nil {
				return State{}, Transfer{}, err
			}
			bundle, bundleErr := createBundle(ctx, absolute, transfer.CommitRange)
			if bundleErr != nil {
				return State{}, Transfer{}, bundleErr
			}
			transfer.CommitBundle = bundle
		}
	}
	if options.StashRef != "" {
		if state.Repository.Head == "" {
			return State{}, Transfer{}, errors.New("gitstate: selected stash requires a repository base commit")
		}
		stashTip, resolveErr := resolveSelectedStash(ctx, absolute, options.StashRef)
		if resolveErr != nil {
			return State{}, Transfer{}, resolveErr
		}
		if inspectErr := inspectStash(ctx, absolute, stashTip); inspectErr != nil {
			return State{}, Transfer{}, inspectErr
		}
		bundle, bundleErr := createBundle(ctx, absolute, stashTip)
		if bundleErr != nil {
			return State{}, Transfer{}, bundleErr
		}
		transfer.WorktreeBase = state.Repository.Head
		transfer.WorktreeTip = stashTip
		transfer.WorktreeStashRef = options.StashRef
		transfer.WorktreeBundle = bundle
	} else if len(state.Worktree.Entries) != 0 {
		if state.Repository.Head == "" {
			state.Transfer.Reason = "the repository has no base commit for a worktree snapshot"
		} else {
			stashTip, bundle, transferErr := createWorktreeTransfer(ctx, absolute, state)
			if transferErr != nil {
				return State{}, Transfer{}, transferErr
			}
			if stashTip != "" {
				transfer.WorktreeBase = state.Repository.Head
				transfer.WorktreeTip = stashTip
				transfer.WorktreeBundle = bundle
			}
		}
	}
	state.Transfer = TransferMetadataFor(transfer, true, state.Transfer.Reason)
	if len(transfer.CommitBundle) == 0 && len(transfer.WorktreeBundle) == 0 && state.Transfer.Reason == "" {
		state.Transfer.Reason = "no unpushed commits or uncommitted changes were found"
	}
	return state, transfer, nil
}

func resolveSelectedStash(ctx context.Context, root, ref string) (string, error) {
	if err := validateStashRef(ref); err != nil {
		return "", err
	}
	tip, err := runGit(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("gitstate: selected stash %q was not found: %w", ref, ErrTransferUnavailable)
	}
	tip = strings.TrimSpace(tip)
	if err := validateHex(tip, "stash tip"); err != nil {
		return "", fmt.Errorf("gitstate: selected stash %q is invalid: %w", ref, ErrTransferUnavailable)
	}
	return tip, nil
}

func inspectStash(ctx context.Context, root, ref string) error {
	data, err := runGitRaw(ctx, root, "stash", "show", "--name-only", "--include-untracked", ref)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("gitstate: inspect selected stash paths: %w", ErrTransferUnavailable)
	}
	paths, pathsErr := stashShowPaths(data)
	if pathsErr != nil {
		return pathsErr
	}
	for _, path := range paths {
		if err := validateRelativePath(path); err != nil {
			return fmt.Errorf("gitstate: selected stash contains an invalid path: %w", err)
		}
		if sensitivePath(path) {
			return ErrSensitiveContent
		}
	}
	data, err = runGitRaw(ctx, root, "stash", "show", "--include-untracked", "--patch", "--binary", "--no-ext-diff", ref)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("gitstate: inspect selected stash changes: %w", ErrTransferUnavailable)
	}
	return inspectDiff(data)
}

// Git stash create does not include ordinary untracked files on supported Git
// versions. When one is present, use a temporary stash and restore it before returning.
func createWorktreeTransfer(ctx context.Context, root string, state State) (string, []byte, error) {
	if hasUntrackedWorktreeChanges(state.Worktree.Entries) {
		return createTemporaryStashTransfer(ctx, root)
	}
	stashTip, stashErr := runGit(ctx, root, "stash", "create", "AgentSync workspace transfer")
	if stashErr != nil {
		return "", nil, fmt.Errorf("gitstate: create worktree snapshot: %w", ErrTransferUnavailable)
	}
	stashTip = strings.TrimSpace(stashTip)
	if stashTip == "" {
		return "", nil, nil
	}
	bundle, bundleErr := createBundle(ctx, root, stashTip)
	if bundleErr != nil {
		return "", nil, bundleErr
	}
	return stashTip, bundle, nil
}

func hasUntrackedWorktreeChanges(entries []StatusEntry) bool {
	for _, entry := range entries {
		if entry.XY == "??" {
			return true
		}
	}
	return false
}

func createTemporaryStashTransfer(ctx context.Context, root string) (string, []byte, error) {
	previousTip := ""
	if value, err := runGit(ctx, root, "rev-parse", "--verify", "refs/stash"); err == nil {
		previousTip = strings.TrimSpace(value)
	} else if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	if _, err := runGit(ctx, root, "stash", "push", "--include-untracked", "--message", "AgentSync temporary workspace transfer"); err != nil {
		return "", nil, fmt.Errorf("gitstate: create temporary worktree snapshot: %w", ErrTransferUnavailable)
	}
	stashTip, err := runGit(ctx, root, "rev-parse", "--verify", "refs/stash")
	if err != nil {
		return "", nil, fmt.Errorf("gitstate: locate temporary worktree snapshot failed; the saved changes remain in stash: %w", ErrTransferUnavailable)
	}
	stashTip = strings.TrimSpace(stashTip)
	if stashTip == "" || stashTip == previousTip {
		return "", nil, fmt.Errorf("gitstate: temporary worktree snapshot was not created: %w", ErrTransferUnavailable)
	}
	bundle, bundleErr := createBundle(ctx, root, stashTip)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, restoreErr := runGit(cleanupCtx, root, "stash", "apply", "--index", stashTip); restoreErr != nil {
		return "", nil, fmt.Errorf("gitstate: restore temporary worktree snapshot failed; the saved changes remain in stash: %w", ErrTransferUnavailable)
	}
	currentTip, currentErr := runGit(cleanupCtx, root, "rev-parse", "--verify", "refs/stash")
	if currentErr != nil || strings.TrimSpace(currentTip) != stashTip {
		return "", nil, fmt.Errorf("gitstate: temporary stash changed before cleanup; the saved changes remain in stash: %w", ErrTransferUnavailable)
	}
	if _, dropErr := runGit(cleanupCtx, root, "stash", "drop", "stash@{0}"); dropErr != nil {
		return "", nil, fmt.Errorf("gitstate: remove temporary worktree snapshot failed; the saved changes remain in stash: %w", ErrTransferUnavailable)
	}
	if bundleErr != nil {
		return "", nil, bundleErr
	}
	return stashTip, bundle, nil
}

func captureStatus(ctx context.Context, root string) (WorktreeState, error) {
	data, err := runGitRaw(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return WorktreeState{}, fmt.Errorf("gitstate: read worktree status: %w", ErrTransferUnavailable)
	}
	fields := splitNUL(data)
	entries := make([]StatusEntry, 0, len(fields))
	state := WorktreeState{Clean: len(fields) == 0}
	for index := 0; index < len(fields); index++ {
		record := fields[index]
		if len(record) < 4 || record[2] != ' ' {
			return WorktreeState{}, errors.New("gitstate: invalid Git status record")
		}
		entry := StatusEntry{XY: record[:2], Path: record[3:]}
		if entry.Path == "" {
			return WorktreeState{}, errors.New("gitstate: Git status contains an empty path")
		}
		if strings.ContainsAny(entry.XY, "RC") {
			if index+1 >= len(fields) {
				return WorktreeState{}, errors.New("gitstate: Git rename record is incomplete")
			}
			entry.OriginalPath = fields[index+1]
			index++
		}
		if sensitivePath(entry.Path) || sensitivePath(entry.OriginalPath) {
			state.SensitiveOmitted = true
			continue
		}
		if err := validateRelativePath(entry.Path); err != nil {
			return WorktreeState{}, err
		}
		if entry.OriginalPath != "" {
			if err := validateRelativePath(entry.OriginalPath); err != nil {
				return WorktreeState{}, err
			}
		}
		entries = append(entries, entry)
	}
	if len(entries) > MaxStatusEntries {
		return WorktreeState{}, errors.New("gitstate: worktree has too many changed paths")
	}
	SortEntries(entries)
	state.Entries = entries
	state.Clean = len(entries) == 0 && !state.SensitiveOmitted
	return state, nil
}

func captureStashes(ctx context.Context, root string) []Stash {
	data, err := runGitRaw(ctx, root, "stash", "list", "--format=%gd%x00%gs")
	if err != nil {
		return nil
	}
	var stashes []Stash
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		parts := bytes.SplitN(line, []byte{0}, 2)
		if len(parts) != 2 {
			continue
		}
		ref := strings.TrimSpace(string(parts[0]))
		if ref == "" || len(stashes) >= MaxStashes {
			break
		}
		subject := sanitizeSubject(string(parts[1]))
		stashes = append(stashes, Stash{Ref: ref, Subject: subject})
	}
	return stashes
}

func inspectWorkingTree(ctx context.Context, root string, state State) error {
	for _, args := range [][]string{
		{"diff", "--binary", "--no-ext-diff", "HEAD", "--"},
		{"diff", "--binary", "--no-ext-diff", "--cached", "--"},
	} {
		data, err := runGitRaw(ctx, root, args...)
		if err != nil {
			return fmt.Errorf("gitstate: inspect worktree changes: %w", ErrTransferUnavailable)
		}
		if err := inspectDiff(data); err != nil {
			return err
		}
	}
	for _, entry := range state.Worktree.Entries {
		if entry.XY == "??" || strings.Contains(entry.XY, "A") || strings.Contains(entry.XY, "M") {
			if err := inspectFile(root, entry.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectRevision(ctx context.Context, root, revision string) error {
	paths, err := runGitRaw(ctx, root, "diff", "--name-only", "-z", revision, "--")
	if err != nil {
		return fmt.Errorf("gitstate: inspect commit paths: %w", ErrTransferUnavailable)
	}
	for _, path := range splitNUL(paths) {
		if sensitivePath(path) {
			return ErrSensitiveContent
		}
	}
	data, err := runGitRaw(ctx, root, "diff", "--binary", "--no-ext-diff", revision, "--")
	if err != nil {
		return fmt.Errorf("gitstate: inspect commit changes: %w", ErrTransferUnavailable)
	}
	return inspectDiff(data)
}

func inspectDiff(data []byte) error {
	if len(data) > maxInspectionBytes {
		return ErrInspectionTooLarge
	}
	// `git diff --binary` marks real binary content with a standalone
	// `GIT binary patch` line. Check complete diff lines so source text that
	// merely mentions the marker is not rejected as binary content.
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("GIT binary patch")) || bytes.HasPrefix(line, []byte("Binary files ")) {
			return ErrTransferUnavailable
		}
	}
	if containsSensitiveContent(data) {
		return ErrSensitiveContent
	}
	return nil
}

func inspectFile(root, relative string) error {
	if sensitivePath(relative) {
		return ErrSensitiveContent
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitstate: inspect worktree file: %w", ErrTransferUnavailable)
	}
	if info.IsDir() || info.Size() == 0 {
		return nil
	}
	if info.Size() > maxInspectionFileBytes {
		return ErrInspectionTooLarge
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("gitstate: inspect worktree file: %w", ErrTransferUnavailable)
	}
	if containsSensitiveContent(data) {
		return ErrSensitiveContent
	}
	return nil
}

func createBundle(ctx context.Context, root, revision string) ([]byte, error) {
	bundleRevision := revision
	var temporaryRef string
	if !strings.Contains(revision, "..") {
		nonce := make([]byte, 12)
		if _, err := crand.Read(nonce); err != nil {
			return nil, fmt.Errorf("gitstate: prepare bundle: %w", ErrTransferUnavailable)
		}
		temporaryRef = "refs/agentsync/transfer/" + hex.EncodeToString(nonce)
		if _, err := runGit(ctx, root, "update-ref", temporaryRef, revision); err != nil {
			return nil, fmt.Errorf("gitstate: prepare bundle: %w", ErrTransferUnavailable)
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = runGit(cleanupCtx, root, "update-ref", "-d", temporaryRef)
		}()
		bundleRevision = temporaryRef
	}
	file, err := os.CreateTemp("", "agentsync-git-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("gitstate: prepare bundle: %w", ErrTransferUnavailable)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("gitstate: prepare bundle: %w", ErrTransferUnavailable)
	}
	defer os.Remove(path)
	if _, err := runGitRaw(ctx, root, "bundle", "create", path, bundleRevision); err != nil {
		return nil, fmt.Errorf("gitstate: create bundle: %w", ErrTransferUnavailable)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 || info.Size() > MaxTransferBytes {
		return nil, ErrTransferUnavailable
	}
	data, err := readLimited(path, MaxTransferBytes)
	if err != nil {
		return nil, fmt.Errorf("gitstate: read bundle: %w", ErrTransferUnavailable)
	}
	return data, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrInspectionTooLarge
	}
	return data, nil
}

func parseAheadBehind(value string) (behind, ahead uint64, err error) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, 0, errors.New("invalid ahead/behind count")
	}
	behind, err = strconv.ParseUint(strings.TrimPrefix(fields[0], "+"), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	ahead, err = strconv.ParseUint(strings.TrimPrefix(fields[1], "+"), 10, 64)
	return behind, ahead, err
}

func splitNUL(data []byte) []string {
	var result []string
	for _, field := range bytes.Split(data, []byte{0}) {
		if len(field) != 0 {
			result = append(result, string(field))
		}
	}
	return result
}

func gitUnavailable(err error) bool {
	var commandErr *gitCommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	var execErr *exec.Error
	return errors.As(commandErr.cause, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound)
}

func sensitivePath(value string) bool {
	value = strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
	for _, part := range strings.Split(value, "/") {
		part = strings.TrimSpace(part)
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") || part == "credentials" || part == "secrets" || part == "cookies" || part == "token" || part == "tokens" {
			return true
		}
		if strings.HasPrefix(part, "id_rsa") || strings.HasPrefix(part, "id_ed25519") {
			return true
		}
		for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".kdbx"} {
			if strings.HasSuffix(part, suffix) {
				return true
			}
		}
	}
	return false
}

func containsSensitiveContent(data []byte) bool {
	text := strings.ToLower(string(data))
	for _, marker := range []string{
		"-----begin private key-----",
		"-----begin rsa private key-----",
		"-----begin openssh private key-----",
		"aws_secret_access_key",
		"secret_access_key",
		"client_secret",
		"private_key",
		"api_key",
		"access_token",
		"refresh_token",
		"session_token",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "+"))
		for _, key := range []string{"password", "passwd", "secret", "token", "authorization"} {
			if strings.HasPrefix(line, key+"=") || strings.HasPrefix(line, key+":") || strings.HasPrefix(line, "\""+key+"\"") {
				return true
			}
		}
	}
	return false
}

func sanitizeSubject(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " "))
	if containsSensitiveContent([]byte(value)) {
		return "redacted"
	}
	if len(value) > MaxTextBytes {
		return value[:MaxTextBytes]
	}
	return value
}
