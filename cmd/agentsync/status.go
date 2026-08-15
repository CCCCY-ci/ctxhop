package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/project"
)

const statusProjectTimeout = 2 * time.Second

// statusOptions controls the output format without changing what status
// reads. In particular, status never contacts the configured backend.
type statusOptions struct {
	json bool
}

// statusReport is deliberately a CLI-owned, redacted view. Keeping it apart
// from Config prevents a future configuration field from being printed by
// accident.
type statusReport struct {
	Scope         string              `json:"scope"`
	Configuration statusConfiguration `json:"configuration"`
	Project       statusProject       `json:"project"`
}

type statusConfiguration struct {
	Version  int              `json:"version"`
	Remote   statusRemote     `json:"remote"`
	Device   statusReadiness  `json:"device"`
	Identity statusReadiness  `json:"identity"`
	Projects statusProjectSet `json:"projects"`
	Agents   []statusAgent    `json:"agents,omitempty"`
}

type statusRemote struct {
	Type       string `json:"type,omitempty"`
	Configured bool   `json:"configured"`
	Endpoint   bool   `json:"endpointSet"`
}

type statusReadiness struct {
	Configured bool `json:"configured"`
}

type statusProjectSet struct {
	Bound    int `json:"bound"`
	Excluded int `json:"excluded"`
	PushOnly int `json:"pushOnly"`
}

type statusAgent struct {
	Name          string `json:"name"`
	HookInstalled bool   `json:"hookInstalled"`
}

type statusProject struct {
	Detected     bool   `json:"detected"`
	IdentityKind string `json:"identityKind"`
	Bound        bool   `json:"bound"`
	Reason       string `json:"reason,omitempty"`
}

func init() {
	for i := range commands {
		if commands[i].name == "status" {
			commands[i].run = runStatus
		}
	}
}

func runStatus(args []string) error {
	options, err := parseStatusOptions(args)
	if err != nil {
		return err
	}

	dir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(dir)
	if err != nil {
		return err
	}

	report, err := collectStatus(c, ".")
	if err != nil {
		return err
	}
	if options.json {
		return writeStatusJSON(os.Stdout, report)
	}
	return writeStatusText(os.Stdout, report)
}

func parseStatusOptions(args []string) (statusOptions, error) {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return statusOptions{}, fmt.Errorf("status: %w", err)
	}
	if flags.NArg() != 0 {
		return statusOptions{}, fmt.Errorf("status: unexpected argument %q", flags.Arg(0))
	}
	return statusOptions{json: *jsonOutput}, nil
}

func collectStatus(c *config.Config, dir string) (statusReport, error) {
	if c == nil {
		return statusReport{}, fmt.Errorf("status: configuration is unavailable")
	}

	summary := c.Summarise()
	report := statusReport{
		Scope: "global",
		Configuration: statusConfiguration{
			Version: summary.Version,
			Remote: statusRemote{
				Type:       safeRemoteType(summary.RemoteType),
				Configured: summary.RemoteConfigured,
				Endpoint:   summary.EndpointSet,
			},
			Device:   statusReadiness{Configured: summary.DeviceIdentified},
			Identity: statusReadiness{Configured: summary.IdentityPinned},
			Projects: statusProjectSet{
				Bound:    summary.BoundProjects,
				Excluded: summary.ExcludedProjects,
				PushOnly: summary.PushOnlyProjects,
			},
		},
	}

	for name, state := range summary.Agents {
		report.Configuration.Agents = append(report.Configuration.Agents, statusAgent{
			Name:          name,
			HookInstalled: state.HookInstalled,
		})
	}
	sort.Slice(report.Configuration.Agents, func(i, j int) bool {
		return report.Configuration.Agents[i].Name < report.Configuration.Agents[j].Name
	})

	ctx, cancel := context.WithTimeout(context.Background(), statusProjectTimeout)
	defer cancel()
	current, err := project.Identify(ctx, dir)
	if err != nil {
		return statusReport{}, fmt.Errorf("status: identify the current project: %w", err)
	}

	identityKind := string(current.Identity.Kind)
	if !current.Identity.Stable() {
		identityKind = string(project.KindNone)
	}
	report.Project = statusProject{
		Detected:     current.Identity.Stable(),
		IdentityKind: identityKind,
		Reason:       current.Reason,
	}
	if current.Identity.Stable() {
		report.Scope = "project"
		report.Project.Bound = bindingExists(c, current.Identity.Value)
	}
	return report, nil
}

func safeRemoteType(value string) string {
	switch value {
	case "dir", "s3":
		return value
	case "":
		return ""
	default:
		return "unknown"
	}
}

func bindingExists(c *config.Config, identity string) bool {
	for _, binding := range c.Projects.Bindings {
		if binding.Identity == identity {
			return true
		}
	}
	return false
}

func writeStatusJSON(w io.Writer, report statusReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeStatusText(w io.Writer, report statusReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "configuration:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  version: %d\n", report.Configuration.Version); err != nil {
		return err
	}
	remote := "not configured"
	if report.Configuration.Remote.Configured {
		remote = "configured"
	}
	if report.Configuration.Remote.Type != "" {
		remote += " (" + report.Configuration.Remote.Type
		if report.Configuration.Remote.Endpoint {
			remote += ", endpoint set"
		}
		remote += ")"
	}
	if _, err := fmt.Fprintf(w, "  remote: %s\n", remote); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  device identity: %s\n", readiness(report.Configuration.Device.Configured)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  encryption identity: %s\n", readiness(report.Configuration.Identity.Configured)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  projects: bound=%d excluded=%d push-only=%d\n",
		report.Configuration.Projects.Bound,
		report.Configuration.Projects.Excluded,
		report.Configuration.Projects.PushOnly); err != nil {
		return err
	}
	if len(report.Configuration.Agents) > 0 {
		if _, err := fmt.Fprintln(w, "  agents:"); err != nil {
			return err
		}
		for _, agent := range report.Configuration.Agents {
			if _, err := fmt.Fprintf(w, "    %s: hook-%s\n", agent.Name, hookState(agent.HookInstalled)); err != nil {
				return err
			}
		}
	}

	if _, err := fmt.Fprintln(w, "project:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  identity: %s\n", projectIdentity(report.Project)); err != nil {
		return err
	}
	if report.Project.Detected {
		binding := "not configured"
		if report.Project.Bound {
			binding = "configured"
		}
		if _, err := fmt.Fprintf(w, "  binding: %s\n", binding); err != nil {
			return err
		}
	}
	if report.Project.Reason != "" {
		_, err := fmt.Fprintf(w, "  note: %s\n", report.Project.Reason)
		return err
	}
	return nil
}

func readiness(configured bool) string {
	if configured {
		return "configured"
	}
	return "not configured"
}

func hookState(installed bool) string {
	if installed {
		return "installed"
	}
	return "not-installed"
}

func projectIdentity(status statusProject) string {
	if !status.Detected {
		return "not detected"
	}
	return "detected (" + status.IdentityKind + ")"
}
