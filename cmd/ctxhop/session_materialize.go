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
	"github.com/CCCCY-ci/ctxhop/internal/sessionhub"
	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"github.com/CCCCY-ci/ctxhop/internal/syncflow"
)

const (
	sessionActionMaterialize  = "materialize"
	materializeCommandTimeout = 10 * time.Minute
	materializeContextCausal  = string(syncflow.MaterializeContextCausalHead)
	materializeContextAll     = string(syncflow.MaterializeContextAllHeads)
	materializeContextAgent   = string(syncflow.MaterializeContextAgentOnly)
)

type materializeOptions struct {
	json             bool
	preview          bool
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
	SelectedRecordCount  uint64                              `json:"selectedRecordCount"`
	ContextItems         int                                 `json:"contextItems"`
	Stats                adapter.MaterializeStats            `json:"stats"`
	WriteStatus          string                              `json:"writeStatus"`
}

func runSessionMaterializeWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseMaterializeOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("session materialize: input is required")
	}
	if output == nil {
		return errors.New("session materialize: output is required")
	}
	if prompt == nil {
		return errors.New("session materialize: prompt output is required")
	}
	if !options.preview {
		return errors.New("session materialize: apply is not implemented yet; pass --preview to run the read-only Phase 5 operation")
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

	report, err := collectMaterializePreviewWithPrompt(ctx, c, configDir, ".", options, input, prompt)
	if err != nil {
		return err
	}
	if options.json {
		return writeMaterializePreviewJSON(output, report)
	}
	return writeMaterializePreviewText(output, report)
}

func parseMaterializeOptions(args []string) (materializeOptions, error) {
	flags := flag.NewFlagSet("session materialize", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options materializeOptions
	var heads materializeHeadFlag
	flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
	flags.BoolVar(&options.preview, "preview", false, "show the materialization without changing local files")
	flags.StringVar(&options.targetAgent, "to", "", "target Agent, for example codex or claude-code")
	flags.StringVar(&options.contextPolicy, "context", materializeContextCausal, "context policy: causal-head, all-heads, or agent-only")
	flags.Var(&heads, "head", "Contribution head; repeat for an explicit head set")
	flags.StringVar(&options.sourceAgent, "source", "", "source Agent for the agent-only policy")
	flags.BoolVar(&options.allowUnsupported, "allow-unsupported", false, "allow a diagnostic preview when source records are omitted")
	if err := flags.Parse(normalizeMaterializeArgs(args)); err != nil {
		return materializeOptions{}, fmt.Errorf("session materialize: %w", err)
	}
	if flags.NArg() != 1 {
		return materializeOptions{}, errors.New("session materialize: expected exactly one logical Session ID")
	}
	options.sessionID = flags.Arg(0)
	if strings.TrimSpace(options.sessionID) == "" || strings.ContainsRune(options.sessionID, 0) {
		return materializeOptions{}, errors.New("session materialize: Session ID is empty or invalid")
	}
	options.targetAgent = strings.TrimSpace(options.targetAgent)
	if options.targetAgent == "" {
		return materializeOptions{}, errors.New("session materialize: --to is required")
	}
	options.contextPolicy = strings.TrimSpace(options.contextPolicy)
	if options.contextPolicy == "" {
		return materializeOptions{}, errors.New("session materialize: --context cannot be empty")
	}
	options.sourceAgent = strings.TrimSpace(options.sourceAgent)
	options.heads = append([]string(nil), heads...)
	if options.sourceAgent != "" && options.contextPolicy != materializeContextAgent {
		return materializeOptions{}, errors.New("session materialize: --source is only valid with --context agent-only")
	}
	if len(options.heads) != 0 && options.contextPolicy != materializeContextCausal {
		return materializeOptions{}, errors.New("session materialize: --head is only valid with --context causal-head")
	}
	if options.contextPolicy == materializeContextAgent && options.sourceAgent == "" {
		return materializeOptions{}, errors.New("session materialize: --source is required with --context agent-only")
	}
	if options.contextPolicy != materializeContextCausal && options.contextPolicy != materializeContextAll && options.contextPolicy != materializeContextAgent {
		return materializeOptions{}, fmt.Errorf("session materialize: unsupported --context %q", options.contextPolicy)
	}
	return options, nil
}

// normalizeMaterializeArgs accepts the documented positional-first spelling:
// `session materialize <session-id> --to codex ...`. The standard flag parser
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
	if c == nil {
		return materializePreviewReport{}, errors.New("session materialize: configuration is unavailable")
	}
	if ctx == nil {
		return materializePreviewReport{}, errors.New("session materialize: context is required")
	}
	if err := ctx.Err(); err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: %w", err)
	}
	if err := devicePullError("session materialize", c); err != nil {
		return materializePreviewReport{}, err
	}
	if err := config.ValidateDeviceID(c.Device.ID); err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: local device identity is invalid: %w", err)
	}

	current, err := resolveCurrentProject(ctx, c, projectDir)
	if err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: identify the current project: %w", err)
	}
	if !current.Identity.Stable() {
		reason := current.Reason
		if reason == "" {
			reason = "the current directory has no stable project identity"
		}
		return materializePreviewReport{}, fmt.Errorf("session materialize: %s", reason)
	}
	switch projectPullMode(c, current.Identity.Value) {
	case projectModeExcluded:
		return materializePreviewReport{}, errors.New("session materialize: project is excluded from synchronization")
	case projectModePushOnly:
		return materializePreviewReport{}, errors.New("session materialize: project is configured as push-only; remote sessions are unavailable")
	}

	secrets, err := config.LoadSecrets(configDir)
	if err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: load local sync material: %w", err)
	}
	hubScope, projectScope, projectID, err := sessionHubAndProject(secrets.IdentifierKey, current)
	if err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: prepare Session Hub identity: %w", err)
	}
	sessionLayout, err := syncer.NewSessionHubLayout(hubScope.ID, projectID, options.sessionID)
	if err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: invalid logical Session identity: %w", err)
	}

	target, targetCapability, err := materializeTarget(ctx, options.targetAgent)
	if err != nil {
		return materializePreviewReport{}, err
	}
	sourceCapabilities, err := materializeSourceCapabilities()
	if err != nil {
		return materializePreviewReport{}, err
	}

	access, err := openDomainForRead(ctx, c, configDir, input, prompt, "session materialize")
	if err != nil {
		return materializePreviewReport{}, err
	}
	defer access.close()

	preview, err := syncflow.FetchMaterializePreview(ctx, syncflow.RemoteMaterializePreviewRequest{
		Store:         access.Store,
		Identities:    access.Identities,
		Layout:        sessionLayout,
		ContextPolicy: syncflow.MaterializeContextPolicy(options.contextPolicy),
		SourceAgent:   options.sourceAgent,
		Heads:         append([]string(nil), options.heads...),
		MaterializePreviewOptions: syncflow.MaterializePreviewOptions{
			SourceCapabilities: sourceCapabilities,
			TargetAgent:        options.targetAgent,
			TargetCapability:   targetCapability,
			Target: adapter.MaterializeTarget{
				PathSpace: adapter.PathSpace{ProjectRoot: current.Root, AgentHome: target.Installation.DataDir},
				CreatedAt: time.Now().UTC(),
			},
			AllowUnsupported: options.allowUnsupported,
		},
	})
	if err != nil {
		return materializePreviewReport{}, fmt.Errorf("session materialize: %w", err)
	}
	return materializePreviewReport{
		Preview:              true,
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
		SelectedRecordCount:  preview.SelectedRecordCount,
		ContextItems:         preview.ContextItems,
		Stats:                preview.Stats,
		WriteStatus:          "new-target-session-not-written",
	}, nil
}

