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

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/diagnostic"
	"github.com/CCCCY-ci/agentsync/internal/remote"
)

const doctorProbeTimeout = 5 * time.Second

type doctorReport struct {
	Configuration statusConfiguration `json:"configuration"`
	Backend       doctorCheck         `json:"backend"`
	Agent         doctorAgent         `json:"agent"`
	Project       statusProject       `json:"project"`
	RecentErrors  doctorRecentErrors  `json:"recentErrors"`
}

type doctorCheck struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type doctorAgent struct {
	Name          string `json:"name"`
	Installed     bool   `json:"installed"`
	Version       string `json:"version,omitempty"`
	Compatibility string `json:"compatibility,omitempty"`
	Hook          string `json:"hook"`
	Reason        string `json:"reason,omitempty"`
}

type doctorRecentErrors struct {
	Status string                  `json:"status"`
	Count  int                     `json:"count"`
	Events []diagnostic.ErrorEvent `json:"events,omitempty"`
	Reason string                  `json:"reason,omitempty"`
}

func init() {
	for i := range commands {
		if commands[i].name == "doctor" {
			commands[i].run = runDoctor
		}
	}
}

func runDoctor(args []string) error {
	jsonOutput, err := parseDoctorOptions(args)
	if err != nil {
		return err
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	report, err := collectDoctor(c, configDir, ".")
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeDoctorJSON(os.Stdout, report)
	}
	return writeDoctorText(os.Stdout, report)
}

func parseDoctorOptions(args []string) (bool, error) {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return false, fmt.Errorf("doctor: %w", err)
	}
	if flags.NArg() != 0 {
		return false, fmt.Errorf("doctor: unexpected argument %q", flags.Arg(0))
	}
	return *jsonOutput, nil
}

func collectDoctor(c *config.Config, configDir, projectDir string) (doctorReport, error) {
	status, err := collectStatus(c, projectDir)
	if err != nil {
		return doctorReport{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorProbeTimeout)
	defer cancel()

	recentErrors := collectRecentErrors(configDir)

	return doctorReport{
		Configuration: status.Configuration,
		Backend:       probeBackend(ctx, c, configDir),
		Agent:         detectAgent(ctx),
		Project:       status.Project,
		RecentErrors:  recentErrors,
	}, nil
}

func collectRecentErrors(configDir string) doctorRecentErrors {
	events, err := diagnostic.Recent(configDir)
	if err != nil {
		return doctorRecentErrors{Status: "unavailable", Reason: "recent error history is unavailable"}
	}
	if len(events) == 0 {
		return doctorRecentErrors{Status: "none"}
	}
	return doctorRecentErrors{Status: "available", Count: len(events), Events: events}
}

func probeBackend(ctx context.Context, c *config.Config, configDir string) doctorCheck {
	if c == nil {
		return doctorCheck{Status: "not-configured", Reason: "configuration is unavailable; run agentsync init"}
	}

	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return doctorCheck{Status: "not-configured", Reason: safeBackendSetupError(err)}
	}
	prober, ok := store.(remote.Prober)
	if !ok {
		return doctorCheck{Status: "failed", Reason: "the configured backend cannot verify read and write access"}
	}
	if err := prober.Probe(ctx); err != nil {
		return doctorCheck{Status: "failed", Reason: safeBackendProbeError(err)}
	}
	return doctorCheck{Status: "passed"}
}

func buildConfiguredRemote(c *config.Config, configDir string) (remote.Remote, error) {
	switch strings.ToLower(strings.TrimSpace(c.Remote.Type)) {
	case "dir":
		if strings.TrimSpace(c.Remote.Path) == "" {
			return nil, errors.New("directory backend path is missing")
		}
		return remote.NewDir(c.Remote.Path)
	case "s3":
		if strings.TrimSpace(c.Remote.Endpoint) == "" || strings.TrimSpace(c.Remote.Bucket) == "" {
			return nil, errors.New("S3 endpoint or bucket is missing")
		}
		secrets, err := config.LoadSecrets(configDir)
		if err != nil {
			return nil, err
		}
		return remote.NewS3(remote.S3Config{
			Endpoint:     c.Remote.Endpoint,
			Region:       c.Remote.Region,
			Bucket:       c.Remote.Bucket,
			Prefix:       c.Remote.Prefix,
			AccessKey:    secrets.Credentials.AccessKeyID,
			SecretKey:    secrets.Credentials.SecretAccessKey,
			SessionToken: secrets.Credentials.SessionToken,
		})
	default:
		return nil, errors.New("storage backend type is unknown")
	}
}

