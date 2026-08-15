package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/crypto"
	"github.com/CCCCY-ci/agentsync/internal/project"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
	"github.com/CCCCY-ci/agentsync/internal/syncflow"
)

const resumeTimeout = 60 * time.Second

type resumeOptions struct {
	json            bool
	allowLimited    bool
	allowDivergent  bool
	replaceExisting bool
	version         int
	session         string
}

type resumeReport struct {
	Session     string   `json:"session"`
	Title       string   `json:"title"`
	Workspace   string   `json:"workspace"`
	Differences int      `json:"differences"`
	Replaced    bool     `json:"replaced"`
	Sources     []string `json:"sources"`
}

type resumeCandidate struct {
	Group   syncer.ProjectMetadataRef
	Summary syncflow.SessionSummary
}

func init() {
	for i := range commands {
		if commands[i].name == "resume" {
			commands[i].run = runResume
		}
	}
}

func runResume(args []string) error {
	return runResumeWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runResumeWithIO(args []string, input io.Reader, output io.Writer) error {
	return runResumeWithStreams(args, input, output, output)
}

func runResumeWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseResumeOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("resume: input is required")
	}
	if output == nil {
		return errors.New("resume: output is required")
	}
	if prompt == nil {
		return errors.New("resume: prompt output is required")
	}
	if options.json && options.session == "" {
		return errors.New("resume: --json requires a session argument")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeTimeout)
	defer cancel()
	report, err := collectResumeWithPrompt(ctx, c, configDir, ".", options, input, output, prompt)
	if err != nil {
		return err
	}
	if options.json {
		return writeResumeJSON(output, report)
	}
	return writeResumeText(output, report)
}

func parseResumeOptions(args []string) (resumeOptions, error) {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options resumeOptions
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	flags.BoolVar(&options.allowLimited, "allow-limited", false, "allow restore for an unverified agent version")
	flags.BoolVar(&options.allowDivergent, "allow-divergent", false, "allow restore despite a divergent workspace")
	flags.BoolVar(&options.replaceExisting, "replace-existing", false, "replace an existing local session")
	flags.IntVar(&options.version, "version", -1, "select a zero-based remote fork version")
	if err := flags.Parse(args); err != nil {
		return resumeOptions{}, fmt.Errorf("resume: %w", err)
	}
	if flags.NArg() > 1 {
		return resumeOptions{}, fmt.Errorf("resume: unexpected argument %q", flags.Arg(1))
	}
	if flags.NArg() == 1 {
		options.session = flags.Arg(0)
	}
	if options.version < -1 {
		return resumeOptions{}, errors.New("resume: version must be zero or greater")
	}
	if strings.ContainsRune(options.session, 0) {
		return resumeOptions{}, errors.New("resume: session ID contains an invalid character")
	}
	return options, nil
}

func collectResume(ctx context.Context, c *config.Config, configDir, projectDir string, options resumeOptions, input io.Reader, output io.Writer) (resumeReport, error) {
	return collectResumeWithPrompt(ctx, c, configDir, projectDir, options, input, output, output)
}

func collectResumeWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, options resumeOptions, input io.Reader, output, prompt io.Writer) (resumeReport, error) {
	if c == nil {
		return resumeReport{}, errors.New("resume: configuration is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return resumeReport{}, fmt.Errorf("resume: %w", err)
	}
	if err := devicePullError("resume", c); err != nil {
		return resumeReport{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return resumeReport{}, fmt.Errorf("resume: local device identity is invalid: %w", err)
	}
	if len(c.IdentityPublic) == 0 {
		return resumeReport{}, errors.New("resume: encryption identity is not configured; run 'agentsync init'")
	}

	current, err := project.Identify(ctx, projectDir)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return resumeReport{}, fmt.Errorf("resume: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return resumeReport{}, errors.New("resume: project is excluded from synchronization")
	case projectModePushOnly:
		return resumeReport{}, errors.New("resume: project is configured as push-only; remote sessions are unavailable")
	}
	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: load local sync material: %w", err)
	}
	projectID, err := crypto.ProjectID(secrets.IdentifierKey, current.Identity.Value)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: derive project identity: %w", err)
	}
	store, err := buildConfiguredRemote(c, configDir)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: configure backend: %s", safeBackendSetupError(err))
	}
	keyfile, err := syncer.FetchKeyfile(ctx, store)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: read remote keyfile: %w", err)
	}
	public, err := keyfile.IdentityPublicKey()
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: validate remote identity: %w", err)
	}
	if !bytes.Equal(public.Bytes(), c.IdentityPublic) {
		return resumeReport{}, errors.New("resume: remote encryption identity does not match this configuration")
	}

	prompter := &resumePrompter{reader: bufio.NewReader(input), output: prompt}
	passphrase, err := prompter.secret("Passphrase: ")
	if err != nil {
		return resumeReport{}, err
	}
	dataKey, err := keyfile.UnlockWithPassphrase(passphrase)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: unlock remote keyfile: %w", err)
	}
	defer dataKey.Close()
	identity, err := dataKey.IdentityPrivate()
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: open remote identity: %w", err)
	}
	groups, err := syncer.FetchProjectMetadata(ctx, store, projectID, identity)
	if errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return resumeReport{}, errors.New("resume: no encrypted sessions are available for this project")
	}
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: read encrypted session metadata: %w", err)
	}

	candidate, err := chooseResumeCandidate(groups, projectID, secrets.IdentifierKey, options.session, prompter)
	if err != nil {
		return resumeReport{}, err
	}
	home, err := adapter.DefaultHome()
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: locate Claude Code: %w", err)
	}
	layout := adapter.Layout{Home: home}
	installation, err := layout.Detect(ctx)
	if errors.Is(err, adapter.ErrNotInstalled) {
		return resumeReport{}, errors.New("resume: Claude Code is not installed")
	}
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: inspect Claude Code: %w", err)
	}
	space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installation.DataDir}
	restoreOptions := syncflow.RestoreOptions{AllowLimited: options.allowLimited}
	if options.version >= 0 {
		restoreOptions.VersionIndex = &options.version
	}
	plan, err := syncflow.FetchRestorePlan(ctx, store, projectID, candidate.Group.SessionID, identity, space, installation, restoreOptions)
	if err != nil {
		return resumeReport{}, safeResumePlanError(err)
	}
	fingerprint := resumeFingerprint(candidate.Group, plan)
	if fingerprint == nil {
		return resumeReport{}, errors.New("resume: selected session has no matching workspace fingerprint; push it again from the source device")
	}
	result, err := syncflow.ApplyRestore(ctx, layout, current.Root, candidate.Summary.NativeID, plan, syncflow.RestoreApplyOptions{
		Fingerprint:     fingerprint,
		AllowLimited:    options.allowLimited,
		AllowDivergent:  options.allowDivergent,
		ReplaceExisting: options.replaceExisting,
	})
	if err != nil {
		return resumeReport{}, safeResumeApplyError(err)
	}
	if err := recordResumeStats(ctx, configDir, c.Device.ID, plan.Devices); err != nil {
		return resumeReport{}, fmt.Errorf("resume: restore completed but local statistics could not be saved: %w", err)
	}
	if err := saveResumeObservedTips(ctx, configDir, projectID, candidate.Group.SessionID, c.Device.ID, candidate.Group.Devices, plan.Devices); err != nil {
		return resumeReport{}, fmt.Errorf("resume: restore completed but pull state could not be saved: %w", err)
	}
	sources := append([]string(nil), plan.Devices...)
	sort.Strings(sources)
	return resumeReport{
		Session:     candidate.Summary.NativeID,
		Title:       safeListText(candidate.Summary.Title),
		Workspace:   result.Workspace.Verdict.String(),
		Differences: len(result.Workspace.Files),
		Replaced:    result.Replaced,
		Sources:     sources,
	}, nil
}

func chooseResumeCandidate(groups []syncer.ProjectMetadataRef, projectID string, identifierKey []byte, requested string, prompter *resumePrompter) (resumeCandidate, error) {
	candidates := make([]resumeCandidate, 0, len(groups))
	for _, group := range groups {
		summary, ok := bestResumeSummary(group)
		if ok {
			candidates = append(candidates, resumeCandidate{Group: group, Summary: summary})
		}
	}
	if requested != "" {
		for _, candidate := range candidates {
			if candidate.Summary.NativeID == requested || candidate.Group.SessionID == requested {
				return candidate, nil
			}
		}
		if sessionID, err := crypto.SessionID(identifierKey, projectID, requested); err == nil {
			for _, group := range groups {
				if group.SessionID != sessionID {
					continue
				}
				summary, ok := bestResumeSummary(group)
				if !ok {
					break
				}
				return resumeCandidate{Group: group, Summary: summary}, nil
			}
		}
		return resumeCandidate{}, errors.New("resume: requested session is not available for this project")
	}
	if len(candidates) == 0 {
		return resumeCandidate{}, errors.New("resume: remote sessions have no supported listing metadata")
	}
	if prompter == nil {
		return resumeCandidate{}, errors.New("resume: interactive session selection is unavailable")
	}
	if _, err := fmt.Fprintln(prompter.output, "Available sessions:"); err != nil {
		return resumeCandidate{}, err
	}
	for i, candidate := range candidates {
		if _, err := fmt.Fprintf(prompter.output, "  %d. %s [%s]\n", i+1, safeListText(candidate.Summary.Title), safeListText(candidate.Summary.NativeID)); err != nil {
			return resumeCandidate{}, err
		}
	}
	value, err := prompter.line("Select session number: ")
	if err != nil {
		return resumeCandidate{}, err
	}
	choice, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || choice < 1 || choice > len(candidates) {
		return resumeCandidate{}, errors.New("resume: session selection is invalid")
	}
	return candidates[choice-1], nil
}

