package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// versionTimeout bounds the one subprocess this package ever starts. The agent
// must keep working whether or not we do, so a hung `--version` can never hold
// up anything of ours (§4 P2).
const versionTimeout = 5 * time.Second

// verifiedVersions lists the agent versions this adapter has been checked
// against.
//
// Kept as data rather than code so it can be updated without shipping a new
// binary, which is what keeps the window of unavailability short after the
// agent releases (spec §4.8).
var verifiedVersions = map[string]bool{
	"2.1.226": true,
}

// DefaultHome returns the agent's data directory for this machine.
//
// CLAUDE_CONFIG_DIR relocates it; the agent honours that variable, so anything
// that ignored it would read and write the wrong place entirely.
func DefaultHome() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return dir, nil
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
		return Installation{}, fmt.Errorf("adapter: agent data path is not a directory")
	}

	lookup := l.version
	if lookup == nil {
		lookup = agentVersion
	}

	inst := Installation{DataDir: l.Home}
	inst.Version = lookup(ctx)
	inst.Compatibility, inst.CompatibilityReason = gradeVersion(inst.Version)
	return inst, nil
}

// agentVersion asks the agent to report itself, returning "" if it cannot.
//
// This is the only place the agent's executable is ever run, and only with
// --version: we never drive the agent, only read what it leaves on disk.
func agentVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return ""
	}
	return parseVersion(string(out))
}

// parseVersion pulls a dotted version out of the agent's `--version` output,
// which carries extra words around it.
func parseVersion(out string) string {
	for _, field := range strings.Fields(out) {
		if strings.Count(field, ".") >= 2 && strings.IndexFunc(field, isDigit) == 0 {
			return field
		}
	}
	return ""
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// gradeVersion classifies a version, returning the level and a reason safe to
// print in diagnostics.
//
// An unknown version does not stop the tool. Agents ship constantly, and
// refusing everything on each release would leave users stranded far more often
// than it would protect them; what an unknown version restricts is writing,
// because writing is the operation that can destroy data (spec §4.8).
func gradeVersion(version string) (Compatibility, string) {
	switch {
	case version == "":
		return CompatUnknown, "could not determine the agent version"
	case verifiedVersions[version]:
		return CompatFull, "agent version is verified"
	default:
		return CompatLimited, "agent version has not been verified; backup continues, restoring needs confirmation"
	}
}

// GradeSession downgrades a compatibility level in light of what a session
// actually contained.
//
// A version check alone is not enough: the risk is not that the version is new,
// it is that the session holds a path-bearing field we do not rewrite, which
// would restore a session still pointing at the machine that produced it. When
// that happens the adapter stops rather than writing something broken.
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