func materializeTarget(ctx context.Context, name string) (adapter.AgentSessions, adapter.MaterializeCapability, error) {
	target, err := adapter.FindInstalled(ctx, name)
	if errors.Is(err, adapter.ErrNotInstalled) {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session materialize: target Agent %s is not installed", resumeAgentLabel(name))
	}
	if err != nil {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session materialize: inspect target Agent %s: %w", resumeAgentLabel(name), err)
	}
	capability, err := adapter.MaterializeFor(target.Layout)
	if err != nil {
		return adapter.AgentSessions{}, nil, fmt.Errorf("session materialize: target Agent %q has no materialize capability: %w", name, err)
	}
	return target, capability, nil
}

func materializeSourceCapabilities() (map[string]adapter.MaterializeCapability, error) {
	layouts, err := adapter.DefaultLayouts()
	if err != nil {
		return nil, fmt.Errorf("session materialize: discover built-in Agent capabilities: %w", err)
	}
	capabilities := make(map[string]adapter.MaterializeCapability, len(layouts))
	for _, layout := range layouts {
		capability, err := adapter.MaterializeFor(layout)
		if err != nil {
			if errors.Is(err, adapter.ErrMaterializeUnsupportedLayout) {
				continue
			}
			return nil, fmt.Errorf("session materialize: inspect %s materialize capability: %w", layout.Name(), err)
		}
		capabilities[layout.Name()] = capability
	}
	return capabilities, nil
}

func writeMaterializePreviewJSON(w io.Writer, report materializePreviewReport) error {
	if w == nil {
		return errors.New("session materialize: output is required")
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeMaterializePreviewText(w io.Writer, report materializePreviewReport) error {
	if w == nil {
		return errors.New("session materialize: output is required")
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
	if _, err := fmt.Fprintf(w, "write-status: %s\n", safeListText(report.WriteStatus)); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "source sessions, Agent files, LocalBinding and Remote objects: unchanged")
	return err
}
