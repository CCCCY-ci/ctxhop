package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var commandSubcommands = map[string][]string{
	"device":     {"status", "mode", "list", "rename", "remove", "rotate-key", "invite"},
	"history":    {"cleanup", "prune"},
	"hook":       {"install"},
	"passphrase": {"change", "reset"},
	"project":    {"bind", "unbind", "mode", "list", "discover"},
	"remote":     {"delete-session", "delete-project", "delete-all"},
	"session":    {"discover", "list", "materialize", "resume", "show"},
}

var commandOptions = map[string][]string{
	"init":                  {"--backend", "--path", "--endpoint", "--bucket", "--region", "--prefix", "--path-style", "--device-name", "--device-mode", "--no-hook", "--expect-domain-fingerprint", "--invite"},
	"hook install":          {"--agent"},
	"install":               {"--dir", "--no-path"},
	"uninstall":             {"--dir"},
	"status":                {"--json", "--remote"},
	"list":                  {"--json"},
	"resume":                {"--json", "--preview", "--workspace", "--allow-limited", "--allow-divergent", "--no-workspace-context", "--replace-existing", "--version", "--agent", "--replica"},
	"history":               {"--json"},
	"history prune":         {"--yes", "--remote-id", "--path", "--keep", "--before"},
	"stats":                 {"--json"},
	"push":                  {"--workspace", "--git-stash"},
	"watch":                 {"--interval", "--once", "--json"},
	"doctor":                {"--json"},
	"pull":                  {"--json"},
	"device status":         {"--json"},
	"device list":           {"--json"},
	"device remove":         {"--yes"},
	"device rotate-key":     {},
	"device invite":         {"--output"},
	"project bind":          {"--path", "--identity", "--name"},
	"project unbind":        {"--path", "--identity"},
	"project mode":          {"--path", "--identity"},
	"project list":          {"--json"},
	"project discover":      {"--json"},
	"session discover":      {"--json"},
	"session list":          {"--json"},
	"session materialize":   {"--json", "--preview", "--to", "--context", "--head", "--source", "--allow-unsupported"},
	"session show":          {"--json"},
	"session resume":        {"--json", "--preview", "--workspace", "--allow-limited", "--allow-divergent", "--no-workspace-context", "--replace-existing", "--version", "--agent", "--replica"},
	"remote delete-session": {"--yes", "--remote-id", "--path"},
	"remote delete-project": {"--yes", "--path"},
	"remote delete-all":     {"--yes"},
}

// writeCommandDiscovery is the compact command browser behind
// `ctxhop help <command>`. It intentionally describes only command
// names and flags; it does not try to turn every flag into an interactive
// wizard or guess values that belong to the user's project.
func writeCommandDiscovery(w io.Writer, path []string) error {
	if w == nil {
		return errors.New("command discovery: output is required")
	}
	for index, value := range path {
		path[index] = strings.TrimSpace(value)
		if path[index] == "" {
			return errors.New("command discovery: command name is required")
		}
	}
	if len(path) == 0 {
		return writeCommandIndex(w)
	}

	root := path[0]
	if len(path) == 1 {
		if actions := sortedDiscoveryValues(commandSubcommands[root]); len(actions) != 0 {
			if _, err := fmt.Fprintf(w, "%s %s commands:\n", cliName, root); err != nil {
				return err
			}
			for _, action := range actions {
				if _, err := fmt.Fprintf(w, "  %s %s %s\n", cliName, root, action); err != nil {
					return err
				}
			}
			return writeDiscoveryOptions(w, root)
		}
		if _, exists := findCommand(root); exists {
			return writeDiscoveryCommand(w, root)
		}
		return fmt.Errorf("command discovery: unknown command %q; run '%s help'", root, cliName)
	}

	name := strings.Join(path, " ")
	if actions := sortedDiscoveryValues(commandSubcommands[name]); len(actions) != 0 {
		if _, err := fmt.Fprintf(w, "%s %s commands:\n", cliName, name); err != nil {
			return err
		}
		for _, action := range actions {
			if _, err := fmt.Fprintf(w, "  %s %s %s\n", cliName, name, action); err != nil {
				return err
			}
		}
		return writeDiscoveryOptions(w, name)
	}
	if _, exists := commandOptions[name]; exists {
		return writeDiscoveryCommand(w, name)
	}
	return fmt.Errorf("command discovery: unknown command path %q; run '%s help %s'", name, cliName, root)
}

func writeCommandIndex(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s commands:\n", cliName); err != nil {
		return err
	}
	for _, command := range commands {
		if actions := sortedDiscoveryValues(commandSubcommands[command.name]); len(actions) != 0 {
			if _, err := fmt.Fprintf(w, "  %-12s %s\n", command.name, strings.Join(actions, ", ")); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "  %-12s %s\n", command.name, command.summary); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "\nUse `%s help <group>` for its second-level commands, or `%s help <command> [action]` for flags.\n", cliName, cliName)
	return err
}

func writeDiscoveryCommand(w io.Writer, name string) error {
	if _, err := fmt.Fprintf(w, "%s %s [arguments]\n", cliName, name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %s\n", discoverySummary(name)); err != nil {
		return err
	}
	return writeDiscoveryOptions(w, name)
}

func writeDiscoveryOptions(w io.Writer, name string) error {
	options := sortedDiscoveryValues(commandOptions[name])
	options = append(options, "--help")
	sort.Strings(options)
	if len(options) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(w, "  options: %s\n", strings.Join(options, ", "))
	return err
}

func discoverySummary(name string) string {
	if command, exists := findCommand(name); exists {
		return command.summary
	}
	if index := strings.IndexByte(name, ' '); index > 0 {
		if command, exists := findCommand(name[:index]); exists {
			return command.summary
		}
	}
	return "run this command"
}

func findCommand(name string) (command, bool) {
	for _, command := range commands {
		if command.name == name {
			return command, true
		}
	}
	return command{}, false
}

func sortedDiscoveryValues(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
