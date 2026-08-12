package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultTimeout bounds every git invocation. `git status` on a very large
// working tree is the known cost here (PoC-2 §7.4), and a repository that stalls
// must not stall the whole program (code_style §4.3).
const defaultTimeout = 30 * time.Second

// ErrGitUnavailable reports that git could not be run at all.
//
// Callers degrade to "no stable identity" rather than failing: a machine
// without git is a machine where this feature does not apply, not a broken one
// (spec §5).
var ErrGitUnavailable = errors.New("project: git is not available on PATH")

// gitOverrides are layered on top of the inherited environment, not used in
// place of it. Replacing the environment outright would take away PATH and,
// on Windows, SystemRoot - git needs both to find its own helper executables.
//
// Every entry closes off a way for git to block forever waiting for input that
// will never come, because stdin is not a terminal here. A repository whose
// remote needs credentials would otherwise hang the process (spec §5.1).
var gitOverrides = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"SSH_ASKPASS=",
	"GIT_OPTIONAL_LOCKS=0",
	// Keep messages parseable regardless of the user's locale.
	"LC_ALL=C",
}

// runGit executes a read-only git command in dir and returns its stdout.
//
// Only the commands this package needs are ever passed in, and none of them
// touch the network. That is a hard constraint, not a convention: `git fetch`
// or `git ls-remote` would be an outbound request to somewhere other than the
// user's configured storage, which the product forbids without exception
// (P7, spec §5.1).
//
// The output is returned with only its trailing newline removed. Trimming all
// whitespace would eat the leading status column of `git status --porcelain`,
// shifting every path by one character so that no file ever matches - which
// surfaces as a modified file being silently reported as unchanged (spec §4.1).
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGitRaw(ctx, dir, args...)
	return strings.TrimRight(string(out), "\r\n"), err
}

// runGitRaw is runGit without any trimming, for commands read with -z.
func runGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	// --no-optional-locks before the subcommand: `git status` otherwise
	// refreshes and rewrites .git/index, which is a write to the user's
	// repository during what is meant to be a read (measured; spec §5.1).
	full := append([]string{"--no-optional-locks"}, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	// os/exec keeps the last occurrence of a duplicated key, so appending
	// overrides the inherited values.
	cmd.Env = append(os.Environ(), gitOverrides...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, ErrGitUnavailable
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("git %s timed out after %s: %w", args[0], defaultTimeout, ctxErr)
		}
		// stderr can name branches and paths, so it is summarised rather than
		// quoted: this text reaches the user and must stay safe to share
		// (code_style §5.2, BR-09).
		return nil, fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// gitAvailable reports whether git can be run at all.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// splitNUL splits the NUL-separated output of a git command run with -z.
//
// -z is used for anything listing paths because git otherwise quotes and
// octal-escapes any path that is not plain ASCII: a file named 中文名.txt comes
// back as "\344\270\255\346\226\207\345\220\215.txt", which matches no real
// filename and would silently classify every non-English file as unchanged
// (measured on git 2.55; spec §4.1).
func splitNUL(out []byte) []string {
	var fields []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			fields = append(fields, string(f))
		}
	}
	return fields
}
