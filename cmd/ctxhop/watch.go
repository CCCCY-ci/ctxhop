package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

const (
	watchDefaultInterval = 30 * time.Second
	watchMinInterval     = time.Second
	watchMaxInterval     = 24 * time.Hour
)

type watchOptions struct {
	interval time.Duration
	once     bool
	json     bool
}

type watchEvent struct {
	Scope    string `json:"scope"`
	Event    string `json:"event"`
	Interval string `json:"interval,omitempty"`
	Pushed   int    `json:"pushed,omitempty"`
	Failed   int    `json:"failed,omitempty"`
	Skipped  int    `json:"skipped,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (event watchEvent) MarshalJSON() ([]byte, error) {
	type jsonEvent struct {
		Scope    string `json:"scope"`
		Event    string `json:"event"`
		Interval string `json:"interval,omitempty"`
		Pushed   *int   `json:"pushed,omitempty"`
		Failed   *int   `json:"failed,omitempty"`
		Skipped  *int   `json:"skipped,omitempty"`
		Error    string `json:"error,omitempty"`
	}

	payload := jsonEvent{
		Scope:    event.Scope,
		Event:    event.Event,
		Interval: event.Interval,
		Error:    event.Error,
	}
	if event.Event == "push" {
		payload.Pushed = &event.Pushed
		payload.Failed = &event.Failed
		payload.Skipped = &event.Skipped
	}
	return json.Marshal(payload)
}

type watchSnapshot struct {
	signature string
}

type watchCycleResult struct {
	changed bool
	summary pushSummary
}

func init() {
	for i := range commands {
		if commands[i].name == "watch" {
			commands[i].run = runWatch
		}
	}
}

func runWatch(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runWatchWithContext(ctx, args, os.Stdout)
}

func runWatchWithIO(args []string, output io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return runWatchWithContext(ctx, args, output)
}

func runWatchWithContext(ctx context.Context, args []string, output io.Writer) error {
	if ctx == nil {
		return errors.New("watch: context is required")
	}
	if output == nil {
		return errors.New("watch: output is required")
	}
	options, err := parseWatchOptions(args)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	if err := writeWatchEvent(output, options.json, watchEvent{
		Scope:    "project",
		Event:    "started",
		Interval: options.interval.String(),
	}); err != nil {
		return err
	}

	var previous string
	initialized := false
	for {
		result, err := runWatchCycle(ctx, c, configDir, ".", &previous, &initialized)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if writeErr := writeWatchEvent(output, options.json, watchEvent{
				Scope: "project",
				Event: "error",
				Error: safeWatchError(err),
			}); writeErr != nil {
				return writeErr
			}
			if options.once {
				return errors.New("watch: " + safeWatchError(err))
			}
		} else if result.changed {
			if writeErr := writeWatchEvent(output, options.json, watchEvent{
				Scope:   "project",
				Event:   "push",
				Pushed:  result.summary.Pushed,
				Failed:  result.summary.Failed,
				Skipped: result.summary.Skipped,
			}); writeErr != nil {
				return writeErr
			}
			if options.once && result.summary.Failed != 0 {
				return errors.New("watch: push cycle reported failed sessions")
			}
		}
		if options.once {
			return nil
		}

		timer := time.NewTimer(options.interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func parseWatchOptions(args []string) (watchOptions, error) {
	flags := flag.NewFlagSet("watch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	options := watchOptions{interval: watchDefaultInterval}
	flags.DurationVar(&options.interval, "interval", options.interval, "polling interval")
	flags.BoolVar(&options.once, "once", false, "run one scan and exit")
	flags.BoolVar(&options.json, "json", false, "write newline-delimited JSON events")
	if err := flags.Parse(args); err != nil {
		return watchOptions{}, fmt.Errorf("watch: %w", err)
	}
	if flags.NArg() != 0 {
		return watchOptions{}, fmt.Errorf("watch: unexpected argument %q", flags.Arg(0))
	}
	if options.interval < watchMinInterval {
		return watchOptions{}, fmt.Errorf("watch: interval must be at least %s", watchMinInterval)
	}
	if options.interval > watchMaxInterval {
		return watchOptions{}, fmt.Errorf("watch: interval must be at most %s", watchMaxInterval)
	}
	return options, nil
}

func runWatchCycle(ctx context.Context, c *config.Config, configDir, projectDir string, previous *string, initialized *bool) (watchCycleResult, error) {
	if ctx == nil {
		return watchCycleResult{}, errors.New("watch: context is required")
	}
	if previous == nil || initialized == nil {
		return watchCycleResult{}, errors.New("watch: cycle state is required")
	}
	cycleCtx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	snapshot, err := discoverWatchSnapshot(cycleCtx, c, projectDir)
	if err != nil {
		return watchCycleResult{}, err
	}
	if *initialized && snapshot.signature == *previous {
		return watchCycleResult{}, nil
	}

	summary, err := collectPush(cycleCtx, c, configDir, projectDir, pushOptions{})
	if err != nil {
		return watchCycleResult{changed: true}, err
	}
	result := watchCycleResult{changed: true, summary: summary}
	if summary.Failed == 0 {
		*previous = snapshot.signature
		*initialized = true
	}
	return result, nil
}

func discoverWatchSnapshot(ctx context.Context, c *config.Config, projectDir string) (watchSnapshot, error) {
	if ctx == nil {
		return watchSnapshot{}, errors.New("watch: context is required")
	}
	if err := ctx.Err(); err != nil {
		return watchSnapshot{}, fmt.Errorf("watch: %w", err)
	}
	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return watchSnapshot{}, fmt.Errorf("watch: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return watchSnapshot{}, fmt.Errorf("watch: %s", reason)
	}
	agents, err := adapter.DiscoverInstalled(ctx, current.Root)
	if err != nil {
		return watchSnapshot{}, fmt.Errorf("watch: discover local agents: %w", err)
	}
	if len(agents) == 0 {
		return watchSnapshot{}, errors.New("watch: no supported coding agent is installed; install Claude Code or Codex CLI")
	}
	var refs []adapter.SessionRef
	for _, agent := range agents {
		refs = append(refs, agent.Sessions...)
	}
	return watchSnapshot{signature: watchSessionSignature(refs)}, nil
}

func watchSessionSignature(refs []adapter.SessionRef) string {
	ordered := append([]adapter.SessionRef(nil), refs...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].NativeID < ordered[j].NativeID
	})

	var b strings.Builder
	for _, ref := range ordered {
		fmt.Fprintf(&b, "%s\x00%d\x00%d\x00%d\x00%s\n",
			ref.NativeID,
			ref.Size,
			ref.CreatedAt.UTC().UnixNano(),
			ref.UpdatedAt.UTC().UnixNano(),
			ref.ProjectPath,
		)
	}
	return b.String()
}

func safeWatchError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "push cycle timed out"
	case strings.Contains(err.Error(), "Claude Code is not installed"):
		return "Claude Code is not installed"
	case strings.Contains(err.Error(), "Codex is not installed"):
		return "Codex is not installed"
	case strings.Contains(err.Error(), "no supported coding agent"):
		return "no supported coding agent is installed"
	case strings.Contains(err.Error(), "stable project identity"):
		return "the current directory has no stable project identity"
	default:
		return "push cycle failed; run 'ctxhop push' for details"
	}
}

func writeWatchEvent(w io.Writer, jsonOutput bool, event watchEvent) error {
	if event.Event != "started" && event.Event != "push" && event.Event != "error" {
		return fmt.Errorf("watch: unsupported event %q", event.Event)
	}
	if jsonOutput {
		encoder := json.NewEncoder(w)
		return encoder.Encode(event)
	}
	switch event.Event {
	case "started":
		_, err := fmt.Fprintf(w, "watching: interval=%s\n", event.Interval)
		return err
	case "push":
		_, err := fmt.Fprintf(w, "push: pushed: %d, failed: %d, skipped: %d\n", event.Pushed, event.Failed, event.Skipped)
		return err
	case "error":
		_, err := fmt.Fprintf(w, "watch error: %s\n", event.Error)
		return err
	default:
		return fmt.Errorf("watch: unsupported event %q", event.Event)
	}
}
