package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/config"
	"github.com/CCCCY-ci/ctxhop/internal/project"
)

const projectCommandTimeout = 30 * time.Second

const (
	projectActionBind     = "bind"
	projectActionUnbind   = "unbind"
	projectActionMode     = "mode"
	projectActionList     = "list"
	projectActionDiscover = "discover"

	projectModeNormal = "normal"
)

type projectOptions struct {
	action   string
	mode     string
	path     string
	identity string
	name     string
	json     bool
}

type projectListReport struct {
	Scope    string             `json:"scope"`
	Projects []projectListEntry `json:"projects"`
}

type projectListEntry struct {
	Identity string   `json:"identity"`
	Mode     string   `json:"mode"`
	Roots    []string `json:"roots"`
}

type projectDiscoverReport struct {
	Scope    string                 `json:"scope"`
	Projects []projectDiscoverEntry `json:"projects"`
}

type projectDiscoverEntry struct {
	ProjectID    string    `json:"projectId"`
	IdentityKind string    `json:"identityKind"`
	Identity     string    `json:"identity"`
	AnnouncedAt  time.Time `json:"announcedAt"`
	Bound        bool      `json:"bound"`
}

func init() {
	for i := range commands {
		if commands[i].name == "project" {
			commands[i].run = runProject
		}
	}
}

func runProject(args []string) error {
	return runProjectWithStreams(args, os.Stdin, os.Stdout, os.Stdout)
}

func runProjectWithIO(args []string, output io.Writer) error {
	return runProjectWithStreams(args, strings.NewReader(""), output, output)
}

func runProjectWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	options, err := parseProjectOptions(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("project: output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), projectCommandTimeout)
	defer cancel()

	switch options.action {
	case projectActionDiscover:
		report, err := collectProjectDiscover(ctx, c, configDir, input, prompt)
		if err != nil {
			return err
		}
		if options.json {
			return writeProjectDiscoverJSON(output, report)
		}
		return writeProjectDiscoverText(output, report)
	case projectActionList:
		report := collectProjectList(c)
		if options.json {
			return writeProjectListJSON(output, report)
		}
		return writeProjectListText(output, report)
	case projectActionBind:
		identity, root, err := resolveProjectBinding(ctx, c, options)
		if err != nil {
			return err
		}
		changed, err := bindProject(c, identity, root)
		if err != nil {
			return err
		}
		if changed {
			if err := c.Save(configDir); err != nil {
				return fmt.Errorf("project bind: save configuration: %w", err)
			}
		}
		return writeProjectMutation(output, projectActionBind, identity, root, !changed)
	case projectActionUnbind:
		identity, root, err := resolveProjectUnbind(ctx, options)
		if err != nil {
			return err
		}
		removed, err := unbindProject(c, identity, root)
		if err != nil {
			return err
		}
		if err := c.Save(configDir); err != nil {
			return fmt.Errorf("project unbind: save configuration: %w", err)
		}
		return writeProjectUnbind(output, removed)
	case projectActionMode:
		identity, err := resolveProjectPolicyTarget(ctx, c, options)
		if err != nil {
			return err
		}
		changed, err := setProjectMode(c, identity, options.mode)
		if err != nil {
			return err
		}
		if changed {
			if err := c.Save(configDir); err != nil {
				return fmt.Errorf("project mode: save configuration: %w", err)
			}
		}
		return writeProjectMode(output, identity, options.mode, !changed)
	default:
		return fmt.Errorf("project: unsupported action %q", options.action)
	}
}

func parseProjectOptions(args []string) (projectOptions, error) {
	if len(args) == 0 {
		return projectOptions{}, errors.New("project: expected bind, unbind, mode, list, or discover")
	}

	switch args[0] {
	case projectActionBind:
		var options projectOptions
		options.action = projectActionBind
		flags := flag.NewFlagSet("project bind", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&options.path, "path", ".", "project directory to bind")
		flags.StringVar(&options.identity, "identity", "", "use this stable project identity")
		flags.StringVar(&options.name, "name", "", "create a manual project identity with this name")
		if err := flags.Parse(args[1:]); err != nil {
			return projectOptions{}, fmt.Errorf("project bind: %w", err)
		}
		if flags.NArg() != 0 {
			return projectOptions{}, fmt.Errorf("project bind: unexpected argument %q", flags.Arg(0))
		}
		if options.identity != "" && options.name != "" {
			return projectOptions{}, errors.New("project bind: use --identity or --name, not both")
		}
		if err := validateProjectIdentityArgument(options.identity); err != nil {
			return projectOptions{}, fmt.Errorf("project bind: %w", err)
		}
		if strings.ContainsRune(options.name, 0) {
			return projectOptions{}, errors.New("project bind: name contains an invalid character")
		}
		return options, nil

	case projectActionUnbind:
		var options projectOptions
		options.action = projectActionUnbind
		flags := flag.NewFlagSet("project unbind", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&options.path, "path", "", "local project directory")
		flags.StringVar(&options.identity, "identity", "", "project identity to unbind")
		if err := flags.Parse(args[1:]); err != nil {
			return projectOptions{}, fmt.Errorf("project unbind: %w", err)
		}
		if flags.NArg() != 0 {
			return projectOptions{}, fmt.Errorf("project unbind: unexpected argument %q", flags.Arg(0))
		}
		if err := validateProjectIdentityArgument(options.identity); err != nil {
			return projectOptions{}, fmt.Errorf("project unbind: %w", err)
		}
		if options.path == "" && options.identity == "" {
			options.path = "."
		}
		return options, nil

	case projectActionMode:
		if len(args) < 2 {
			return projectOptions{}, errors.New("project mode: expected normal, push-only, or excluded")
		}
		mode := strings.ToLower(strings.TrimSpace(args[1]))
		if mode != projectModeNormal && mode != projectModePushOnly && mode != projectModeExcluded {
			return projectOptions{}, errors.New("project mode: expected normal, push-only, or excluded")
		}
		var options projectOptions
		options.action = projectActionMode
		options.mode = mode
		flags := flag.NewFlagSet("project mode", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&options.path, "path", "", "project directory")
		flags.StringVar(&options.identity, "identity", "", "project identity")
		if err := flags.Parse(args[2:]); err != nil {
			return projectOptions{}, fmt.Errorf("project mode: %w", err)
		}
		if flags.NArg() != 0 {
			return projectOptions{}, fmt.Errorf("project mode: unexpected argument %q", flags.Arg(0))
		}
		if options.path != "" && options.identity != "" {
			return projectOptions{}, errors.New("project mode: use --path or --identity, not both")
		}
		if err := validateProjectIdentityArgument(options.identity); err != nil {
			return projectOptions{}, fmt.Errorf("project mode: %w", err)
		}
		if options.path == "" && options.identity == "" {
			options.path = "."
		}
		return options, nil

	case projectActionList:
		var options projectOptions
		options.action = projectActionList
		flags := flag.NewFlagSet("project list", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return projectOptions{}, fmt.Errorf("project list: %w", err)
		}
		if flags.NArg() != 0 {
			return projectOptions{}, fmt.Errorf("project list: unexpected argument %q", flags.Arg(0))
		}
		return options, nil

	case projectActionDiscover:
		var options projectOptions
		options.action = projectActionDiscover
		flags := flag.NewFlagSet("project discover", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.BoolVar(&options.json, "json", false, "write machine-readable JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return projectOptions{}, fmt.Errorf("project discover: %w", err)
		}
		if flags.NArg() != 0 {
			return projectOptions{}, fmt.Errorf("project discover: unexpected argument %q", flags.Arg(0))
		}
		return options, nil

	default:
		return projectOptions{}, fmt.Errorf("project: unknown action %q; expected bind, unbind, mode, list, or discover", args[0])
	}
}

func validateProjectIdentityArgument(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("identity cannot be empty")
	}
	if strings.ContainsRune(value, 0) {
		return errors.New("identity contains an invalid character")
	}
	return nil
}

func resolveProjectBinding(ctx context.Context, c *config.Config, options projectOptions) (string, string, error) {
	current, err := resolveCurrentProject(ctx, c, options.path)
	if err != nil {
		return "", "", fmt.Errorf("project bind: identify directory: %w", err)
	}
	root := filepath.Clean(current.Root)

	if options.name != "" {
		identity, err := project.ManualIdentity(options.name)
		if err != nil {
			return "", "", fmt.Errorf("project bind: %w", err)
		}
		return identity.Value, root, nil
	}
	if options.identity != "" {
		identity, err := normalizeProjectIdentity(options.identity)
		if err != nil {
			return "", "", fmt.Errorf("project bind: %w", err)
		}
		return identity, root, nil
	}
	if !current.Identity.Stable() {
		return "", "", errors.New("project bind: directory has no stable identity; use --name or --identity")
	}
	return current.Identity.Value, root, nil
}

func resolveProjectUnbind(ctx context.Context, options projectOptions) (string, string, error) {
	identity := ""
	if options.identity != "" {
		var err error
		identity, err = normalizeProjectIdentity(options.identity)
		if err != nil {
			return "", "", fmt.Errorf("project unbind: %w", err)
		}
	}
	if options.path == "" {
		return identity, "", nil
	}
	current, err := project.Identify(ctx, options.path)
	if err != nil {
		return "", "", fmt.Errorf("project unbind: identify directory: %w", err)
	}
	return identity, filepath.Clean(current.Root), nil
}

func resolveProjectPolicyTarget(ctx context.Context, c *config.Config, options projectOptions) (string, error) {
	if options.identity != "" {
		identity, err := normalizeProjectIdentity(options.identity)
		if err != nil {
			return "", fmt.Errorf("project mode: %w", err)
		}
		return identity, nil
	}
	current, err := resolveCurrentProject(ctx, c, options.path)
	if err != nil {
		return "", fmt.Errorf("project mode: identify directory: %w", err)
	}
	if !current.Identity.Stable() {
		return "", errors.New("project mode: directory has no stable identity; use --identity")
	}
	return current.Identity.Value, nil
}

func normalizeProjectIdentity(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("identity cannot be empty")
	}
	if strings.ContainsRune(value, 0) {
		return "", errors.New("identity contains an invalid character")
	}
	return value, nil
}