func detectAgent(ctx context.Context) doctorAgent {
	result := doctorAgent{Name: "claude-code", Hook: "not-installed"}
	home, err := adapter.DefaultHome()
	if err != nil {
		result.Reason = "the agent data directory could not be located"
		return result
	}
	layout := adapter.Layout{Home: home}
	installation, err := layout.Detect(ctx)
	if errors.Is(err, adapter.ErrNotInstalled) {
		result.Reason = "Claude Code is not installed"
		return result
	}
	if err != nil {
		result.Reason = "the Claude Code installation could not be inspected"
		return result
	}

	result.Installed = true
	result.Version = safeAgentVersion(installation.Version)
	result.Compatibility = compatibilityName(installation.Compatibility)
	result.Reason = installation.CompatibilityReason
	installed, err := layout.HookInstalled()
	if err != nil {
		result.Hook = "error"
		if result.Reason == "" {
			result.Reason = "the AgentSync hook could not be inspected"
		}
	} else if installed {
		result.Hook = "installed"
	}
	return result
}

func safeAgentVersion(version string) string {
	if version == "" || len(version) > 64 {
		return "unknown"
	}
	for _, r := range version {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || strings.ContainsRune(".-+", r) {
			continue
		}
		return "unknown"
	}
	return version
}

func compatibilityName(level adapter.Compatibility) string {
	switch level {
	case adapter.CompatFull:
		return "full"
	case adapter.CompatLimited:
		return "limited"
	case adapter.CompatStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func safeBackendSetupError(err error) string {
	switch {
	case errors.Is(err, config.ErrNoSecrets):
		return "backend credentials are unavailable; run agentsync init or set the credential environment variables"
	case errors.Is(err, config.ErrPartialEnvironment):
		return "backend credential environment variables are incomplete"
	default:
		return "backend configuration is incomplete or invalid; run agentsync init"
	}
}

func safeBackendProbeError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "backend probe timed out or was cancelled; check storage availability"
	default:
		return "backend read, write or cleanup probe failed; check storage permissions and credentials"
	}
}

func writeDoctorJSON(w io.Writer, report doctorReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeDoctorText(w io.Writer, report doctorReport) error {
	if _, err := fmt.Fprintln(w, "configuration:"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  remote: %s\n", readiness(report.Configuration.Remote.Configured)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  device identity: %s\n", readiness(report.Configuration.Device.Configured)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  encryption identity: %s\n", readiness(report.Configuration.Identity.Configured)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "backend: %s\n", report.Backend.Status); err != nil {
		return err
	}
	if report.Backend.Reason != "" {
		if _, err := fmt.Fprintf(w, "  note: %s\n", report.Backend.Reason); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "agent %s: %s", report.Agent.Name, agentState(report.Agent)); err != nil {
		return err
	}
	if report.Agent.Reason != "" {
		if _, err := fmt.Fprintf(w, " (%s)", report.Agent.Reason); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "project: %s\n", projectIdentity(report.Project)); err != nil {
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
		if err != nil {
			return err
		}
	}
	return writeDoctorRecentErrors(w, report.RecentErrors)
}

func writeDoctorRecentErrors(w io.Writer, recent doctorRecentErrors) error {
	switch recent.Status {
	case "none":
		_, err := fmt.Fprintln(w, "recent errors: none")
		return err
	case "unavailable":
		_, err := fmt.Fprintf(w, "recent errors: unavailable (%s)\n", recent.Reason)
		return err
	default:
		if _, err := fmt.Fprintf(w, "recent errors: %d recorded\n", recent.Count); err != nil {
			return err
		}
		for _, event := range recent.Events {
			if _, err := fmt.Fprintf(w, "  - %s command=%s class=%s\n", event.Time.Format(time.RFC3339), event.Command, event.Class); err != nil {
				return err
			}
		}
		return nil
	}
}

func agentState(agent doctorAgent) string {
	if !agent.Installed {
		return "not-installed"
	}
	return fmt.Sprintf("installed, version=%s, compatibility=%s, hook=%s", agent.Version, agent.Compatibility, agent.Hook)
}
