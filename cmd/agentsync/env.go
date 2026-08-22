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

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/environment"
)

const envPreviewTimeout = 15 * time.Second

type envOptions struct {
	action  string
	session string
	json    bool
	yes     bool
}

type environmentPreviewReport struct {
	Scope        string                         `json:"scope"`
	Session      string                         `json:"session"`
	Agent        string                         `json:"agent,omitempty"`
	NativeID     string                         `json:"nativeId,omitempty"`
	Dependencies []environment.Reference        `json:"dependencies,omitempty"`
	Requirements []environmentRequirementChange `json:"requirements,omitempty"`
	HookState    string                         `json:"hookState,omitempty"`
	Components   []environment.Component        `json:"components,omitempty"`
	Changes      []environmentComponentChange   `json:"changes,omitempty"`
	Status       string                         `json:"status"`
	Notes        []string                       `json:"notes"`
}

func init() {
	for i := range commands {
		if commands[i].name == "env" {
			commands[i].run = runEnvironment
		}
	}
}

func runEnvironment(args []string) error {
	return runEnvironmentWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runEnvironmentWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseEnvironmentOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("env: input is required")
	}
	if output == nil {
		return errors.New("env: output is required")
	}
	if prompt == nil {
		return errors.New("env: prompt output is required")
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), envPreviewTimeout)
	defer cancel()

	state, err := collectEnvironmentContext(ctx, c, configDir, ".", input, prompt)
	if err != nil {
		return err
	}
	defer state.Access.close()
	session := findEnvironmentSession(state.List.Sessions, options.session)
	if session == nil {
		return fmt.Errorf("env %s: session %q was not found in the current project", options.action, options.session)
	}
	report := buildEnvironmentPreviewReport(ctx, state, session)
	var applyErr error
	if options.action == "apply" {
		if !options.yes {
			report.Status = "confirmation-required"
			report.Notes = append(report.Notes, "no files changed; rerun with 'env apply --yes' to write filtered Codex component values")
		} else {
			applyErr = applyEnvironmentComponents(ctx, state, session, &report)
		}
	}
	var writeErr error
	if options.json {
		writeErr = writeEnvironmentPreviewJSON(output, report)
	} else {
		writeErr = writeEnvironmentPreviewText(output, report)
	}
	if writeErr != nil {
		return writeErr
	}
	return applyErr
}

func parseEnvironmentOptions(args []string) (envOptions, error) {
	if len(args) == 0 {
		return envOptions{}, errors.New("env: expected 'preview|apply <SESSION_ID>'")
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	if action == "" {
		return envOptions{}, errors.New("env: expected 'preview|apply <SESSION_ID>'")
	}
	if action != "preview" && action != "apply" {
		return envOptions{}, fmt.Errorf("env: unknown action %q; expected preview or apply", action)
	}
	flags := flag.NewFlagSet("env "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	yes := flags.Bool("yes", false, "confirm writing filtered Codex component values")
	if err := flags.Parse(args[1:]); err != nil {
		return envOptions{}, fmt.Errorf("env %s: %w", action, err)
	}
	if action == "preview" && *yes {
		return envOptions{}, errors.New("env preview: --yes is only valid with apply")
	}
	if flags.NArg() != 1 {
		return envOptions{}, fmt.Errorf("env %s: expected one session ID", action)
	}
	session := strings.TrimSpace(flags.Arg(0))
	if session == "" || strings.ContainsRune(session, 0) {
		return envOptions{}, errors.New("env: session ID is invalid")
	}
	return envOptions{action: action, session: session, json: *jsonOutput, yes: *yes}, nil
}

func findEnvironmentSession(sessions []listSession, requested string) *listSession {
	for i := range sessions {
		if sessions[i].RemoteID == requested {
			return &sessions[i]
		}
	}
	for i := range sessions {
		if sessions[i].NativeID == requested {
			return &sessions[i]
		}
	}
	return nil
}

func writeEnvironmentPreviewJSON(w io.Writer, report environmentPreviewReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeEnvironmentPreviewText(w io.Writer, report environmentPreviewReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if report.NativeID != "" {
		if _, err := fmt.Fprintf(w, "native session: %s\n", safeListText(report.NativeID)); err != nil {
			return err
		}
	}
	if report.Agent != "" {
		if _, err := fmt.Fprintf(w, "agent: %s\n", safeListText(report.Agent)); err != nil {
			return err
		}
	}
	if report.HookState != "" {
		if _, err := fmt.Fprintf(w, "local hook: %s\n", safeListText(report.HookState)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "status: %s\n", report.Status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "dependencies: %d\n", len(report.Dependencies)); err != nil {
		return err
	}
	for _, dependency := range report.Dependencies {
		version := ""
		if dependency.Version != "" {
			version = " version=" + safeListText(dependency.Version)
		}
		if _, err := fmt.Fprintf(w, "- kind=%s name=%s portability=%s%s\n",
			safeListText(dependency.Kind),
			safeListText(dependency.Name),
			safeListText(dependency.Portability),
			version,
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "tool requirements: %d\n", len(report.Requirements)); err != nil {
		return err
	}
	for _, requirement := range report.Requirements {
		line := fmt.Sprintf("- requirement kind=%s name=%s state=%s",
			safeListText(requirement.Dependency.Kind),
			safeListText(requirement.Dependency.Name),
			safeListText(requirement.State),
		)
		if requirement.Dependency.Version != "" {
			line += " observed-version=" + safeListText(requirement.Dependency.Version)
		}
		if requirement.LocalVersion != "" {
			line += " local-version=" + safeListText(requirement.LocalVersion)
		}
		if requirement.LocalVersionSource != "" {
			line += " local-version-source=" + safeListText(requirement.LocalVersionSource)
		}
		if requirement.Reason != "" {
			line += " reason=" + safeListText(requirement.Reason)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "components: %d\n", len(report.Components)); err != nil {
		return err
	}
	for _, component := range report.Components {
		if _, err := fmt.Fprintf(w, "- component kind=%s name=%s scope=%s size=%d fingerprint=%s\n",
			safeListText(component.Kind),
			safeListText(component.Name),
			safeListText(component.Scope),
			component.Size,
			safeListText(component.Fingerprint),
		); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "changes: %d\n", len(report.Changes)); err != nil {
		return err
	}
	for _, change := range report.Changes {
		line := fmt.Sprintf("- change kind=%s name=%s scope=%s state=%s",
			safeListText(change.Component.Kind),
			safeListText(change.Component.Name),
			safeListText(change.Component.Scope),
			safeListText(change.State),
		)
		if change.Path != "" {
			line += " path=" + safeListText(change.Path)
		}
		if change.Backup != "" {
			line += " backup=" + safeListText(change.Backup)
		}
		if change.Reason != "" {
			line += " reason=" + safeListText(change.Reason)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	for _, note := range report.Notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", safeListText(note)); err != nil {
			return err
		}
	}
	return nil
}
