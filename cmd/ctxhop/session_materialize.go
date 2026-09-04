package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/environment"
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const (
	sessionActionSwitch       = "switch"
	materializeCommandTimeout = 10 * time.Minute
	materializeContextCausal  = string(syncflow.MaterializeContextCausalHead)
	materializeContextAll     = string(syncflow.MaterializeContextAllHeads)
	materializeContextAgent   = string(syncflow.MaterializeContextAgentOnly)
)

type materializeOptions struct {
	json    bool
	preview bool
	// apply is derived from preview. There is no public apply flag: a switch
	// executes by default, while --preview is the explicit read-only mode.
	apply            bool
	applyEnvironment bool
	launch           bool
	targetAgent      string
	contextPolicy    string
	heads            []string
	sourceAgent      string
	allowUnsupported bool
	sessionID        string
}

type materializeHeadFlag []string

func (flag *materializeHeadFlag) String() string {
	if flag == nil {
		return ""
	}
	return strings.Join(*flag, ",")
}

func (flag *materializeHeadFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("head cannot be empty")
	}
	if strings.ContainsRune(value, 0) {
		return errors.New("head contains an invalid character")
	}
	*flag = append(*flag, value)
	return nil
}

// materializePreviewReport is the command-safe projection of a
// syncflow.MaterializePreview. EncodedRecords are intentionally not exposed;
// the CLI reports coverage and conversion facts, never source or target
// transcript bodies.
type materializePreviewReport struct {
	Preview              bool                                `json:"preview"`
	TransactionID        string                              `json:"transactionId,omitempty"`
	AlreadyApplied       bool                                `json:"alreadyApplied,omitempty"`
	Scope                string                              `json:"scope"`
	HubID                string                              `json:"hubId"`
	ProjectID            string                              `json:"projectId"`
	SessionID            string                              `json:"sessionId"`
	ContextPolicy        string                              `json:"contextPolicy"`
	SourceAgent          string                              `json:"sourceAgent,omitempty"`
	SelectedHeads        []string                            `json:"selectedHeads"`
	Coverage             sessionhub.Coverage                 `json:"coverage"`
	Sources              []syncflow.MaterializeSourceSummary `json:"sources"`
	TargetAgent          string                              `json:"targetAgent"`
	TargetNativeID       string                              `json:"targetNativeId"`
	TargetAdapterVersion string                              `json:"targetAdapterVersion"`
	SourceSnapshotDigest string                              `json:"sourceSnapshotDigest,omitempty"`
	SelectedRecordCount  uint64                              `json:"selectedRecordCount"`
	ContextItems         int                                 `json:"contextItems"`
	Stats                adapter.MaterializeStats            `json:"stats"`
	WriteStatus          string                              `json:"writeStatus"`
	EnvironmentStatus    string                              `json:"environmentStatus,omitempty"`
	LaunchStatus         string                              `json:"launchStatus,omitempty"`
}

func runSessionMaterializeWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseMaterializeOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("session switch: input is required")
	}
	if output == nil {
		return errors.New("session switch: output is required")
	}
	if prompt == nil {
		return errors.New("session switch: prompt output is required")
	}
	if options.allowUnsupported && !options.preview {
		return errors.New("session switch: --allow-unsupported is only available for a diagnostic --preview")
	}
	if options.applyEnvironment && !options.apply {
		return errors.New("session switch: --with-environment requires an executing switch")
	}
	if options.launch && !options.apply {
		return errors.New("session switch: --launch requires an executing switch")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), materializeCommandTimeout)
	defer cancel()

	execution, err := collectMaterializeExecutionWithPrompt(ctx, c, configDir, ".", options, input, prompt)
	if err != nil {
		return err
	}
	if options.apply {
		return applyMaterializeExecution(ctx, execution, output, options.json)
	}
	report := execution.Report
	if options.json {
		return writeMaterializePreviewJSON(output, report)
	}
	return writeMaterializePreviewText(output, report)
}

