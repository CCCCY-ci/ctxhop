// Command ctxhop synchronises AI coding agent sessions across devices
// through storage the user owns, with no server in the middle.
//
// See the PRD in docs/ for the full design. The command table stays
// declarative; each implemented handler attaches itself during package
// initialisation.
package main

import (
	"fmt"
	"io"
	"os"
)

// Build metadata, injected at link time by scripts/build.sh.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// command describes one subcommand of the CLI.
type command struct {
	name    string
	summary string
	// run executes the command. A nil run means the command is declared but
	// not implemented yet.
	run func(args []string) error
}

// commands is the full command surface from PRD §11.1. Declaring the unbuilt
// ones keeps `ctxhop help` honest about where the project is.
//
// Handlers that read this slice are attached in init rather than in the literal
// below, because a self-referential initialiser is an initialisation cycle.
var commands = []command{
	{name: "init", summary: "configure storage backend, encryption password and agent hooks"},
	{name: "install", summary: "install ctxhop as a user-level command"},
	{name: "uninstall", summary: "remove the installed ctxhop command without deleting sync data"},
	{name: "status", summary: "show sync state for this project or globally"},
	{name: "list", summary: "list sessions available for this project"},
	{name: "resume", summary: "restore a session and its filtered environment"},
	{name: "history", summary: "list recoverable versions for a session"},
	{name: "passphrase", summary: "change or reset the storage encryption password"},
	{name: "stats", summary: "show local cross-device restore statistics"},
	{name: "push", summary: "push local sessions and filtered environments to the remote"},
	{name: "watch", summary: "watch local sessions and push changes"},
	{name: "hook", summary: "install Agent session lifecycle hooks"},
	{name: "doctor", summary: "diagnose agent, backend and configuration problems"},
	{name: "device", summary: "inspect or change local device settings"},
	{name: "remote", summary: "delete scoped or all remote data"},
	{name: "project", summary: "bind projects and manage synchronization policy"},
	{name: "pull", summary: "check remote metadata without restoring sessions"},
	{name: "version", summary: "print version information", run: runVersion},
	{name: "help", summary: "print this message"},
}

func init() {
	for i := range commands {
		if commands[i].name == "help" {
			commands[i].run = runHelp
		}
	}
}

func main() {
	args := os.Args[1:]
	startCommandLogging(args)
	defer func() {
		commandLogger = nil
	}()

	err := run(args)
	finishCommandLogging(args, err)
	if err != nil {
		recordCommandFailure(os.Args[1:], err)
		fmt.Fprintf(os.Stderr, "%s: %v\n", cliName, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == installerWelcomeArgument {
		return writeInstallerWelcome(os.Stdout)
	}
	if len(args) == 0 {
		return runDefaultEntry()
	}
	if path, ok := commandHelpPath(args); ok {
		return runHelp(path)
	}

	name := args[0]
	for _, c := range commands {
		if c.name != name {
			continue
		}
		if c.run == nil {
			return fmt.Errorf("command %q is not implemented yet; see docs/ for the roadmap", name)
		}
		return c.run(args[1:])
	}

	return fmt.Errorf("unknown command %q; run '%s help' for usage", name, cliName)
}

func commandHelpPath(args []string) ([]string, bool) {
	for index, arg := range args {
		if arg == "--help" || arg == "-h" {
			return append([]string(nil), args[:index]...), true
		}
	}
	return nil, false
}

func runVersion([]string) error {
	_, err := fmt.Fprintf(os.Stdout, "%s %s\n", cliName, version)
	return err
}

func runHelp(args []string) error {
	// Usage goes to stdout: it is the requested output of `help`, not a
	// diagnostic (§11.2).
	if len(args) != 0 {
		return writeCommandDiscovery(os.Stdout, args)
	}
	return writeHelp(os.Stdout)
}

func runDefaultEntry() error {
	return runHelp(nil)
}

func writeHelp(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "%s - cross-device sync for AI coding agent sessions\n\nusage:\n  %s <command> [arguments]\n\ncommands:\n", cliName, cliName); err != nil {
		return err
	}
	for _, c := range commands {
		marker := ""
		if c.run == nil {
			marker = "  (not implemented yet)"
		}
		if _, err := fmt.Fprintf(w, "  %-8s %s%s\n", c.name, c.summary, marker); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "\nYour sessions are encrypted locally and stored in a backend you own.\nThis tool collects no data of any kind.\nRun `%s help <command>` for command details.\n", cliName)
	return err
}
