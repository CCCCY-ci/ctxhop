// Command agentsync synchronises AI coding agent sessions across devices
// through storage the user owns, with no server in the middle.
//
// See the PRD in docs/ for the full design. This binary is a scaffold: the
// command surface is declared here, but the sync implementation is gated on
// PoC-1 (does a Claude Code session survive a cross-device, cross-path move at
// all?). Commands that are not implemented yet say so instead of pretending.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
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
// ones keeps `agentsync help` honest about where the project is.
//
// Handlers that read this slice are attached in init rather than in the literal
// below, because a self-referential initialiser is an initialisation cycle.
var commands = []command{
	{name: "init", summary: "configure storage backend, passphrase and agent hooks"},
	{name: "status", summary: "show sync state for this project or globally"},
	{name: "list", summary: "list sessions available for this project"},
	{name: "resume", summary: "restore a session onto this machine"},
	{name: "push", summary: "push local session changes to the remote"},
	{name: "doctor", summary: "diagnose agent, backend and configuration problems"},
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
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentsync: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runHelp(nil)
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

	return fmt.Errorf("unknown command %q; run 'agentsync help' for usage", name)
}

func runVersion([]string) error {
	_, err := fmt.Fprintf(os.Stdout, "agentsync %s (commit %s, built %s, %s/%s, %s)\n",
		version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return err
}

func runHelp([]string) error {
	// Usage goes to stdout: it is the requested output of `help`, not a
	// diagnostic (§11.2).
	return writeHelp(os.Stdout)
}

func writeHelp(w io.Writer) error {
	if _, err := fmt.Fprint(w, "agentsync - cross-device sync for AI coding agent sessions\n\nusage:\n  agentsync <command> [arguments]\n\ncommands:\n"); err != nil {
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
	_, err := fmt.Fprint(w, "\nYour sessions are encrypted locally and stored in a backend you own.\nThis tool collects no data of any kind.\n")
	return err
}
