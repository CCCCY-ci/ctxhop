package main

import (
	"bufio"
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

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/crypto"
	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/project"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const resumeTimeout = 10 * time.Minute

type resumeOptions struct {
	json               bool
	preview            bool
	workspace          bool
	allowLimited       bool
	allowDivergent     bool
	noWorkspaceContext bool
	replaceExisting    bool
	version            int
	session            string
	agent              string
	replica            string
}

type resumeReport struct {
	Preview         bool                      `json:"preview,omitempty"`
	Session         string                    `json:"session"`
	LogicalSession  string                    `json:"logicalSession,omitempty"`
	Agent           string                    `json:"agent,omitempty"`
	ReplicaID       string                    `json:"replicaId,omitempty"`
	LocalState      string                    `json:"localState,omitempty"`
	RemoteRecords   uint64                    `json:"remoteRecordCount,omitempty"`
	LocalRecords    uint64                    `json:"localRecordCount,omitempty"`
	AppendRecords   uint64                    `json:"appendRecordCount,omitempty"`
	Title           string                    `json:"title"`
	Workspace       string                    `json:"workspace"`
	Differences     int                       `json:"differences"`
	Replaced        bool                      `json:"replaced"`
	Merged          bool                      `json:"merged"`
	ContextInjected bool                      `json:"contextInjected"`
	Sources         []string                  `json:"sources"`
	OmittedAgents   []string                  `json:"omittedAgents,omitempty"`
	OmittedReplicas []string                  `json:"omittedReplicas,omitempty"`
	Environment     *environmentPreviewReport `json:"environment,omitempty"`
	WorkspaceState  *projectStateReport       `json:"workspaceState,omitempty"`
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
	flags.BoolVar(&options.preview, "preview", false, "show the restore without changing local files")
	flags.BoolVar(&options.workspace, "workspace", false, "also restore project files and Git state")
	flags.BoolVar(&options.allowLimited, "allow-limited", false, "allow restore when structural compatibility is limited")
	flags.BoolVar(&options.allowDivergent, "allow-divergent", false, "allow restore despite a divergent workspace")
	flags.BoolVar(&options.noWorkspaceContext, "no-workspace-context", false, "do not inject workspace differences into the restored session")
	flags.BoolVar(&options.replaceExisting, "replace-existing", false, "replace an existing local session")
	flags.IntVar(&options.version, "version", -1, "select a zero-based remote fork version")
	flags.StringVar(&options.agent, "agent", "", "select the source Agent for a Session Hub resume")
	flags.StringVar(&options.replica, "replica", "", "select one NativeReplica for a Session Hub resume")
	if err := flags.Parse(normalizeResumeArgs(args)); err != nil {
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
	agentProvided, replicaProvided := false, false
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "agent":
			agentProvided = true
		case "replica":
			replicaProvided = true
		}
	})
	options.agent = strings.TrimSpace(options.agent)
	options.replica = strings.TrimSpace(options.replica)
	if agentProvided && options.agent == "" {
		return resumeOptions{}, errors.New("resume: --agent cannot be empty")
	}
	if replicaProvided && options.replica == "" {
		return resumeOptions{}, errors.New("resume: --replica cannot be empty")
	}
	return options, nil
}

