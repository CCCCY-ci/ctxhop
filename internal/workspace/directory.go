package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errDirectoryScanLimit = errors.New("workspace: directory scan limit reached")

// DirectoryScan is the safe, bounded view of a project directory used by
// no-Git workspace synchronization. Paths are relative and never contain file
// contents.
type DirectoryScan struct {
	Paths    []string
	Complete bool
	Warnings []string
}

// CaptureDirectory creates an explicit, filtered snapshot of a no-Git project.
// It scans the project root only; generated dependency directories and unsafe
// paths are excluded before any file body is read.
func CaptureDirectory(ctx context.Context, root string) (Snapshot, error) {
	scan, err := ScanDirectory(ctx, root)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		Version:  SnapshotVersion,
		Mode:     ModeDirectory,
		Coverage: CoverageDirectory,
		Complete: scan.Complete,
		Warnings: append([]string(nil), scan.Warnings...),
	}
	totalContentBytes := 0
	totalLimitWarning := false
	for _, relative := range scan.Paths {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		entry, captureErr := captureDirectoryFile(ctx, root, relative)
		if captureErr != nil {
			return Snapshot{}, captureErr
		}
		if entry.Available && totalContentBytes+len(entry.Content) > MaxTotalContentBytes {
			entry.Available = false
			entry.Content = nil
			entry.ReasonCode = FileReasonBodyLimit
			entry.Reason = "the total workspace file-body limit was reached"
			snapshot.Complete = false
			if !totalLimitWarning {
				snapshot.Warnings = append(snapshot.Warnings, "some eligible file bodies exceeded the total workspace size limit")
				totalLimitWarning = true
			}
		}
		if entry.Available {
			totalContentBytes += len(entry.Content)
		} else {
			snapshot.Complete = false
		}
		snapshot.Files = append(snapshot.Files, entry)
	}
	if len(snapshot.Omitted) != 0 {
		snapshot.Warnings = append(snapshot.Warnings, "some eligible files were omitted by safety, size or read checks; they require manual handling")
	}
	if err := normalizeSnapshot(&snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// ScanDirectory returns the paths that a no-Git directory snapshot considers.
// It intentionally does not read file bodies, so it is also safe for target
// preview and deletion-candidate inspection.
func ScanDirectory(ctx context.Context, root string) (DirectoryScan, error) {
	if ctx == nil {
		return DirectoryScan{}, errors.New("workspace: context is required")
	}
	if err := ctx.Err(); err != nil {
		return DirectoryScan{}, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return DirectoryScan{}, fmt.Errorf("workspace: resolve project root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return DirectoryScan{}, fmt.Errorf("workspace: inspect project root: %w", err)
	}
	if !info.IsDir() {
		return DirectoryScan{}, errors.New("workspace: project root is not a directory")
	}
	scan := DirectoryScan{Complete: true}
	warningSet := make(map[string]bool)
	addWarning := func(value string) {
		if !warningSet[value] {
			scan.Warnings = append(scan.Warnings, value)
			warningSet[value] = true
		}
	}
	walkErr := filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			scan.Complete = false
			addWarning("some project paths could not be inspected")
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == absoluteRoot {
			return nil
		}
		relative, err := filepath.Rel(absoluteRoot, path)
		if err != nil {
			scan.Complete = false
			addWarning("some project paths could not be localized")
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if excludedDirectory(relative) {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			scan.Complete = false
			addWarning("symbolic links and non-regular entries were excluded")
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			scan.Complete = false
			addWarning("some project paths could not be inspected")
			return nil
		}
		if !info.Mode().IsRegular() {
			scan.Complete = false
			addWarning("symbolic links and non-regular entries were excluded")
			return nil
		}
		if unsafeFilePath(relative) {
			scan.Complete = false
			addWarning("sensitive and reserved file paths were excluded")
			return nil
		}
		if len(scan.Paths) >= MaxFiles {
			scan.Complete = false
			addWarning("the project file count exceeded the workspace snapshot limit")
			return errDirectoryScanLimit
		}
		scan.Paths = append(scan.Paths, relative)
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errDirectoryScanLimit) {
		return DirectoryScan{}, walkErr
	}
	sort.Strings(scan.Paths)
	sort.Strings(scan.Warnings)
	return scan, nil
}

func captureDirectoryFile(ctx context.Context, root, relative string) (File, error) {
	entry := File{Path: relative, Kind: KindFile}
	if err := ctx.Err(); err != nil {
		return File{}, err
	}
	path, err := targetPath(relative, root)
	if err != nil {
		entry.ReasonCode = FileReasonUnsafeEntry
		entry.Reason = "the source path is outside the project or is reserved"
		return entry, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		entry.ReasonCode = FileReasonAbsent
		entry.Reason = "the source file is no longer present"
		return entry, nil
	}
	if err != nil {
		entry.ReasonCode = FileReasonReadFailed
		entry.Reason = "the source file could not be inspected"
		return entry, nil
	}
	entry.Size = info.Size()
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		if info.Mode()&os.ModeSymlink != 0 {
			entry.ReasonCode = FileReasonSymlink
			entry.Reason = "symbolic links are not synchronized"
		} else {
			entry.ReasonCode = FileReasonNonRegular
			entry.Reason = "the source entry is not a regular file"
		}
		return entry, nil
	}
	if info.Size() > MaxFileBytes {
		entry.ReasonCode = FileReasonTooLarge
		entry.Reason = "the source file exceeds the size limit"
		return entry, nil
	}
	content, err := os.ReadFile(path)
	if err != nil || len(content) > MaxFileBytes {
		entry.ReasonCode = FileReasonReadFailed
		entry.Reason = "the source file could not be read safely"
		return entry, nil
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		entry.ReasonCode = FileReasonChanged
		entry.Reason = "the source file changed while the snapshot was captured"
		return entry, nil
	}
	digest := blobDigest(content)
	if !safeTextContent(content) || containsSensitiveMaterial(content) {
		entry.Digest = digest
		if !safeTextContent(content) {
			entry.ReasonCode = FileReasonBinary
			entry.Reason = "binary file bodies are not synchronized in this phase"
		} else {
			entry.ReasonCode = FileReasonSensitive
			entry.Reason = "the file body looks like sensitive material"
		}
		return entry, nil
	}
	entry.Digest = digest
	entry.Size = int64(len(content))
	entry.Available = true
	entry.Content = append([]byte(nil), content...)
	return entry, nil
}

func excludedDirectory(relative string) bool {
	switch strings.ToLower(filepath.Base(filepath.FromSlash(relative))) {
	case ".ctxhop", ".git", ".claude", ".codex", ".cache",
		"node_modules", "vendor", "target", "dist", "build", "coverage",
		"__pycache__", ".venv", "venv", ".next":
		return true
	default:
		return false
	}
}
