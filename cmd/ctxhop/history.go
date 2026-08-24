package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const historyTimeout = 10 * time.Minute

type historyOptions struct {
	json    bool
	session string
}

type historyReport struct {
	Scope        string           `json:"scope"`
	Session      string           `json:"session"`
	Title        string           `json:"title"`
	Resolution   string           `json:"resolution"`
	CommonPrefix uint64           `json:"commonPrefix"`
	Versions     []historyVersion `json:"versions"`
}

type historyVersion struct {
	Index       int        `json:"index"`
	RecordCount uint64     `json:"recordCount"`
	Sources     []string   `json:"sources"`
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
}

type historyCandidate struct {
	Group      syncer.ProjectMetadataRef
	Summary    syncflow.SessionSummary
	HasSummary bool
}

func init() {
	for i := range commands {
		if commands[i].name == "history" {
			commands[i].run = runHistory
		}
	}
}

func runHistory(args []string) error {
	if isHistoryMaintenance(args) {
		return runHistoryMaintenanceWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
	}
	return runHistoryWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runHistoryWithIO(args []string, input io.Reader, output io.Writer) error {
	if isHistoryMaintenance(args) {
		return runHistoryMaintenanceWithStreams(args, input, output, output)
	}
	return runHistoryWithStreams(args, input, output, output)
}

func runHistoryWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	if isHistoryMaintenance(args) {
		return runHistoryMaintenanceWithStreams(args, input, output, prompt)
	}
	options, err := parseHistoryOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("history: input is required")
	}
	if output == nil {
		return errors.New("history: output is required")
	}
	if prompt == nil {
		return errors.New("history: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), historyTimeout)
	defer cancel()
	report, err := collectHistory(ctx, c, configDir, ".", options.session, input, prompt)
	if err != nil {
		return err
	}
	if options.json {
		return writeHistoryJSON(output, report)
	}
	return writeHistoryText(output, report)
}

func parseHistoryOptions(args []string) (historyOptions, error) {
	flags := flag.NewFlagSet("history", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	jsonOutput := flags.Bool("json", false, "write machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return historyOptions{}, fmt.Errorf("history: %w", err)
	}
	if flags.NArg() != 1 {
		if flags.NArg() == 0 {
			return historyOptions{}, errors.New("history: a session ID is required")
		}
		return historyOptions{}, fmt.Errorf("history: unexpected argument %q", flags.Arg(1))
	}
	session := flags.Arg(0)
	if session == "" {
		return historyOptions{}, errors.New("history: session ID cannot be empty")
	}
	if strings.ContainsRune(session, 0) {
		return historyOptions{}, errors.New("history: session ID contains an invalid character")
	}
	return historyOptions{json: *jsonOutput, session: session}, nil
}

func collectHistory(ctx context.Context, c *config.Config, configDir, projectDir, requested string, input io.Reader, prompt io.Writer) (historyReport, error) {
	if ctx == nil {
		return historyReport{}, errors.New("history: context is required")
	}
	if c == nil {
		return historyReport{}, errors.New("history: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return historyReport{}, fmt.Errorf("history: %w", err)
	}
	if input == nil {
		return historyReport{}, errors.New("history: input is required")
	}
	if prompt == nil {
		return historyReport{}, errors.New("history: prompt output is required")
	}
	if err := devicePullError("history", c); err != nil {
		return historyReport{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return historyReport{}, fmt.Errorf("history: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return historyReport{}, errors.New("history: encryption identity is not configured; run 'ctxhop init'")
	}

	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return historyReport{}, fmt.Errorf("history: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return historyReport{}, fmt.Errorf("history: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return historyReport{}, errors.New("history: project is excluded from synchronization")
	case projectModePushOnly:
		return historyReport{}, errors.New("history: project is configured as push-only; remote sessions are unavailable")
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return historyReport{}, fmt.Errorf("history: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return historyReport{}, fmt.Errorf("history: derive project identity: %w", err)
	}
	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "history")
	if err != nil {
		return historyReport{}, err
	}
	defer access.close()
	store := access.Store
	identities := access.Identities

	groups, err := syncer.FetchProjectMetadataWithIdentitiesAndDevices(ctx, store, projectID, identities, access.allowedDevices())
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return historyReport{}, errors.New("history: no encrypted sessions are available for this project")
	}
	if err != nil {
		return historyReport{}, fmt.Errorf("history: read encrypted session metadata: %w", err)
	}
	candidate, err := chooseHistoryCandidate(groups, projectID, secrets.IdentifierKey, requested)
	if err != nil {
		return historyReport{}, err
	}

	branches, err := syncer.FetchCompleteBranchesWithIdentitiesAndDevices(ctx, store, projectID, candidate.Group.SessionID, identities, access.allowedDevices())
	if err != nil {
		return historyReport{}, safeHistoryReadError(ctx, err)
	}
	resolution, err := syncer.ResolveBranches(branches)
	if err != nil {
		return historyReport{}, safeHistoryReadError(ctx, err)
	}
	return buildHistoryReport(c.Device.ID, candidate, resolution), nil
}

func chooseHistoryCandidate(groups []syncer.ProjectMetadataRef, projectID string, identifierKey []byte, requested string) (historyCandidate, error) {
	for _, group := range groups {
		summary, ok := bestResumeSummary(group)
		if group.SessionID == requested || ok && summary.NativeID == requested {
			return historyCandidate{Group: group, Summary: summary, HasSummary: ok}, nil
		}
	}

	sessionID, err := crypto.SessionID(identifierKey, projectID, requested)
	if err == nil {
		for _, group := range groups {
			if group.SessionID != sessionID {
				continue
			}
			summary, ok := bestResumeSummary(group)
			return historyCandidate{Group: group, Summary: summary, HasSummary: ok}, nil
		}
	}
	return historyCandidate{}, errors.New("history: requested session is not available for this project")
}

func buildHistoryReport(localDeviceID string, candidate historyCandidate, resolution syncer.Resolution) historyReport {
	session := candidate.Group.SessionID
	title := "encrypted session metadata"
	if candidate.HasSummary {
		session = safeListText(candidate.Summary.NativeID)
		title = safeListText(candidate.Summary.Title)
	}
	versions := make([]historyVersion, 0, len(resolution.Versions))
	for index, version := range resolution.Versions {
		sources := make([]string, 0, len(version.Devices))
		for _, deviceID := range version.Devices {
			sources = append(sources, listSource(localDeviceID, deviceID))
		}
		sort.Strings(sources)

		entry := historyVersion{
			Index:       index,
			RecordCount: uint64(len(version.Records)),
			Sources:     sources,
		}
		if updated := matchingHistoryUpdate(candidate.Group, version); !updated.IsZero() {
			updated = updated.UTC()
			entry.UpdatedAt = &updated
		}
		versions = append(versions, entry)
	}
	return historyReport{
		Scope:        "session",
		Session:      session,
		Title:        title,
		Resolution:   resolution.Kind.String(),
		CommonPrefix: resolution.CommonPrefix,
		Versions:     versions,
	}
}

func matchingHistoryUpdate(group syncer.ProjectMetadataRef, version syncer.Version) time.Time {
	wantCount := uint64(len(version.Records))
	var latest time.Time
	for _, deviceID := range version.Devices {
		for _, ref := range group.Devices {
			if ref.DeviceID != deviceID || ref.Metadata.RecordCount != wantCount || ref.Metadata.HeadDigest != version.HeadDigest {
				continue
			}
			summary, err := syncflow.DecodeSessionSummary(ref.Metadata.Payload)
			if err != nil {
				continue
			}
			if summary.UpdatedAt.After(latest) {
				latest = summary.UpdatedAt
			}
		}
	}
	return latest
}

func safeHistoryReadError(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("history: %w", ctx.Err())
	}
	if errors.Is(err, syncer.ErrIncompleteRemoteSession) {
		return errors.New("history: remote session is incomplete; retry later")
	}
	if errors.Is(err, syncer.ErrNoRemoteBranches) {
		return errors.New("history: no complete remote versions are available")
	}
	return errors.New("history: remote session could not be read safely")
}

func writeHistoryJSON(w io.Writer, report historyReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeHistoryText(w io.Writer, report historyReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", report.Session); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "title: %s\n", report.Title); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "resolution: %s\n", report.Resolution); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "common-prefix: %d\n", report.CommonPrefix); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "versions: %d\n", len(report.Versions)); err != nil {
		return err
	}
	for _, version := range report.Versions {
		updated := "unknown"
		if version.UpdatedAt != nil {
			updated = version.UpdatedAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(w, "- version=%d records=%d updated=%s sources=%s\n",
			version.Index, version.RecordCount, updated, strings.Join(version.Sources, ",")); err != nil {
			return err
		}
	}
	return nil
}
