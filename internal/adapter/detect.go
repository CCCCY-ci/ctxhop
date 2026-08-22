package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const structuralCompatibilityReason = "compatibility is determined from session path fields; agent version is informational"

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

// Detect locates Claude Code on this machine and reports a structural
// compatibility baseline. Individual sessions are classified after their fields
// have been canonicalized. It returns ErrNotInstalled when the agent is absent,
// which is an expected outcome and not a failure (§9.2).
//
// The agent's own executable is deliberately never run - not even for
// `--version`. Starting it would hand our "no network traffic we did not ask
// for" guarantee to somebody else's startup path, which does things like check
// for updates (§4 P7). The version is read from what the agent wrote instead,
// and retained for diagnostics. It does not decide compatibility; the fields
// in the records we are about to parse do.
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
	inst.VersionSource = observedVersionSource(inst.Version)
	inst.Compatibility, inst.CompatibilityReason = compatibilityBaseline()
	return inst, nil
}

func observedVersionSource(version string) string {
	if version == "" {
		return "unavailable"
	}
	return "session-record"
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

// compatibilityBaseline is deliberately independent of the Agent version.
// The version is retained for diagnostics, while the session's actual fields
// decide whether the adapter can safely canonicalize and restore it.
func compatibilityBaseline() (Compatibility, string) {
	return CompatFull, structuralCompatibilityReason
}

// GradeSession classifies compatibility from the fields actually present in a
// session. A new Agent release remains fully compatible when the structural
// adapter can rewrite all path-bearing fields it encounters.
//
// findings are the field names reported by a Canonicalizer, which are already
// redacted for diagnostics (BR-09).
func GradeSession(level Compatibility, findings []string) (Compatibility, string) {
	if len(findings) != 0 {
		return CompatStopped, fmt.Sprintf(
			"session contains %d path-bearing field(s) this adapter does not know how to rewrite: %s",
			len(findings), strings.Join(findings, ", "))
	}
	if level == CompatStopped {
		return CompatStopped, "adapter compatibility policy stopped this session"
	}
	return CompatFull, structuralCompatibilityReason
}
