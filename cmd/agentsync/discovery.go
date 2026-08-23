package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// writeCommandDiscovery is the compact command browser behind
// `agentsync help <command>`. It intentionally describes only command
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
		if actions := sortedDiscoveryValues(completionSubcommands[root]); len(actions) != 0 {
			if _, err := fmt.Fprintf(w, "agentsync %s commands:\n", root); err != nil {
				return err
			}
			for _, action := range actions {
				if _, err := fmt.Fprintf(w, "  agentsync %s %s\n", root, action); err != nil {
					return err
				}
			}
			return writeDiscoveryOptions(w, root)
		}
		if _, exists := findCommand(root); exists {
			return writeDiscoveryCommand(w, root)
		}
		return fmt.Errorf("command discovery: unknown command %q; run 'agentsync help'", root)
	}

	if len(path) == 2 {
		actions := completionSubcommands[root]
		for _, action := range actions {
			if action != path[1] {
				continue
			}
			name := root + " " + path[1]
			return writeDiscoveryCommand(w, name)
		}
	}
	return fmt.Errorf("command discovery: unknown command path %q; run 'agentsync help %s'", strings.Join(path, " "), root)
}

func writeCommandIndex(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "agentsync commands:"); err != nil {
		return err
	}
	for _, command := range commands {
		if actions := sortedDiscoveryValues(completionSubcommands[command.name]); len(actions) != 0 {
			if _, err := fmt.Fprintf(w, "  %-12s %s\n", command.name, strings.Join(actions, ", ")); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "  %-12s %s\n", command.name, command.summary); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "\nUse `agentsync help <group>` for its second-level commands, or `agentsync help <command> [action]` for flags.")
	return err
}

func writeDiscoveryCommand(w io.Writer, name string) error {
	if _, err := fmt.Fprintf(w, "agentsync %s [arguments]\n", name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %s\n", discoverySummary(name)); err != nil {
		return err
	}
	return writeDiscoveryOptions(w, name)
}

func writeDiscoveryOptions(w io.Writer, name string) error {
	options := sortedDiscoveryValues(completionOptions[name])
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
