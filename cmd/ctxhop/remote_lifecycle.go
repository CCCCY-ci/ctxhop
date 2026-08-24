package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
)

// Remote deletion is an explicit administrative operation. The individual
// S3 requests still have their own 30-second timeout, while the larger
// operation gets enough time to remove a project with many immutable shards.
const remoteLifecycleTimeout = 5 * time.Minute

const (
	remoteActionDeleteSession = "delete-session"
	remoteActionDeleteProject = "delete-project"
	remoteActionDeleteAll     = "delete-all"
)

type remoteOptions struct {
	action   string
	target   string
	path     string
	remoteID bool
	yes      bool
}

func init() {
	for i := range commands {
		if commands[i].name == "remote" {
			commands[i].run = runRemote
		}
	}
}

func runRemote(args []string) error {
	return runRemoteWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runRemoteWithIO(args []string, input io.Reader, output io.Writer) error {
	return runRemoteWithStreams(args, input, output, output)
}

func runRemoteWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	if output == nil {
		return errors.New("remote: output is required")
	}
	options, err := parseRemoteOptions(args)
	if err != nil {
		return err
	}
	if !options.yes {
		if input == nil {
			return fmt.Errorf("remote %s: input is required", options.action)
		}
		if prompt == nil {
			return fmt.Errorf("remote %s: prompt output is required", options.action)
		}
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteLifecycleTimeout)
	defer cancel()

	switch options.action {
	case remoteActionDeleteSession:
		projectID, sessionID, err := resolveRemoteSession(ctx, c, configDir, options.path, options.target, options.remoteID)
		if err != nil {
			return err
		}
		if !options.yes {
			confirmed, err := confirmRemoteDeletion(input, prompt, options.action, sessionID)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("remote %s: cancelled", options.action)
			}
		}
		access, err := openAuthorizedDomain(ctx, c, configDir, "remote "+options.action)
		if err != nil {
			return err
		}
		defer access.close()
		store := access.Store
		removed, err := syncer.DeleteRemoteSession(ctx, store, projectID, sessionID)
		if err != nil {
			return remoteDeletionError(options.action, removed, err)
		}
		return writeRemoteDeletionResult(output, options.action, removed)
	case remoteActionDeleteProject:
		projectID, err := resolveRemoteProject(ctx, c, configDir, options.path)
		if err != nil {
			return err
		}
		if !options.yes {
			confirmed, err := confirmRemoteDeletion(input, prompt, options.action, projectID)
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("remote %s: cancelled", options.action)
			}
		}
		access, err := openAuthorizedDomain(ctx, c, configDir, "remote "+options.action)
		if err != nil {
			return err
		}
		defer access.close()
		store := access.Store
		removed, err := syncer.DeleteRemoteProject(ctx, store, projectID)
		if err != nil {
			return remoteDeletionError(options.action, removed, err)
		}
		return writeRemoteDeletionResult(output, options.action, removed)
	case remoteActionDeleteAll:
		if !options.yes {
			confirmed, err := confirmRemoteDeletion(input, prompt, options.action, "")
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("remote %s: cancelled", options.action)
			}
		}
		access, err := openAuthorizedDomain(ctx, c, configDir, "remote "+options.action)
		if err != nil {
			return err
		}
		defer access.close()
		store := access.Store
		removed, err := syncer.DeleteRemoteAll(ctx, store)
		if err != nil {
			return remoteDeletionError(options.action, removed, err)
		}
		return writeRemoteDeletionResult(output, options.action, removed)
	default:
		return fmt.Errorf("remote: unsupported action %q", options.action)
	}
}

