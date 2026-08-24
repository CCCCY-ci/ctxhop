// Package workspace describes the small, encrypted workspace snapshot that
// can travel beside a session. It is deliberately not a project backup: only
// files already selected by the session fingerprint are considered, and the
// safety filter may omit a file body.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/CCCCY-ci/ctxhop/internal/project"
)

const (
	SnapshotVersion        = 1
	MaxFiles               = 512
	MaxFileBytes           = 512 << 10
	MaxTotalContentBytes   = 4 << 20
	MaxSnapshotBytes       = 8 << 20
	maxWarningBytes        = 256
	maxHeadDigestHexLength = 64
)

const (
	ModeGit       = "git"
	ModeDirectory = "directory"
	KindFile      = "file"
	KindAbsent    = "absent"
	KindDirectory = "directory"
)

const (
	CoverageFingerprint = "fingerprint"
	CoverageDirectory   = "directory"
)

var ErrInvalidSnapshot = errors.New("workspace: invalid snapshot")

// Snapshot contains Git state and bounded file bodies for one session tip.
// Paths are project-relative; no local project root is ever serialized.
type Snapshot struct {
	Version     int      `json:"version"`
	Mode        string   `json:"mode"`
	RecordCount uint64   `json:"recordCount"`
	HeadDigest  string   `json:"headDigest,omitempty"`
	Head        string   `json:"head,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Coverage    string   `json:"coverage,omitempty"`
	Dirty       []string `json:"dirty,omitempty"`
	Files       []File   `json:"files,omitempty"`
	Omitted     []string `json:"omitted,omitempty"`
	Complete    bool     `json:"complete"`
	Warnings    []string `json:"warnings,omitempty"`
}

// File is one project-relative entry. Content is present only when the body
// passed the safety filter and the source digest still matched at capture
// time.
type File struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Content   []byte `json:"content,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Capture creates a bounded snapshot from the already captured fingerprint.
// Files that cannot safely travel are represented without bodies so the core
// session push can still succeed and the target can report the omission.
func Capture(ctx context.Context, root string, fingerprint project.Fingerprint) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("workspace: context is required")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("workspace: resolve project root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return Snapshot{}, fmt.Errorf("workspace: inspect project root: %w", err)
	}
	if !info.IsDir() {
		return Snapshot{}, errors.New("workspace: project root is not a directory")
	}

	snapshot := Snapshot{
		Version:  SnapshotVersion,
		Mode:     ModeDirectory,
		Coverage: CoverageFingerprint,
		Head:     fingerprint.Head,
		Branch:   fingerprint.Branch,
		Dirty:    append([]string(nil), fingerprint.Dirty...),
		Complete: true,
	}
	gitBacked := fingerprint.Head != "" || fingerprint.Branch != ""
	if gitBacked {
		snapshot.Mode = ModeGit
	}
	paths := make([]string, 0, len(fingerprint.Files))
	for path := range fingerprint.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > MaxFiles {
		paths = paths[:MaxFiles]
		snapshot.Complete = false
		snapshot.Warnings = append(snapshot.Warnings, "workspace file count exceeded the snapshot limit; some paths were omitted")
	}
	totalContentBytes := 0
	totalLimitWarning := false
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		expected := fingerprint.Files[path]
		entry := File{Path: path, Digest: expected}
		if !validRelativePath(path) || !validDigest(expected) {
			entry.Kind = KindFile
			entry.Reason = "the source fingerprint entry is unsafe"
			snapshot.Complete = false
			snapshot.Files = append(snapshot.Files, entry)
			continue
		}
		if expected == "<absent>" {
			entry.Kind = KindAbsent
			snapshot.Files = append(snapshot.Files, entry)
			continue
		}
		if expected == "<directory>" {
			entry.Kind = KindDirectory
			snapshot.Files = append(snapshot.Files, entry)
			continue
		}
		entry.Kind = KindFile
		captureFile(ctx, absoluteRoot, expected, gitBacked, &entry)
		if entry.Available && totalContentBytes+len(entry.Content) > MaxTotalContentBytes {
			entry.Available = false
			entry.Content = nil
			entry.Reason = "the total workspace file-body limit was reached"
			if !totalLimitWarning {
				snapshot.Warnings = append(snapshot.Warnings, "workspace file-body size exceeded the snapshot limit; some bodies were omitted")
				totalLimitWarning = true
			}
		}
		if entry.Available {
			totalContentBytes += len(entry.Content)
		}
		if !entry.Available {
			snapshot.Complete = false
		}
		snapshot.Files = append(snapshot.Files, entry)
	}
	if err := normalizeSnapshot(&snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func captureFile(ctx context.Context, root, expected string, gitBacked bool, entry *File) {
	if entry == nil {
		return
	}
	path, err := targetPath(entry.Path, root)
	if err != nil {
		entry.Reason = "the source path is outside the project or is reserved"
		return
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		entry.Reason = "the source file is no longer present"
		return
	}
	if err != nil {
		entry.Reason = "the source file could not be inspected"
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		entry.Reason = "symbolic links are not synchronized"
		return
	}
	if !info.Mode().IsRegular() {
		entry.Reason = "the source entry is not a regular file"
		return
	}
	if info.Size() > MaxFileBytes {
		entry.Reason = "the source file exceeds the size limit"
		return
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) > MaxFileBytes {
		entry.Reason = "the source file could not be read safely"
		return
	}
	fingerprintDigest, digestErr := digestForFingerprint(ctx, root, entry.Path, content, gitBacked)
	if digestErr != nil || fingerprintDigest != expected {
		entry.Reason = "the source file changed while the snapshot was captured"
		return
	}
	if !safeTextContent(content) {
		entry.Reason = "binary file bodies are not synchronized in this phase"
		return
	}
	if containsSensitiveMaterial(content) {
		entry.Reason = "the file body looks like sensitive material"
		return
	}
	entry.Available = true
	entry.Digest = blobDigest(content)
	entry.Content = append([]byte(nil), content...)
}

func digestForFingerprint(ctx context.Context, root, relative string, content []byte, gitBacked bool) (string, error) {
	raw := blobDigest(content)
	if !gitBacked {
		return raw, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "hash-object", "--path="+relative, "--stdin")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = bytes.NewReader(content)
	output, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		// Synthetic fingerprints may be used with a directory that is not a
		// Git worktree. In a real Git project, the expected digest check below
		// still fails closed when Git filters change the content.
		return raw, nil
	}
	digest := strings.TrimSpace(string(output))
	if !validDigest(digest) || digest == "<absent>" || digest == "<directory>" {
		return "", errors.New("workspace: Git returned an invalid file digest")
	}
	return digest, nil
}
func (s Snapshot) Validate() error {
	if s.Version != SnapshotVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidSnapshot, s.Version)
	}
	if s.Mode != ModeGit && s.Mode != ModeDirectory {
		return fmt.Errorf("%w: unsupported mode", ErrInvalidSnapshot)
	}
	if s.Coverage != "" && s.Coverage != CoverageFingerprint && s.Coverage != CoverageDirectory {
		return fmt.Errorf("%w: unsupported coverage", ErrInvalidSnapshot)
	}
	if s.RecordCount == 0 {
		if s.HeadDigest != "" {
			return fmt.Errorf("%w: empty record count has a head digest", ErrInvalidSnapshot)
		}
	} else if !validHex(s.HeadDigest, maxHeadDigestHexLength) {
		return fmt.Errorf("%w: head digest is invalid", ErrInvalidSnapshot)
	}
	if !validText(s.Head, 128) || !validText(s.Branch, 256) {
		return fmt.Errorf("%w: Git state is invalid", ErrInvalidSnapshot)
	}
	if len(s.Dirty) > MaxFiles || len(s.Files) > MaxFiles || len(s.Omitted) > MaxFiles {
		return fmt.Errorf("%w: too many workspace entries", ErrInvalidSnapshot)
	}
	if !sortedUniquePaths(s.Dirty) {
		return fmt.Errorf("%w: dirty paths are not sorted and unique", ErrInvalidSnapshot)
	}
	if !sortedUniquePaths(s.Omitted) {
		return fmt.Errorf("%w: omitted paths are not sorted and unique", ErrInvalidSnapshot)
	}
	if !sort.SliceIsSorted(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path }) {
		return fmt.Errorf("%w: files are not sorted", ErrInvalidSnapshot)
	}
	var total int
	for index, file := range s.Files {
		if index > 0 && s.Files[index-1].Path == file.Path {
			return fmt.Errorf("%w: duplicate file path", ErrInvalidSnapshot)
		}
		if err := file.validate(); err != nil {
			return fmt.Errorf("%w: file %q: %v", ErrInvalidSnapshot, file.Path, err)
		}
		if file.Available {
			total += len(file.Content)
			if total > MaxTotalContentBytes {
				return fmt.Errorf("%w: total file content is too large", ErrInvalidSnapshot)
			}
		}
	}
	for _, warning := range s.Warnings {
		if !validText(warning, maxWarningBytes) {
			return fmt.Errorf("%w: warning is invalid", ErrInvalidSnapshot)
		}
	}
	return nil
}