func bindProject(c *config.Config, identity, root string) (bool, error) {
	if c == nil {
		return false, errors.New("project bind: configuration is unavailable")
	}
	if identity == "" || root == "" {
		return false, errors.New("project bind: identity and root are required")
	}
	root = filepath.Clean(root)
	for _, binding := range c.Projects.Bindings {
		if binding.Identity == identity && strings.HasPrefix(identity, "manual:") && !sameProjectRoot(binding.LocalRoot, root) {
			return false, errors.New("project bind: manual identity is already bound to another local root; use a unique --name for each project")
		}
		if !sameProjectRoot(binding.LocalRoot, root) {
			continue
		}
		if binding.Identity != identity {
			return false, errors.New("project bind: local root is already bound to another identity")
		}
		return false, nil
	}
	c.Projects.Bindings = append(c.Projects.Bindings, config.Binding{
		Identity:  identity,
		LocalRoot: root,
	})
	sortProjectBindings(c.Projects.Bindings)
	return true, nil
}

func unbindProject(c *config.Config, identity, root string) (int, error) {
	if c == nil {
		return 0, errors.New("project unbind: configuration is unavailable")
	}
	if identity == "" && root == "" {
		return 0, errors.New("project unbind: identity or path is required")
	}
	kept := make([]config.Binding, 0, len(c.Projects.Bindings))
	removed := 0
	for _, binding := range c.Projects.Bindings {
		identityMatches := identity == "" || binding.Identity == identity
		rootMatches := root == "" || sameProjectRoot(binding.LocalRoot, root)
		if identityMatches && rootMatches {
			removed++
			continue
		}
		kept = append(kept, binding)
	}
	if removed == 0 {
		return 0, errors.New("project unbind: no matching binding exists")
	}
	c.Projects.Bindings = kept
	return removed, nil
}

func setProjectMode(c *config.Config, identity, mode string) (bool, error) {
	if c == nil {
		return false, errors.New("project mode: configuration is unavailable")
	}
	if identity == "" {
		return false, errors.New("project mode: identity is required")
	}
	if mode != projectModeNormal && mode != projectModePushOnly && mode != projectModeExcluded {
		return false, errors.New("project mode: unsupported mode")
	}

	beforeExcluded := append([]string(nil), c.Projects.Excluded...)
	beforePushOnly := append([]string(nil), c.Projects.PushOnly...)
	c.Projects.Excluded = removeProjectIdentity(c.Projects.Excluded, identity)
	c.Projects.PushOnly = removeProjectIdentity(c.Projects.PushOnly, identity)
	switch mode {
	case projectModePushOnly:
		c.Projects.PushOnly = append(c.Projects.PushOnly, identity)
	case projectModeExcluded:
		c.Projects.Excluded = append(c.Projects.Excluded, identity)
	}
	c.Projects.Excluded = normalizeProjectIdentities(c.Projects.Excluded)
	c.Projects.PushOnly = normalizeProjectIdentities(c.Projects.PushOnly)
	return !sameProjectIdentities(beforeExcluded, c.Projects.Excluded) ||
		!sameProjectIdentities(beforePushOnly, c.Projects.PushOnly), nil
}