func parseRemoteOptions(args []string) (remoteOptions, error) {
	if len(args) == 0 {
		return remoteOptions{}, errors.New("remote: expected delete-session, delete-project, or delete-all")
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	switch action {
	case remoteActionDeleteSession:
		flags := flag.NewFlagSet("remote delete-session", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		remoteID := flags.Bool("remote-id", false, "treat the session argument as its opaque remote ID")
		path := flags.String("path", ".", "project directory used to derive the project ID")
		if err := flags.Parse(args[1:]); err != nil {
			return remoteOptions{}, fmt.Errorf("remote delete-session: %w", err)
		}
		if flags.NArg() != 1 {
			return remoteOptions{}, errors.New("remote delete-session: expected one native session ID or remote ID")
		}
		return remoteOptions{
			action:   action,
			target:   strings.TrimSpace(flags.Arg(0)),
			path:     *path,
			remoteID: *remoteID,
			yes:      *yes,
		}, nil
	case remoteActionDeleteProject:
		flags := flag.NewFlagSet("remote delete-project", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		path := flags.String("path", ".", "project directory used to derive the project ID")
		if err := flags.Parse(args[1:]); err != nil {
			return remoteOptions{}, fmt.Errorf("remote delete-project: %w", err)
		}
		if flags.NArg() != 0 {
			return remoteOptions{}, fmt.Errorf("remote delete-project: unexpected argument %q", flags.Arg(0))
		}
		return remoteOptions{action: action, path: *path, yes: *yes}, nil
	case remoteActionDeleteAll:
		flags := flag.NewFlagSet("remote delete-all", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		yes := flags.Bool("yes", false, "skip the confirmation prompt")
		if err := flags.Parse(args[1:]); err != nil {
			return remoteOptions{}, fmt.Errorf("remote delete-all: %w", err)
		}
		if flags.NArg() != 0 {
			return remoteOptions{}, fmt.Errorf("remote delete-all: unexpected argument %q", flags.Arg(0))
		}
		return remoteOptions{action: action, yes: *yes}, nil
	default:
		return remoteOptions{}, fmt.Errorf("remote: unknown action %q; expected delete-session, delete-project, or delete-all", args[0])
	}
}

func resolveRemoteProject(ctx context.Context, c *config.Config, configDir, projectDir string) (string, error) {
	projectID, identifierKey, err := resolveRemoteProjectInputs(ctx, c, configDir, projectDir)
	if err != nil {
		return "", err
	}
	zeroRemoteIdentifierKey(identifierKey)
	return projectID, nil
}

func resolveRemoteSession(ctx context.Context, c *config.Config, configDir, projectDir, requested string, remoteID bool) (string, string, error) {
	projectID, identifierKey, err := resolveRemoteProjectInputs(ctx, c, configDir, projectDir)
	if err != nil {
		return "", "", err
	}
	defer zeroRemoteIdentifierKey(identifierKey)

	requested = strings.TrimSpace(requested)
	if remoteID {
		if _, err := syncer.NewSessionLayout(projectID, requested); err != nil {
			return "", "", fmt.Errorf("remote delete-session: invalid remote session ID: %w", err)
		}
		return projectID, requested, nil
	}
	sessionID, err := crypto.SessionID(identifierKey, projectID, requested)
	if err != nil {
		return "", "", fmt.Errorf("remote delete-session: derive session ID: %w", err)
	}
	return projectID, sessionID, nil
}

func resolveRemoteProjectInputs(ctx context.Context, c *config.Config, configDir, projectDir string) (string, []byte, error) {
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return "", nil, fmt.Errorf("remote: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return "", nil, fmt.Errorf("remote: %s", reason)
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return "", nil, fmt.Errorf("remote: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		zeroRemoteIdentifierKey(secrets.IdentifierKey)
		return "", nil, fmt.Errorf("remote: derive project identity: %w", err)
	}
	return projectID, secrets.IdentifierKey, nil
}

func zeroRemoteIdentifierKey(key []byte) {
	for i := range key {
		key[i] = 0
	}
}

func confirmRemoteDeletion(input io.Reader, prompt io.Writer, action, target string) (bool, error) {
	if input == nil {
		return false, fmt.Errorf("remote %s: input is required", action)
	}
	if prompt == nil {
		return false, fmt.Errorf("remote %s: prompt output is required", action)
	}

	switch action {
	case remoteActionDeleteSession:
		if _, err := fmt.Fprintf(prompt, "Delete remote session %q for this project? This removes all device branches and metadata. [y/N]: ", target); err != nil {
			return false, err
		}
	case remoteActionDeleteProject:
		if _, err := fmt.Fprintf(prompt, "Delete all remote sessions for project %q? Device records and the keyfile remain. [y/N]: ", target); err != nil {
			return false, err
		}
	case remoteActionDeleteAll:
		if _, err := fmt.Fprint(prompt, "Delete every object in the configured Remote, including the keyfile and device records? [y/N]: "); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("remote: unsupported confirmation action %q", action)
	}

	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("remote %s: read confirmation: %w", action, err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func remoteDeletionError(action string, removed int, err error) error {
	if removed > 0 {
		return fmt.Errorf("remote %s: removed %d objects before failure: %w", action, removed, err)
	}
	return fmt.Errorf("remote %s: delete remote data: %w", action, err)
}

func writeRemoteDeletionResult(output io.Writer, action string, removed int) error {
	_, err := fmt.Fprintf(output, "remote deleted: scope=%s objects=%d\n", action, removed)
	return err
}
