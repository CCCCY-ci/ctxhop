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
}

type environmentPreviewReport struct {
	Scope        string                  `json:"scope"`
	Session      string                  `json:"session"`
	Agent        string                  `json:"agent,omitempty"`
	NativeID     string                  `json:"nativeId,omitempty"`
	Dependencies []environment.Reference `json:"dependencies,omitempty"`
	Components   []environment.Component `json:"components,omitempty"`
	Status       string                  `json:"status"`
	Notes        []string                `json:"notes"`
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
	if options.action != "preview" {
		return fmt.Errorf("env: %s is not implemented; only 'env preview' is currently available", options.action)
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

	list, err := collectListWithPrompt(ctx, c, configDir, ".", input, output, prompt)
	if err != nil {
		return err
	}
	session := findEnvironmentSession(list.Sessions, options.session)
	if session == nil {
		return fmt.Errorf("env preview: session %q was not found in the current project", options.session)
	}
	report := environmentPreviewReport{
		Scope:        list.Scope,
		Session:      session.RemoteID,
		Agent:        session.Agent,
		NativeID:     session.NativeID,
		Dependencies: append([]environment.Reference(nil), session.Dependencies...),
		Components:   append([]environment.Component(nil), session.Components...),
		Status:       "observed-only",
		Notes: []string{
			"only structured dependencies and filtered component summaries recorded in the encrypted env manifest are shown",
			"component bodies are not applied; no local files or commands were changed",
		},
	}
	if len(report.Dependencies) == 0 {
		report.Notes = append(report.Notes, "no structured skill or MCP dependency was observed in this session")
	}
	if options.json {
		return writeEnvironmentPreviewJSON(output, report)
	}
	return writeEnvironmentPreviewText(output, report)
}

func parseEnvironmentOptions(args []string) (envOptions, error) {
	if len(args) == 0 {
		return envOptions{}, errors.New("env: expected 'preview <SESSION_ID>'")
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	if action == "" {
		return envOptions{}, errors.New("env: expected 'preview <SESSION_ID>'")
	}
	flags := flag.NewFlagSet("env "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return envOptions{}, fmt.Errorf("env %s: %w", action, err)
	}
	if flags.NArg() != 1 {
		return envOptions{}, fmt.Errorf("env %s: expected one session ID", action)
	}
	session := strings.TrimSpace(flags.Arg(0))
	if session == "" || strings.ContainsRune(session, 0) {
		return envOptions{}, errors.New("env: session ID is invalid")
	}
	return envOptions{action: action, session: session, json: *jsonOutput}, nil
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
	for _, note := range report.Notes {
		if _, err := fmt.Fprintf(w, "note: %s\n", safeListText(note)); err != nil {
			return err
		}
	}
	return nil
}