func parseMaterializeOptions(args []string) (materializeOptions, error) {
	flags := flag.NewFlagSet("session switch", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options materializeOptions
	var heads materializeHeadFlag
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	flags.BoolVar(&options.preview, "preview", false, "show the switch plan without changing local files")
	flags.BoolVar(&options.applyEnvironment, "with-environment", false, "include portable environment components in the switch")
	flags.BoolVar(&options.launch, "launch", false, "launch the target Agent after the switch")
	flags.StringVar(&options.targetAgent, "to", "", "target Agent, for example codex or claude-code")
	flags.StringVar(&options.contextPolicy, "context", materializeContextCausal, "context policy: causal-head, all-heads, or agent-only")
	flags.Var(&heads, "head", "Contribution head; repeat for an explicit head set")
	flags.StringVar(&options.sourceAgent, "source", "", "source Agent for the agent-only policy")
	flags.BoolVar(&options.allowUnsupported, "allow-unsupported", false, "allow a diagnostic preview when source records are omitted")
	if err := flags.Parse(normalizeMaterializeArgs(args)); err != nil {
		return materializeOptions{}, fmt.Errorf("session switch: %w", err)
	}
	options.apply = !options.preview
	if flags.NArg() != 1 {
		return materializeOptions{}, errors.New("session switch: expected exactly one logical Session ID")
	}
	options.sessionID = flags.Arg(0)
	if strings.TrimSpace(options.sessionID) == "" || strings.ContainsRune(options.sessionID, 0) {
		return materializeOptions{}, errors.New("session switch: Session ID is empty or invalid")
	}
	options.targetAgent = strings.TrimSpace(options.targetAgent)
	if options.targetAgent == "" {
		return materializeOptions{}, errors.New("session switch: --to is required")
	}
	options.contextPolicy = strings.TrimSpace(options.contextPolicy)
	if options.contextPolicy == "" {
		return materializeOptions{}, errors.New("session switch: --context cannot be empty")
	}
	options.sourceAgent = strings.TrimSpace(options.sourceAgent)
	options.heads = append([]string(nil), heads...)
	if options.sourceAgent != "" && options.contextPolicy != materializeContextAgent {
		return materializeOptions{}, errors.New("session switch: --source is only valid with --context agent-only")
	}
	if len(options.heads) != 0 && options.contextPolicy != materializeContextCausal {
		return materializeOptions{}, errors.New("session switch: --head is only valid with --context causal-head")
	}
	if options.contextPolicy == materializeContextAgent && options.sourceAgent == "" {
		return materializeOptions{}, errors.New("session switch: --source is required with --context agent-only")
	}
	if options.contextPolicy != materializeContextCausal && options.contextPolicy != materializeContextAll && options.contextPolicy != materializeContextAgent {
		return materializeOptions{}, fmt.Errorf("session switch: unsupported --context %q", options.contextPolicy)
	}
	if options.allowUnsupported && !options.preview {
		return materializeOptions{}, errors.New("session switch: --allow-unsupported is only available for a diagnostic --preview")
	}
	if options.applyEnvironment && !options.apply {
		return materializeOptions{}, errors.New("session switch: --with-environment requires an executing switch")
	}
	if options.launch && !options.apply {
		return materializeOptions{}, errors.New("session switch: --launch requires an executing switch")
	}
	return options, nil
}

// normalizeMaterializeArgs accepts the documented positional-first spelling:
// `session switch <session-id> --to codex ...`. The standard flag parser
// otherwise stops at the Session ID and silently treats the remaining flags as
// arguments.
func normalizeMaterializeArgs(args []string) []string {
	valueFlags := map[string]struct{}{
		"-to": {}, "--to": {},
		"-context": {}, "--context": {},
		"-head": {}, "--head": {},
		"-source": {}, "--source": {},
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

func collectMaterializePreviewWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, options materializeOptions, input io.Reader, prompt io.Writer) (materializePreviewReport, error) {
	execution, err := collectMaterializeExecutionWithPrompt(ctx, c, configDir, projectDir, options, input, prompt)
	if err != nil {
		return materializePreviewReport{}, err
	}
	return execution.Report, nil
}

type materializeExecution struct {
	Report              materializePreviewReport
	Preview             syncflow.MaterializePreview
	Target              adapter.AgentSessions
	TargetCapability    adapter.MaterializeCapability
	EnvironmentContents []environment.ComponentContent
	ApplyEnvironment    bool
	Launch              bool
	ConfigDir           string
	ProjectRoot         string
	IdentityKind        sessionhub.ProjectIdentityKind
	IdentityValue       string
	IdentifierKey       []byte
	HubName             string
	LocalDeviceID       string
	HubID               string
	ProjectID           string
	SessionID           string
	TransactionID       string
}

func collectMaterializeExecutionWithPrompt(ctx context.Context, c *config.Config, configDir, projectDir string, options materializeOptions, input io.Reader, prompt io.Writer) (materializeExecution, error) {
	if c == nil {
		return materializeExecution{}, errors.New("session switch: configuration is unavailable")
	}
	if ctx == nil {
		return materializeExecution{}, errors.New("session switch: context is required")
	}
	if err := ctx.Err(); err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: %w", err)
	}
	if err := devicePullError("session switch", c); err != nil {
		return materializeExecution{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: local device identity is invalid: %w", err)
	}

	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return materializeExecution{}, fmt.Errorf("session switch: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return materializeExecution{}, errors.New("session switch: project is excluded from synchronization")
	case projectModePushOnly:
		return materializeExecution{}, errors.New("session switch: project is configured as push-only; remote sessions are unavailable")
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: load local sync material: %w", err)
	}
	hubScope, projectScope, projectID, err := sessionHubAndProjectForConfig(secrets.IdentifierKey, current, c)
	if err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: prepare Session Hub identity: %w", err)
	}
	sessionLayout, err := syncer.NewSessionHubLayout(hubScope.ID, projectID, options.sessionID)
	if err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: invalid logical Session identity: %w", err)
	}
	transactionID := ""
	targetNativeID := ""
	targetCreatedAt := time.Now().UTC()
	if options.apply {
		transactionID = materializeRequestID(hubScope.ID, projectID, options)
		targetNativeID = materializeNativeSessionID(transactionID)
		targetCreatedAt = materializeStableTargetTime(transactionID)
	}

	target, targetCapability, err := materializeTarget(ctx, options.targetAgent)
	if err != nil {
		return materializeExecution{}, err
	}
	sourceCapabilities, err := materializeSourceCapabilities()
	if err != nil {
		return materializeExecution{}, err
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "session switch")
	if err != nil {
		return materializeExecution{}, err
	}
	defer access.close()

	preview, err := syncflow.FetchMaterializePreview(ctx, syncflow.RemoteMaterializePreviewRequest{
		Store:              access.Store,
		Identities:         access.Identities,
		IdentifierKey:      append([]byte(nil), secrets.IdentifierKey...),
		Layout:             sessionLayout,
		ContextPolicy:      syncflow.MaterializeContextPolicy(options.contextPolicy),
		SourceAgent:        options.sourceAgent,
		Heads:              append([]string(nil), options.heads...),
		IncludeEnvironment: options.applyEnvironment,
		MaterializePreviewOptions: syncflow.MaterializePreviewOptions{
			SourceCapabilities: sourceCapabilities,
			TargetAgent:        options.targetAgent,
			TargetCapability:   targetCapability,
			Target: adapter.MaterializeTarget{
				NativeID:  targetNativeID,
				PathSpace: adapter.PathSpace{ProjectRoot: current.Root, AgentHome: target.Installation.DataDir},
				CreatedAt: targetCreatedAt,
			},
			// Unsupported source records are a preview-only diagnostic mode. Keep
			// the apply path fail-closed even for callers that bypass parsing.
			AllowUnsupported: options.allowUnsupported && options.preview,
		},
	})
	if err != nil {
		return materializeExecution{}, fmt.Errorf("session switch: %w", err)
	}
	report := materializePreviewReport{
		Preview:              !options.apply,
		TransactionID:        transactionID,
		Scope:                "project",
		HubID:                hubScope.ID,
		ProjectID:            projectScope.ID,
		SessionID:            options.sessionID,
		ContextPolicy:        options.contextPolicy,
		SourceAgent:          options.sourceAgent,
		SelectedHeads:        append([]string(nil), preview.SelectedHeads...),
		Coverage:             preview.Coverage,
		Sources:              append([]syncflow.MaterializeSourceSummary(nil), preview.Sources...),
		TargetAgent:          preview.TargetAgent,
		TargetNativeID:       preview.TargetNativeID,
		TargetAdapterVersion: preview.TargetAdapterVersion,
		SourceSnapshotDigest: preview.SourceSnapshotDigest,
		SelectedRecordCount:  preview.SelectedRecordCount,
		ContextItems:         preview.ContextItems,
		Stats:                preview.Stats,
		WriteStatus:          "new-target-session-not-written",
		EnvironmentStatus:    materializeEnvironmentPreviewStatus(options.applyEnvironment, len(preview.EnvironmentComponents)),
	}
	return materializeExecution{
		Report:              report,
		Preview:             preview,
		Target:              target,
		TargetCapability:    targetCapability,
		EnvironmentContents: cloneMaterializeEnvironmentContents(preview.EnvironmentContents),
		ApplyEnvironment:    options.applyEnvironment,
		Launch:              options.launch,
		ConfigDir:           configDir,
		ProjectRoot:         current.Root,
		IdentityKind:        projectIdentityKind(current.Identity.Kind),
		IdentityValue:       current.Identity.Value,
		IdentifierKey:       append([]byte(nil), secrets.IdentifierKey...),
		HubName:             hubScope.Name,
		LocalDeviceID:       c.Device.ID,
		HubID:               hubScope.ID,
		ProjectID:           projectScope.ID,
		SessionID:           options.sessionID,
		TransactionID:       transactionID,
	}, nil
}