func (f File) validate() error {
	if !validRelativePath(f.Path) || !validDigest(f.Digest) {
		return errors.New("path or digest is invalid")
	}
	switch f.Kind {
	case KindAbsent, KindDirectory:
		if f.Kind == KindAbsent && f.Digest != "<absent>" || f.Kind == KindDirectory && f.Digest != "<directory>" || f.Available || len(f.Content) != 0 {
			return errors.New("special entry is invalid")
		}
	case KindFile:
		if f.Digest == "<absent>" || f.Digest == "<directory>" {
			return errors.New("file digest is invalid")
		}
		if f.Available {
			if len(f.Content) > MaxFileBytes || !safeTextContent(f.Content) || unsafeFilePath(f.Path) || containsSensitiveMaterial(f.Content) {
				return errors.New("file body is not safe")
			}
			if blobDigest(f.Content) != f.Digest {
				return errors.New("file body does not match its digest")
			}
		} else if len(f.Content) != 0 || !validText(f.Reason, maxWarningBytes) || f.Reason == "" {
			return errors.New("unavailable file must have a reason and no body")
		}
	default:
		return errors.New("file kind is invalid")
	}
	return nil
}

func (s Snapshot) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrInvalidSnapshot, err)
	}
	if len(payload) > MaxSnapshotBytes {
		return nil, fmt.Errorf("%w: payload is too large", ErrInvalidSnapshot)
	}
	return payload, nil
}