// normalizeResumeArgs lets the compatibility alias accept both historical
// `resume --preview <session>` and the Session Hub spelling shown in the v2
// spec, `session resume <session> --agent ...`. The standard flag package
// stops at the first positional argument, so known value flags are kept with
// their values and the single positional session is moved to the end.
func normalizeResumeArgs(args []string) []string {
	valueFlags := map[string]struct{}{
		"-version": {}, "--version": {},
		"-agent": {}, "--agent": {},
		"-replica": {}, "--replica": {},
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	parsingFlags := true
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if parsingFlags && arg == "--" {
			parsingFlags = false
			continue
		}
		if parsingFlags && strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			name := arg
			if equal := strings.IndexByte(name, '='); equal >= 0 {
				name = name[:equal]
			}
			if _, takesValue := valueFlags[name]; takesValue && !strings.ContainsRune(arg, '=') && index+1 < len(args) {
				index++
				flags = append(flags, args[index])
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
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
		return resumeReport{}, errors.New("resume: encryption identity is not configured; run 'ctxhop init'")
	}

	current, err := resolveCurrentProject(ctx, c, projectDir)
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
	prompter := &resumePrompter{reader: bufio.NewReader(input), output: output}
	access, err := openDomainForRead(ctx, c, configDir, prompter.reader, prompter.output, "resume")
	if err != nil {
		return resumeReport{}, err
	}
	defer access.close()
	store := access.Store
	identities := access.Identities
	groups, err := syncer.FetchProjectMetadataWithIdentitiesAndDevices(ctx, store, projectID, identities, access.allowedDevices())
	nativeSelector := options.agent != "" || options.replica != ""
	if err != nil && !errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return resumeReport{}, fmt.Errorf("resume: read encrypted session metadata: %w", err)
	}

	if !nativeSelector && errors.Is(err, syncer.ErrNoRemoteMetadata) {
		return resumeReport{}, errors.New("resume: no encrypted sessions are available for this project")
	}

	var selection resumeSelection
	if nativeSelector {
		selection, err = selectNativeResume(ctx, current, secrets.IdentifierKey, groups, options, prompter, access)
	} else {
		var candidate resumeCandidate
		candidate, err = chooseResumeCandidate(groups, projectID, secrets.IdentifierKey, options.session, prompter)
		if err == nil {
			var selectedAgent adapter.AgentSessions
			selectedAgent, err = selectResumeAgent(ctx, current.Root, candidate.Summary)
			if err == nil {
				installation := selectedAgent.Installation
				space := adapter.PathSpace{ProjectRoot: current.Root, AgentHome: installation.DataDir}
				restoreOptions := syncflow.RestoreOptions{AllowLimited: options.allowLimited}
				if options.version >= 0 {
					restoreOptions.VersionIndex = &options.version
				}
				var plan syncflow.RestorePlan
				plan, err = syncflow.FetchRestorePlanWithIdentitiesAndDevices(ctx, store, projectID, candidate.Group.SessionID, identities, access.allowedDevices(), space, installation, restoreOptions)
				if err == nil {
					selection = resumeSelection{Candidate: candidate, Plan: plan, Agent: selectedAgent}
				} else {
					err = safeResumePlanError(err)
				}
			}
		}
	}
	if err != nil {
		return resumeReport{}, err
	}

	candidate := selection.Candidate
	agent := selection.Agent
	layout := agent.Layout
	installation := agent.Installation
	plan := selection.Plan
	fingerprint := resumeFingerprint(candidate.Group, plan)
	if fingerprint == nil {
		return resumeReport{}, errors.New("resume: selected session has no matching workspace fingerprint; push it again from the source device")
	}
	if selection.LogicalSession != "" {
		localState, stateErr := inspectNativeResumeLocalState(configDir, current.Root, selection)
		if stateErr != nil {
			return resumeReport{}, stateErr
		}
		selection.LocalState = localState
		if !options.preview && !options.replaceExisting {
			switch localState.State {
			case "ahead", "diverged", "incompatible":
				return resumeReport{}, fmt.Errorf("resume: local NativeSession is %s; use --replace-existing only after reviewing the selected Replica", localState.State)
			}
		}
	}
	localState, err := newEnvironmentContext(c.Device.ID, secrets.IdentifierKey, projectID, current.Identity.Value, configDir, current.Root, groups, access)
	if err != nil {
		return resumeReport{}, fmt.Errorf("resume: prepare environment context: %w", err)
	}
	environmentSession := findEnvironmentSession(localState.List.Sessions, candidate.Group.SessionID)
	if environmentSession == nil {
		return resumeReport{}, errors.New("resume: selected session has no environment metadata")
	}
	environmentReport := buildEnvironmentPreviewReport(ctx, localState, environmentSession)
	if !options.preview && environmentReportHasConflict(environmentReport) {
		return resumeReport{}, errors.New("resume: environment conflicts require manual resolution")
	}

	var workspaceInspection *projectStateInspection
	if options.workspace {
		inspection, inspectErr := inspectProjectState(ctx, localState, environmentSession)
		if inspectErr != nil {
			return resumeReport{}, fmt.Errorf("resume: inspect workspace: %w", inspectErr)
		}
		workspaceInspection = &inspection
	}

	baseReport := resumeReport{
		Preview:         options.preview,
		Session:         candidate.Summary.NativeID,
		LogicalSession:  selection.LogicalSession,
		Agent:           selection.AgentName,
		ReplicaID:       selection.ReplicaID,
		LocalState:      selection.LocalState.State,
		RemoteRecords:   selection.LocalState.RemoteRecords,
		LocalRecords:    selection.LocalState.LocalRecords,
		AppendRecords:   selection.LocalState.AppendRecords,
		Title:           safeListText(candidate.Summary.Title),
		Environment:     &environmentReport,
		OmittedAgents:   append([]string(nil), selection.OmittedAgents...),
		OmittedReplicas: append([]string(nil), selection.OmittedReplicas...),
	}
	if workspaceInspection != nil {
		baseReport.WorkspaceState = &workspaceInspection.Report
	}

	if options.preview {
		workspaceReport, compareErr := project.Compare(ctx, current.Root, *fingerprint)
		if compareErr != nil {
			return resumeReport{}, fmt.Errorf("resume: preview workspace check failed: %w", compareErr)
		}
		baseReport.Workspace = workspaceReport.Verdict.String()
		baseReport.Differences = len(workspaceReport.Files)
		return baseReport, nil
	}

	if workspaceInspection != nil {
		if err := applyProjectState(ctx, localState, environmentSession, workspaceInspection); err != nil {
			return resumeReport{}, fmt.Errorf("resume: restore workspace: %w", err)
		}
	}
	if err := applyEnvironmentComponents(ctx, localState, environmentSession, &environmentReport); err != nil {
		return resumeReport{}, fmt.Errorf("resume: restore environment: %w", err)
	}

	result, err := syncflow.ApplyRestore(ctx, layout, current.Root, candidate.Summary.NativeID, plan, syncflow.RestoreApplyOptions{
		Fingerprint:            fingerprint,
		AllowLimited:           options.allowLimited,
		AllowDivergent:         options.allowDivergent,
		InjectWorkspaceContext: !options.noWorkspaceContext,
		Agent:                  layout.Name(),
		AgentHome:              installation.DataDir,
		ReplaceExisting:        options.replaceExisting,
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
	if selection.LogicalSession != "" {
		if err := saveNativeResumeState(configDir, secrets.IdentifierKey, current, c.Device.ID, selection); err != nil {
			return resumeReport{}, fmt.Errorf("resume: restore completed but Session Hub binding could not be saved: %w", err)
		}
	}
	sources := append([]string(nil), plan.Devices...)
	sort.Strings(sources)
	baseReport.Environment = &environmentReport
	baseReport.Workspace = result.Workspace.Verdict.String()
	baseReport.Differences = len(result.Workspace.Files)
	baseReport.Replaced = result.Replaced
	baseReport.Merged = result.Merged
	baseReport.ContextInjected = result.ContextInjected
	baseReport.Sources = sources
	if workspaceInspection != nil {
		baseReport.WorkspaceState = &workspaceInspection.Report
	}
	return baseReport, nil
}

func environmentReportHasConflict(report environmentPreviewReport) bool {
	for _, change := range report.Changes {
		if change.State == environment.ComponentStateConflict {
			return true
		}
	}
	return false
}

func selectResumeAgent(ctx context.Context, projectRoot string, summary syncflow.SessionSummary) (adapter.AgentSessions, error) {
	if summary.Agent != "" {
		agent, err := adapter.FindInstalled(ctx, summary.Agent)
		if errors.Is(err, adapter.ErrNotInstalled) {
			return adapter.AgentSessions{}, fmt.Errorf("resume: %s is not installed", resumeAgentLabel(summary.Agent))
		}
		if err != nil {
			return adapter.AgentSessions{}, fmt.Errorf("resume: inspect %s: %w", resumeAgentLabel(summary.Agent), err)
		}
		return agent, nil
	}

	agents, err := adapter.DiscoverInstalled(ctx, projectRoot)
	if err != nil {
		return adapter.AgentSessions{}, fmt.Errorf("resume: discover local agents: %w", err)
	}
	for _, agent := range agents {
		for _, ref := range agent.Sessions {
			if ref.NativeID == summary.NativeID {
				return agent, nil
			}
		}
	}
	for _, agent := range agents {
		if agent.Layout.Name() == "claude-code" {
			return agent, nil
		}
	}
	if len(agents) == 1 {
		return agents[0], nil
	}
	if len(agents) == 0 {
		return adapter.AgentSessions{}, errors.New("resume: no supported coding agent is installed; install Claude Code or Codex CLI")
	}
	return adapter.AgentSessions{}, errors.New("resume: remote session does not identify a supported local agent")
}

func resumeAgentLabel(name string) string {
	switch name {
	case "codex":
		return "Codex"
	case "claude-code":
		return "Claude Code"
	default:
		return "the required coding agent"
	}
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
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("resume: timed out while downloading the remote session; retry on a stable connection")
	case errors.Is(err, context.Canceled):
		return errors.New("resume: remote session download was cancelled")
	case errors.Is(err, syncer.ErrIncompleteRemoteSession), errors.Is(err, syncer.ErrReplicaIncomplete):
		return errors.New("resume: remote session is incomplete; retry later")
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
	case errors.Is(err, syncflow.ErrWorkspaceDiverged), errors.Is(err, syncflow.ErrWorkspaceFingerprintRequired), errors.Is(err, syncflow.ErrWorkspaceContextInjection), errors.Is(err, syncflow.ErrRestoreCompatibility), errors.Is(err, syncflow.ErrExistingSessionConflict), errors.Is(err, adapter.ErrSessionExists):
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
	label := "resumed"
	if report.Preview {
		label = "preview"
	}
	if _, err := fmt.Fprintf(w, "%s: %s\n", label, safeListText(report.Title)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.Session)); err != nil {
		return err
	}
	if report.LogicalSession != "" {
		if _, err := fmt.Fprintf(w, "logical session: %s\n", safeListText(report.LogicalSession)); err != nil {
			return err
		}
	}
	if report.Agent != "" {
		if _, err := fmt.Fprintf(w, "agent: %s\n", safeListText(report.Agent)); err != nil {
			return err
		}
	}
	if report.ReplicaID != "" {
		if _, err := fmt.Fprintf(w, "replica: %s\n", safeListText(report.ReplicaID)); err != nil {
			return err
		}
	}
	if report.LocalState != "" {
		if _, err := fmt.Fprintf(w, "local state: %s", safeListText(report.LocalState)); err != nil {
			return err
		}
		if report.LocalRecords != 0 || report.RemoteRecords != 0 {
			if _, err := fmt.Fprintf(w, " (%d/%d records", report.LocalRecords, report.RemoteRecords); err != nil {
				return err
			}
			if report.AppendRecords != 0 {
				if _, err := fmt.Fprintf(w, ", append=%d", report.AppendRecords); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprint(w, ")"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if len(report.OmittedAgents) != 0 {
		if _, err := fmt.Fprintf(w, "omitted agents: %s\n", safeListText(strings.Join(report.OmittedAgents, ","))); err != nil {
			return err
		}
	}
	if len(report.OmittedReplicas) != 0 {
		if _, err := fmt.Fprintf(w, "omitted replicas: %s\n", safeListText(strings.Join(report.OmittedReplicas, ","))); err != nil {
			return err
		}
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
	if report.Environment != nil {
		if _, err := fmt.Fprintf(w, "environment: %s", safeListText(report.Environment.Status)); err != nil {
			return err
		}
		if changes := len(report.Environment.Changes); changes != 0 {
			if _, err := fmt.Fprintf(w, " (%d component changes)", changes); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if report.WorkspaceState != nil {
		if _, err := fmt.Fprintf(w, "workspace state: %s\n", safeListText(report.WorkspaceState.Status)); err != nil {
			return err
		}
	}
	if report.Preview {
		return nil
	}
	if report.Replaced {
		_, err := fmt.Fprintln(w, "existing session: replaced")
		return err
	}
	if report.Merged {
		if _, err := fmt.Fprintln(w, "existing session: extended"); err != nil {
			return err
		}
	}
	if report.ContextInjected {
		_, err := fmt.Fprintln(w, "workspace context: injected")
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
