package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
)

const sessionCommandTimeout = 30 * time.Second

const (
	sessionActionDiscover = "discover"
	sessionActionList     = "list"
	sessionActionShow     = "show"
)

type sessionOptions struct {
	action    string
	sessionID string
	json      bool
}

type sessionShowReport struct {
	Scope   string              `json:"scope"`
	Hub     sessionHubScope     `json:"hub"`
	Project sessionProjectScope `json:"project"`
	Session sessionListEntry    `json:"session"`
}

func init() {
	for i := range commands {
		if commands[i].name == "session" {
			commands[i].run = runSession
		}
	}
}

func runSession(args []string) error {
	return runSessionWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runSessionWithIO(args []string, output io.Writer) error {
	return runSessionWithStreams(args, strings.NewReader(""), output, output)
}

func runSessionWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseSessionOptions(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("session: output is required")
	}
	if input == nil {
		return errors.New("session: input is required")
	}
	if prompt == nil {
		return errors.New("session: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCommandTimeout)
	defer cancel()

	switch options.action {
	case sessionActionDiscover:
		report, err := collectSessionDiscover(ctx, c, configDir, ".")
		if err != nil {
			return err
		}
		if options.json {
			return writeSessionDiscoverJSON(output, report)
		}
		return writeSessionDiscoverText(output, report)
	case sessionActionList, sessionActionShow:
		report, err := collectSessionListWithPrompt(ctx, c, configDir, ".", input, prompt)
		if err != nil {
			return err
		}
		if options.action == sessionActionShow {
			return writeSessionShow(output, report, options.sessionID, options.json)
		}
		if options.json {
			return writeSessionListJSON(output, report)
		}
		return writeSessionListText(output, report)
	default:
		return fmt.Errorf("session: unsupported action %q", options.action)
	}
}

func parseSessionOptions(args []string) (sessionOptions, error) {
	if len(args) == 0 {
		return sessionOptions{}, errors.New("session: expected discover, list, or show")
	}

	options := sessionOptions{action: args[0]}
	flags := flag.NewFlagSet("session "+options.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	flagArgs := args[1:]
	if options.action == sessionActionShow {
		// The standard flag package stops parsing at the first positional
		// argument. Accept both `show --json <session>` and
		// `show <session> --json` by moving flags ahead of the selector.
		flagArgs = reorderSessionShowArgs(flagArgs)
	}
	if err := flags.Parse(flagArgs); err != nil {
		return sessionOptions{}, fmt.Errorf("session %s: %w", options.action, err)
	}
	switch options.action {
	case sessionActionDiscover, sessionActionList:
		if flags.NArg() != 0 {
			return sessionOptions{}, fmt.Errorf("session %s: unexpected argument %q", options.action, flags.Arg(0))
		}
	case sessionActionShow:
		if flags.NArg() != 1 {
			return sessionOptions{}, errors.New("session show: expected exactly one session ID")
		}
		options.sessionID = flags.Arg(0)
		if strings.ContainsRune(options.sessionID, 0) {
			return sessionOptions{}, errors.New("session show: session ID contains an invalid character")
		}
	default:
		return sessionOptions{}, fmt.Errorf("session: unsupported action %q; expected discover, list, or show", options.action)
	}
	return options, nil
}

func reorderSessionShowArgs(args []string) []string {
	if len(args) < 2 {
		return args
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

func collectSessionListWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, prompt io.Writer) (sessionListReport, error) {
	collection, err := collectListCollection(ctx, c, configDir, projectDir, input, prompt, "session list")
	if err != nil {
		return sessionListReport{}, err
	}
	hubScope, _, _, err := sessionHubAndProject(collection.identifierKey, collection.current)
	if err != nil {
		return sessionListReport{}, err
	}
	registry, err := loadSessionRegistryForRead(configDir, collection.identifierKey, hubScope.ID)
	if err != nil {
		return sessionListReport{}, fmt.Errorf("session list: load local Session Hub registry: %w", err)
	}
	return buildSessionList(collection, registry)
}

func collectSessionDiscover(ctx context.Context, c *config.Config, configDir, projectDir string) (sessionDiscoverReport, error) {
	if c == nil {
		return sessionDiscoverReport{}, errors.New("session discover: configuration is unavailable")
	}
	if ctx == nil {
		return sessionDiscoverReport{}, errors.New("session discover: context is required")
	}
	if err := ctx.Err(); err != nil {
		return sessionDiscoverReport{}, fmt.Errorf("session discover: %w", err)
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return sessionDiscoverReport{}, fmt.Errorf("session discover: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return sessionDiscoverReport{}, fmt.Errorf("session discover: %s", reason)
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return sessionDiscoverReport{}, fmt.Errorf("session discover: load local sync material: %w", err)
	}
	hubScope, _, _, err := sessionHubAndProject(secrets.IdentifierKey, current)
	if err != nil {
		return sessionDiscoverReport{}, err
	}
	registry, err := loadSessionRegistryForRead(configDir, secrets.IdentifierKey, hubScope.ID)
	if err != nil {
		return sessionDiscoverReport{}, fmt.Errorf("session discover: load local Session Hub registry: %w", err)
	}
	refs, err := discoverListSessionsWithContext(ctx, current.Root)
	if err != nil {
		return sessionDiscoverReport{}, fmt.Errorf("session discover: %w", err)
	}
	return buildSessionDiscoverReport(secrets.IdentifierKey, current, refs, registry)
}

func writeSessionListJSON(w io.Writer, report sessionListReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeSessionDiscoverJSON(w io.Writer, report sessionDiscoverReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeSessionShow(w io.Writer, report sessionListReport, sessionID string, jsonOutput bool) error {
	for _, entry := range report.Sessions {
		if entry.SessionID != sessionID {
			continue
		}
		show := sessionShowReport{Scope: report.Scope, Hub: report.Hub, Project: report.Project, Session: entry}
		if jsonOutput {
			encoder := json.NewEncoder(w)
			encoder.SetIndent("", "  ")
			return encoder.Encode(show)
		}
		if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "hub: %s (%s)\n", safeListText(report.Hub.Name), safeListText(report.Hub.ID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "project: %s\n", safeListText(report.Project.ID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(entry.SessionID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "title: %s\n", safeListText(entry.Title)); err != nil {
			return err
		}
		if !entry.CreatedAt.IsZero() {
			if _, err := fmt.Fprintf(w, "created: %s\n", entry.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if !entry.UpdatedAt.IsZero() {
			if _, err := fmt.Fprintf(w, "updated: %s\n", entry.UpdatedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "local: %t\n", entry.Local); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "sources:"); err != nil {
			return err
		}
		for _, source := range entry.Sources {
			if _, err := fmt.Fprintf(w, "- agent=%s", safeListText(source.Agent)); err != nil {
				return err
			}
			if source.NativeID != "" {
				if _, err := fmt.Fprintf(w, " native=%s", safeListText(source.NativeID)); err != nil {
					return err
				}
			}
			if source.DeviceID != "" {
				if _, err := fmt.Fprintf(w, " device=%s", safeListText(source.DeviceID)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, " local=%t\n", source.Local); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("session show: session %q was not found in the current project", sessionID)
}
