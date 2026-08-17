package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// verifiedVersions is a small baseline of Agent versions checked end to end.
// It is not an upload allowlist: unknown versions still use structural
// canonicalization and remain eligible for backup. The list only controls
// whether restore can proceed without explicit limited-compatibility consent.
var verifiedVersions = map[string]bool{
	"2.1.226": true,
	// Verified end to end by PoC-1b: read, canonicalise, localise, install and
	// resume natively.
	"2.1.227": true,
}

// DefaultHome returns the agent's data directory for this machine.
//
// CLAUDE_CONFIG_DIR relocates it; the agent honours that variable, so anything
// that ignored it would read and write the wrong place entirely. A relative
// value is resolved here, because the agent resolves it against its own working
// directory and ours is not the same - leaving it relative would point us at a
// different directory than the one actually in use.
func DefaultHome() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve CLAUDE_CONFIG_DIR: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// Detect locates Claude Code on this machine and grades our compatibility with
// it. It returns ErrNotInstalled when the agent is absent, which is an expected
// outcome and not a failure (§9.2).
//
// The agent's own executable is deliberately never run - not even for
// `--version`. Starting it would hand our "no network traffic we did not ask
// for" guarantee to somebody else's startup path, which does things like check
// for updates (§4 P7). The version is read from what the agent wrote instead,
// which is also the more useful figure: what matters for grading is the version
// that produced the records we are about to parse.
func (l Layout) Detect(ctx context.Context) (Installation, error) {
	if l.Home == "" {
		return Installation{}, errors.New("adapter: no agent home configured")
	}

	info, err := os.Stat(l.Home)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Installation{}, ErrNotInstalled
	case err != nil:
		return Installation{}, fmt.Errorf("inspect agent directory: %w", err)
	case !info.IsDir():
		return Installation{}, errors.New("adapter: agent data path is not a directory")
	}

	lookup := l.version
	if lookup == nil {
		lookup = l.versionFromNewestSession
	}

	inst := Installation{DataDir: l.Home}
	inst.Version = lookup(ctx)
	inst.Compatibility, inst.CompatibilityReason = gradeVersion(inst.Version)
	return inst, nil
}

// versionFromNewestSession reads the agent version recorded in the most
// recently modified session, or "" if there is nothing to read.
//
// Only one file is opened however many sessions exist: finding the newest is a
// directory walk, and the answer is the same whichever recent session is used.
func (l Layout) versionFromNewestSession(ctx context.Context) string {
	newest, err := l.newestSessionFile(ctx)
	if err != nil || newest == "" {
		return ""
	}

	f, err := os.Open(newest)
	if err != nil {
		return ""
	}
	// Read-only handle: a failure to close cannot lose data (code_style §2.1).
	defer f.Close() //nolint:errcheck

	data, err := ReadRecordsLenient(f)
	if err != nil {
		return ""
	}
	return summarize(data.Records).version
}

// newestSessionFile walks the project directories for the most recently
// modified session file.
func (l Layout) newestSessionFile(ctx context.Context) (string, error) {
	projects, err := os.ReadDir(l.ProjectsDir())
	if err != nil {
		// No projects directory simply means nothing has been recorded yet.
		return "", nil //nolint:nilerr
	}

	var newest string
	var newestTime int64

	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !project.IsDir() {
			continue
		}

		dir := filepath.Join(l.ProjectsDir(), project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if t := info.ModTime().UnixNano(); t > newestTime {
				newestTime, newest = t, filepath.Join(dir, entry.Name())
			}
		}
	}
	return newest, nil
}

// gradeVersion classifies a version, returning the level and a reason safe to
// print in diagnostics.
//
// An unknown version does not stop the tool. Agents ship constantly, and
// refusing everything on each release would leave users stranded far more often
// than it would protect them; what an unknown version restricts is restoring
// without consent, because writing is the operation that can destroy data.
//
// A version we could not determine at all grades no less strictly than one we
// merely do not recognise. Grading the case with the least information as the
// most permissive would invert the whole point: a caller asking "may I restore
// without confirmation?" would be told yes precisely when we know least.
func gradeVersion(version string) (Compatibility, string) {
	switch {
	case version == "":
		return CompatLimited, "agent version could not be determined; backup continues, restoring needs confirmation"
	case verifiedVersions[version]:
		return CompatFull, "agent version is verified"
	default:
		return CompatLimited, "agent version has not been verified; backup continues, restoring needs confirmation"
	}
}

// GradeSession downgrades a compatibility level in light of what a session
// actually contained.
//
// Version checks are intentionally coarse. Leaf values that are exact local
// paths are rewritten structurally, so a new Claude Code field does not stop
// backup. Findings are reserved for path-bearing object keys whose semantics
// are ambiguous; those remain stopped rather than silently changed.
//
// findings are the field names reported by a Canonicalizer, which are already
// redacted for diagnostics (BR-09).
func GradeSession(level Compatibility, findings []string) (Compatibility, string) {
	if len(findings) == 0 {
		return level, ""
	}
	return CompatStopped, fmt.Sprintf(
		"session contains %d path-bearing field(s) this adapter does not know how to rewrite: %s",
		len(findings), strings.Join(findings, ", "))
}
