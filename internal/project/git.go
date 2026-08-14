package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// gitError keeps Git's diagnostic for callers that need to classify it, while
// deliberately hiding that diagnostic from Error. Git routinely includes the
// repository path, branch name or remote address in stderr, and those values
// must not reach doctor output or a user-pasted error report (BR-09).
type gitError struct {
	subcommand string
	stderr     string
	cause      error
}

func (e *gitError) Error() string {
	if e == nil || e.subcommand == "" {
		return "git command failed"
	}
	return fmt.Sprintf("git %s failed", e.subcommand)
}

func (e *gitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// runGit executes a read-only Git command and removes only its final line
// ending. In particular, it must not trim the leading status columns emitted
// by porcelain output.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	data, err := runGitRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// runGitRaw is runGit's byte-preserving form. It is used for NUL-delimited
// output and filenames that cannot safely travel through a line-oriented API.
func runGitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runGitInput(ctx, dir, nil, args...)
}

// runGitInput executes Git with an optional stdin payload. The environment is
// inherited so Git can find its helpers on Windows, while optional index locks
// and terminal prompting are disabled for a non-mutating, unattended check.
func runGitInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &gitError{
			subcommand: firstArg(args),
			stderr:     stderr.String(),
			cause:      err,
		}
	}
	return stdout.Bytes(), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return "command"
	}
	return args[0]
}

// splitNUL returns non-empty fields from a NUL-delimited Git response.
func splitNUL(data []byte) []string {
	var out []string
	for _, field := range bytes.Split(data, []byte{0}) {
		if len(field) > 0 {
			out = append(out, string(field))
		}
	}
	return out
}

// pathSafe strips a filename from a filesystem error without losing its
// underlying cause, so errors.Is remains useful to callers.
func pathSafe(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// dubiousOwnership reports Git's safe.directory refusal without exposing the
// repository named in the diagnostic.
func dubiousOwnership(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "detected dubious ownership")
}

func gitUnavailable(err error) bool {
	var ee *exec.Error
	return errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound)
}