func bestResumeSummary(group syncer.ProjectMetadataRef) (syncflow.SessionSummary, bool) {
	var best syncflow.SessionSummary
	found := false
	for _, device := range group.Devices {
		summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
		if err != nil {
			continue
		}
		if !found || summary.UpdatedAt.After(best.UpdatedAt) {
			best = summary
			found = true
		}
	}
	return best, found
}

func resumeFingerprint(group syncer.ProjectMetadataRef, plan syncflow.RestorePlan) *project.Fingerprint {
	wantCount := uint64(len(plan.CanonicalRecords))
	for _, deviceID := range plan.Devices {
		for _, device := range group.Devices {
			if device.DeviceID != deviceID || device.Metadata.RecordCount != wantCount || device.Metadata.HeadDigest != plan.HeadDigest {
				continue
			}
			summary, err := syncflow.DecodeSessionSummary(device.Metadata.Payload)
			if err == nil && summary.Fingerprint != nil {
				return summary.Fingerprint
			}
		}
	}
	return nil
}

func recordResumeStats(ctx context.Context, stateRoot, localDeviceID string, selectedDevices []string) error {
	store, err := syncer.NewRestoreStatsStore(stateRoot)
	if err != nil {
		return err
	}
	_, err = store.RecordRestore(ctx, localDeviceID, selectedDevices, time.Now().UTC())
	return err
}

func saveResumeObservedTips(ctx context.Context, stateRoot, projectID, sessionID, localDeviceID string, refs []syncer.MetadataRef, selectedDevices []string) error {
	layout, err := syncer.NewObjectLayout(projectID, sessionID, localDeviceID)
	if err != nil {
		return err
	}
	pullTipStore, err := syncer.NewPullTipStore(stateRoot, layout)
	if err != nil {
		return err
	}
	selected := make(map[string]struct{}, len(selectedDevices))
	for _, deviceID := range selectedDevices {
		if deviceID == localDeviceID {
			continue
		}
		selected[deviceID] = struct{}{}
	}
	tips := make([]syncflow.RemoteTip, 0, len(selected))
	for _, ref := range refs {
		if _, ok := selected[ref.DeviceID]; !ok {
			continue
		}
		tips = append(tips, syncflow.RemoteTip{
			DeviceID:    ref.DeviceID,
			RecordCount: ref.Metadata.RecordCount,
			HeadDigest:  ref.Metadata.HeadDigest,
		})
	}
	if len(tips) != len(selected) {
		return errors.New("resume: selected restore version has incomplete device metadata")
	}
	return syncflow.SaveObservedTips(ctx, pullTipStore, tips)
}

func safeResumePlanError(err error) error {
	switch {
	case errors.Is(err, syncflow.ErrRestoreCompatibility):
		return fmt.Errorf("resume: %w", err)
	case errors.Is(err, syncflow.ErrForkSelectionRequired), errors.Is(err, syncflow.ErrInvalidVersionSelection):
		return fmt.Errorf("resume: %w", err)
	default:
		return errors.New("resume: remote session could not be read safely")
	}
}

func safeResumeApplyError(err error) error {
	switch {
	case errors.Is(err, syncflow.ErrWorkspaceDiverged), errors.Is(err, syncflow.ErrWorkspaceFingerprintRequired), errors.Is(err, syncflow.ErrRestoreCompatibility), errors.Is(err, adapter.ErrSessionExists):
		return fmt.Errorf("resume: %w", err)
	default:
		return errors.New("resume: session was not written because a safety check failed")
	}
}

func writeResumeJSON(w io.Writer, report resumeReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeResumeText(w io.Writer, report resumeReport) error {
	if _, err := fmt.Fprintf(w, "resumed: %s\n", safeListText(report.Title)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "workspace: %s", report.Workspace); err != nil {
		return err
	}
	if report.Differences != 0 {
		if _, err := fmt.Fprintf(w, " (%d file differences)", report.Differences); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if report.Replaced {
		_, err := fmt.Fprintln(w, "existing session: replaced")
		return err
	}
	return nil
}

type resumePrompter struct {
	reader *bufio.Reader
	output io.Writer
}

func (p *resumePrompter) secret(prompt string) (string, error) {
	value, err := p.line(prompt)
	if err != nil {
		return "", fmt.Errorf("resume: read passphrase: %w", err)
	}
	if value == "" {
		return "", errors.New("resume: passphrase cannot be empty")
	}
	return value, nil
}

func (p *resumePrompter) line(prompt string) (string, error) {
	if _, err := fmt.Fprint(p.output, prompt); err != nil {
		return "", err
	}
	value, err := p.reader.ReadString('\n')
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.EOF
	}
	return value, nil
}