func removeProjectIdentity(values []string, identity string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == identity {
			continue
		}
		out = append(out, value)
	}
	return out
}

func normalizeProjectIdentities(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sameProjectIdentities(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortProjectBindings(bindings []config.Binding) {
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Identity == bindings[j].Identity {
			return normalizedProjectRoot(bindings[i].LocalRoot) < normalizedProjectRoot(bindings[j].LocalRoot)
		}
		return bindings[i].Identity < bindings[j].Identity
	})
}

func normalizedProjectRoot(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	absolute = filepath.Clean(absolute)
	// Bindings are local and may have been created through a symlinked path.
	// Resolve only for comparison; callers still retain the path spelling they
	// received so Agent session slugs and user-facing output stay unchanged.
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	return filepath.Clean(absolute)
}

func sameProjectRoot(left, right string) bool {
	left = normalizedProjectRoot(left)
	right = normalizedProjectRoot(right)
	if left == "" || right == "" {
		return false
	}
	if filepath.VolumeName(left) != "" || filepath.VolumeName(right) != "" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func collectProjectList(c *config.Config) projectListReport {
	entries := make(map[string]*projectListEntry)
	if c == nil {
		return projectListReport{Scope: "global"}
	}
	get := func(identity string) *projectListEntry {
		entry := entries[identity]
		if entry == nil {
			entry = &projectListEntry{Identity: identity, Mode: projectModeNormal}
			entries[identity] = entry
		}
		return entry
	}
	for _, binding := range c.Projects.Bindings {
		entry := get(binding.Identity)
		root := normalizedProjectRoot(binding.LocalRoot)
		if root != "" && !containsProjectRoot(entry.Roots, root) {
			entry.Roots = append(entry.Roots, root)
		}
	}
	for _, identity := range c.Projects.PushOnly {
		get(identity).Mode = projectModePushOnly
	}
	for _, identity := range c.Projects.Excluded {
		get(identity).Mode = projectModeExcluded
	}

	out := make([]projectListEntry, 0, len(entries))
	for _, entry := range entries {
		sort.Strings(entry.Roots)
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Identity < out[j].Identity
	})
	return projectListReport{Scope: "global", Projects: out}
}

func containsProjectRoot(roots []string, want string) bool {
	for _, root := range roots {
		if sameProjectRoot(root, want) {
			return true
		}
	}
	return false
}

func writeProjectListJSON(w io.Writer, report projectListReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func writeProjectListText(w io.Writer, report projectListReport) error {
	if _, err := fmt.Fprintf(w, "scope: %s\n", report.Scope); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "projects: %d\n", len(report.Projects)); err != nil {
		return err
	}
	for _, entry := range report.Projects {
		roots := "none"
		if len(entry.Roots) > 0 {
			roots = strings.Join(entry.Roots, ",")
		}
		if _, err := fmt.Fprintf(w, "- identity=%s mode=%s roots=%s\n", entry.Identity, entry.Mode, roots); err != nil {
			return err
		}
	}
	return nil
}

func writeProjectMutation(w io.Writer, action, identity, root string, unchanged bool) error {
	state := "updated"
	if unchanged {
		state = "unchanged"
	}
	if _, err := fmt.Fprintf(w, "project %s: %s\n", action, state); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "identity: %s\n", identity); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "root: %s\n", root)
	return err
}

func writeProjectUnbind(w io.Writer, removed int) error {
	_, err := fmt.Fprintf(w, "project unbind: removed=%d\n", removed)
	return err
}

func writeProjectMode(w io.Writer, identity, mode string, unchanged bool) error {
	state := "updated"
	if unchanged {
		state = "unchanged"
	}
	_, err := fmt.Fprintf(w, "project mode: %s (%s)\n", mode, state)
	return err
}