func materializeTarget(ctx context.Context, name string) (adapter.AgentSessions, adapter.MaterializeCapability, error) {
	target, err := adapter.FindInstalled(ctx, name)
	if errors.Is(err, adapter.ErrNotInstalled) {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session switch: target Agent %s is not installed", resumeAgentLabel(name))
	}
	if err != nil {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session switch: inspect target Agent %s: %w", resumeAgentLabel(name), err)
	}
	capability, err := adapter.MaterializeFor(target.Layout)
	if err != nil {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session switch: target Agent %q has no switching capability: %w", name, err)
	}
	return target, capability, nil
}

func materializeSourceCapabilities() (map[string]adapter.MaterializeCapability, error) {
	layouts, err := adapter.DefaultLayouts()
	if err != nil {
		return nil, fmt.Errorf("session switch: discover built-in Agent capabilities: %w", err)
	}
	capabilities := make(map[string]adapter.MaterializeCapability, len(layouts))
	for _, layout := range layouts {
		capability, err := adapter.MaterializeFor(layout)
		if err != nil {
			if errors.Is(err, adapter.ErrMaterializeUnsupportedLayout) {
				continue
			}
			return nil, fmt.Errorf("session switch: inspect %s switching capability: %w", layout.Name(), err)
		}
		capabilities[layout.Name()] = capability
	}
	return capabilities, nil
}