func ParseSnapshot(payload []byte) (Snapshot, error) {
	if len(payload) == 0 || len(payload) > MaxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("%w: payload size is invalid", ErrInvalidSnapshot)
	}
	var snapshot Snapshot
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("%w: decode: %v", ErrInvalidSnapshot, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Snapshot{}, fmt.Errorf("%w: trailing JSON", ErrInvalidSnapshot)
	} else if !errors.Is(err, io.EOF) {
		return Snapshot{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidSnapshot, err)
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func normalizeSnapshot(snapshot *Snapshot) error {
	if snapshot == nil {
		return ErrInvalidSnapshot
	}
	sort.Strings(snapshot.Dirty)
	snapshot.Dirty = unique(snapshot.Dirty)
	sort.Strings(snapshot.Omitted)
	snapshot.Omitted = unique(snapshot.Omitted)
	sort.Slice(snapshot.Files, func(i, j int) bool { return snapshot.Files[i].Path < snapshot.Files[j].Path })
	return snapshot.Validate()
}

func sortedUniquePaths(paths []string) bool {
	for i, path := range paths {
		if !validRelativePath(path) || i > 0 && paths[i-1] >= path {
			return false
		}
	}
	return true
}

func unique(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value == "" || len(out) != 0 && out[len(out)-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}

func validRelativePath(value string) bool {
	if value == "" || strings.ContainsRune(value, 0) {
		return false
	}
	normalized := filepath.ToSlash(value)
	if strings.HasPrefix(normalized, "/") || strings.Contains(normalized, ":") {
		return false
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return utf8.ValidString(value)
}

func validDigest(value string) bool {
	if value == "<absent>" || value == "<directory>" {
		return true
	}
	return validHex(value, 40)
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func blobDigest(content []byte) string {
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "blob %d\x00", len(content))
	_, _ = hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

func unsafeFilePath(path string) bool {
	normalized := strings.ToLower(filepath.ToSlash(path))
	for _, part := range strings.Split(normalized, "/") {
		if part == ".git" || part == ".env" || strings.HasPrefix(part, ".env.") {
			return true
		}
	}
	base := filepath.Base(normalized)
	if base == "credentials" || base == "secrets" || base == "cookies" || base == "cookie" {
		return true
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".kdbx"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return strings.HasPrefix(base, "id_rsa") || strings.HasPrefix(base, "id_ed25519")
}

func safeTextContent(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, r := range string(content) {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

func containsSensitiveMaterial(content []byte) bool {
	upper := strings.ToUpper(string(content))
	for _, marker := range []string{
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
	} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(strings.TrimLeft(rawLine, "`*_>- \t"))
		if strings.HasPrefix(strings.ToLower(line), "export ") {
			line = strings.TrimSpace(line[len("export "):])
		}
		separator := strings.IndexAny(line, ":=")
		if separator <= 0 || separator == len(line)-1 {
			continue
		}
		key := strings.ToLower(strings.Trim(strings.TrimSpace(line[:separator]), "`'\"{}[] "))
		if sensitiveKey(key) && strings.TrimSpace(line[separator+1:]) != "" {
			return true
		}
	}
	lower := strings.ToLower(string(content))
	for _, key := range []string{"token", "secret", "password", "api_key", "api-key", "access_key", "private_key", "authorization", "cookie", "access_token", "privatekey", "accesstoken", "apikey", "accesskey", "clientsecret", "refreshtoken", "sessiontoken"} {
		needle := "\"" + key + "\""
		for start := 0; ; {
			index := strings.Index(lower[start:], needle)
			if index < 0 {
				break
			}
			index += start + len(needle)
			colon := strings.IndexByte(lower[index:], ':')
			if colon < 0 {
				break
			}
			value := strings.TrimSpace(lower[index+colon+1:])
			if value != "" && value != "null" && value != "\"\"" && value != "{}" && value != "[]" {
				return true
			}
			start = index
		}
	}
	return false
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	compactKey := strings.NewReplacer("_", "", "-", "").Replace(key)
	for _, marker := range []string{"token", "secret", "password", "api_key", "api-key", "access_key", "private_key", "authorization", "cookie"} {
		compactMarker := strings.NewReplacer("_", "", "-", "").Replace(marker)
		if key == marker || compactKey == compactMarker || strings.HasSuffix(key, "_"+marker) || strings.HasSuffix(key, "-"+marker) || strings.HasSuffix(compactKey, compactMarker) {
			return true
		}
	}
	return false
}
