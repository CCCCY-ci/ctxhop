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
	sessionActionDiscover  = "discover"
	sessionActionList      = "list"
	sessionActionMigrate   = "migrate"
	sessionActionShow      = "show"
	sessionActionResume    = "resume"
	sessionActionAttach    = "attach"
	sessionActionReconcile = "reconcile"
)

type sessionOptions struct {
	action    string
	sessionID string
	json      bool
	preview   bool
	publishV2 bool
	rollback  bool
	agent     string
	nativeID  string
	asRoot    bool
	parent    string
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
	if len(args) == 0 && isInteractiveTerminal(os.Stdin, os.Stdout) {
		return runInteractiveSessionMenu(os.Stdin, os.Stdout, os.Stderr)
	}
	return runSessionWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runSessionWithIO(args []string, output io.Writer) error {
	return runSessionWithStreams(args, strings.NewReader(""), output, output)
}

func runSessionWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	// `session resume` is the nested spelling of the same operation exposed by
	// the historical top-level `resume` command. Dispatch before parsing the
	// metadata-only session subcommands so all resume flags remain owned by one
	// parser and both entry points have identical safety semantics.
	if len(args) != 0 && args[0] == sessionActionResume {
		return runResumeWithStreamsMode(args[1:], input, output, prompt, true)
	}
	if len(args) != 0 && args[0] == sessionActionSwitch {
		return runSessionMaterializeWithStreams(args[1:], input, output, prompt)
	}
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
	case sessionActionMigrate:
		report, err := collectSessionMigrationWithPrompt(ctx, c, configDir, ".", options, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			return writeSessionMigrationJSON(output, report)
		}
		return writeSessionMigrationText(output, report)
	case sessionActionAttach:
		report, err := collectSessionAttach(ctx, c, configDir, ".", options, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			return writeSessionAttachJSON(output, report)
		}
		return writeSessionAttachText(output, report)
	case sessionActionReconcile:
		report, err := collectSessionReconcile(ctx, c, configDir, ".", options, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			return writeSessionReconcileJSON(output, report)
		}
		return writeSessionReconcileText(output, report)
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
		return sessionOptions{}, errors.New("session: expected discover, list, migrate, show, attach, reconcile, switch, or resume")
	}

	options := sessionOptions{action: args[0]}
	flags := flag.NewFlagSet("session "+options.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	flagArgs := args[1:]
	if options.action == sessionActionMigrate {
		flags.BoolVar(&options.preview, "preview", false, "show a read-only migration plan")
		flags.BoolVar(&options.publishV2, "publish-v2", false, "publish the selected legacy branch as a Replica")
		flags.BoolVar(&options.rollback, "rollback", false, "select the legacy compatibility reader")
	}
	if options.action == sessionActionShow || options.action == sessionActionMigrate {
		// The standard flag package stops parsing at the first positional
		// argument. Accept both `show --json <session>` and
		// `show <session> --json` (and the corresponding migrate forms) by
		// moving flags ahead of the selector.
		flagArgs = reorderSessionShowArgs(flagArgs)
	}
	if options.action == sessionActionAttach || options.action == sessionActionReconcile {
		flags.StringVar(&options.agent, "agent", "", "local Agent owning the native session")
		flags.StringVar(&options.nativeID, "native", "", "local Agent-native session ID")
		if options.action == sessionActionAttach {
			flags.BoolVar(&options.asRoot, "as-root", false, "attach the native session as a new root")
			flags.StringVar(&options.parent, "parent", "", "attach after this Contribution head")
		}
		flagArgs = reorderSessionIdentityArgs(flagArgs)
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
	case sessionActionMigrate:
		if flags.NArg() > 1 {
			return sessionOptions{}, errors.New("session migrate: expected zero or one session ID")
		}
		if flags.NArg() == 1 {
			options.sessionID = flags.Arg(0)
			if strings.ContainsRune(options.sessionID, 0) {
				return sessionOptions{}, errors.New("session migrate: session ID contains an invalid character")
			}
		}
		if options.publishV2 && options.rollback {
			return sessionOptions{}, errors.New("session migrate: --publish-v2 and --rollback cannot be used together")
		}
	case sessionActionAttach:
		if flags.NArg() != 1 {
			return sessionOptions{}, errors.New("session attach: expected exactly one logical Session ID")
		}
		options.sessionID = flags.Arg(0)
		if strings.TrimSpace(options.sessionID) == "" || strings.ContainsRune(options.sessionID, 0) {
			return sessionOptions{}, errors.New("session attach: Session ID is empty or invalid")
		}
		if strings.TrimSpace(options.agent) == "" {
			return sessionOptions{}, errors.New("session attach: --agent is required")
		}
		if strings.TrimSpace(options.nativeID) == "" {
			return sessionOptions{}, errors.New("session attach: --native is required")
		}
		if options.asRoot == (strings.TrimSpace(options.parent) == "") {
			return sessionOptions{}, errors.New("session attach: choose exactly one of --as-root or --parent")
		}
	case sessionActionReconcile:
		if flags.NArg() != 0 {
			return sessionOptions{}, fmt.Errorf("session reconcile: unexpected argument %q", flags.Arg(0))
		}
		if strings.TrimSpace(options.agent) == "" {
			return sessionOptions{}, errors.New("session reconcile: --agent is required")
		}
		if strings.TrimSpace(options.nativeID) == "" {
			return sessionOptions{}, errors.New("session reconcile: --native is required")
		}
	default:
		return sessionOptions{}, fmt.Errorf("session: unsupported action %q; expected discover, list, migrate, show, attach, reconcile, switch, or resume", options.action)
	}
	options.agent = strings.ToLower(strings.TrimSpace(options.agent))
	options.nativeID = strings.TrimSpace(options.nativeID)
	options.parent = strings.TrimSpace(options.parent)
	if strings.ContainsRune(options.agent, 0) || strings.ContainsRune(options.nativeID, 0) || strings.ContainsRune(options.parent, 0) {
		return sessionOptions{}, errors.New("session: identity contains an invalid character")
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

// reorderSessionIdentityArgs accepts both documented positional-first and
// flag-first spellings for attach/reconcile while keeping values attached to
// their flags. A plain "move every dash-prefixed token" pass would turn
// `--agent codex <id>` into `--agent <id> codex`, silently binding the wrong
// native session.
func reorderSessionIdentityArgs(args []string) []string {
	valueFlags := map[string]struct{}{
		"-agent":   {},
		"--agent":  {},
		"-native":  {},
		"--native": {},
		"-parent":  {},
		"--parent": {},
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name := arg
		if equal := strings.IndexByte(name, '='); equal >= 0 {
			name = name[:equal]
		}
		if _, takesValue := valueFlags[name]; takesValue && !strings.ContainsRune(arg, '=') && index+1 < len(args) {
			index++
			flags = append(flags, args[index])
		}
	}
	return append(flags, positionals...)
}

func collectSessionListWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, input io.Reader, prompt io.Writer) (sessionListReport, error) {
	collection, err := collectListCollection(ctx, c, configDir, projectDir, input, prompt, "session list")
	if err != nil {
		return sessionListReport{}, err
	}
	hubScope, _, _, err := sessionHubAndProjectForConfig(collection.identifierKey, collection.current, c)
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
	hubScope, _, _, err := sessionHubAndProjectForConfig(secrets.IdentifierKey, current, c)
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
	return buildSessionDiscoverReport(secrets.IdentifierKey, current, configuredProjectHub(c, current.Identity.Value), refs, registry)
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
				if _, err := fmt.Fprintf(w, " native=%s", shortNativeSessionID(source.NativeID)); err != nil {
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