func materializeEnvironmentPreviewStatus(requested bool, count int) string {
	if !requested {
		return ""
	}
	if count == 0 {
		return "no safe filtered components found"
	}
	return fmt.Sprintf("%d filtered component(s) available", count)
}

func writeMaterializePreviewJSON(w io.Writer, report materializePreviewReport) error {
	if w == nil {
		return errors.New("session switch: output is required")
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeMaterializePreviewText(w io.Writer, report materializePreviewReport) error {
	if w == nil {
		return errors.New("session switch: output is required")
	}
	if _, err := fmt.Fprintf(w, "scope: %s\n", safeListText(report.Scope)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "hub: %s\n", safeListText(report.HubID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "project: %s\n", safeListText(report.ProjectID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "session: %s\n", safeListText(report.SessionID)); err != nil {
		return err
	}
	if report.TransactionID != "" {
		if _, err := fmt.Fprintf(w, "transaction: %s\n", safeListText(report.TransactionID)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "context: %s\n", safeListText(report.ContextPolicy)); err != nil {
		return err
	}
	if report.SourceAgent != "" {
		if _, err := fmt.Fprintf(w, "source-agent: %s\n", safeListText(report.SourceAgent)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "selected-heads: %d\n", len(report.SelectedHeads)); err != nil {
		return err
	}
	for _, head := range report.SelectedHeads {
		if _, err := fmt.Fprintf(w, "- head=%s\n", safeListText(head)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "selected-contributions: %d\n", len(report.Coverage.SelectedIDs)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "omitted-contributions: %d\n", len(report.Coverage.OmittedIDs)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "records: %d\ncontext-items: %d\n", report.SelectedRecordCount, report.ContextItems); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "sources:"); err != nil {
		return err
	}
	for _, source := range report.Sources {
		if _, err := fmt.Fprintf(w, "- agent=%s contribution=%s replica=%s records=%d items=%d unsupported=%d filtered=%d\n",
			safeListText(source.SourceAgent), safeListText(source.ContributionID), safeListText(source.ReplicaID), source.RecordCount, source.ContextItems, source.Unsupported, source.Filtered); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "conversion: converted=%d summarized=%d unsupported=%d filtered=%d\n", report.Stats.Converted, report.Stats.Summarized, report.Stats.Unsupported, report.Stats.Filtered); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "target: agent=%s native=%s adapter=%s\n", safeListText(report.TargetAgent), safeListText(report.TargetNativeID), safeListText(report.TargetAdapterVersion)); err != nil {
		return err
	}
	if report.SourceSnapshotDigest != "" {
		if _, err := fmt.Fprintf(w, "source-snapshot: %s\n", safeListText(report.SourceSnapshotDigest)); err != nil {
			return err
		}
	}
	if report.EnvironmentStatus != "" {
		if _, err := fmt.Fprintf(w, "environment: %s\n", safeListText(report.EnvironmentStatus)); err != nil {
			return err
		}
	}
	if report.LaunchStatus != "" {
		if _, err := fmt.Fprintf(w, "launch: %s\n", safeListText(report.LaunchStatus)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "write-status: %s\n", safeListText(report.WriteStatus)); err != nil {
		return err
	}
	if report.Preview {
		_, err := fmt.Fprintln(w, "source sessions, Agent files, LocalBinding and Remote objects: unchanged")
		return err
	}
	_, err := fmt.Fprintln(w, "source sessions and Remote objects: unchanged; target Agent session and local binding: committed")
	return err
}
