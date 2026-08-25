package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/ctxhop/internal/adapter"
	"github.com/CCCCY-ci/ctxhop/internal/config"
)

const hookActionInstall = "install"

type hookOptions struct {
	agent string
}

func init() {
	for i := range commands {
		if commands[i].name == "hook" {
			commands[i].run = runHook
		}
	}
}

func runHook(args []string) error {
	return runHookWithIO(args, os.Stdin, os.Stdout, "")
}

func runHookWithIO(args []string, input io.Reader, output io.Writer, executable string) error {
	options, err := parseHookOptions(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("hook install: input is required")
	}
	if output == nil {
		return errors.New("hook install: output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("hook install: %w", err)
	}
	c, err := config.Load(configDir)
	if err != nil {
		return fmt.Errorf("hook install: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		executable, err = os.Executable()
		if err != nil {
			return errors.New("hook install: cannot locate the ctxhop executable")
		}
	}

	prompter := &initPrompter{
		input:       bufio.NewReader(input),
		secretInput: input,
		output:      output,
	}
	switch options.agent {
	case "all":
		err = maybeInstallInitHook(c, configDir, prompter, executable)
	case "claude-code":
		err = installClaudeHookForCommand(c, configDir, prompter, executable)
	case "codex":
		err = maybeInstallCodexHook(c, configDir, prompter, executable)
	}
	if err != nil {
		return fmt.Errorf("hook install: %w", err)
	}
	_, err = fmt.Fprintln(output, "hook installation complete")
	return err
}

func installClaudeHookForCommand(c *config.Config, configDir string, p *initPrompter, executable string) error {
	home, err := adapter.DefaultHome()
	if err != nil {
		return fmt.Errorf("hook install: locate Claude Code: %w", err)
	}
	layout := adapter.Layout{Home: home}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := layout.Detect(ctx); errors.Is(err, adapter.ErrNotInstalled) {
		_, printErr := fmt.Fprintln(p.output, "Claude Code is not installed; hook skipped")
		return printErr
	} else if err != nil {
		return fmt.Errorf("hook install: inspect Claude Code: %w", err)
	}
	return installClaudeHook(c, configDir, p, executable, layout, c.HookScope.Effective())
}
func parseHookOptions(args []string) (hookOptions, error) {
	if len(args) == 0 {
		return hookOptions{}, errors.New("hook: expected 'install'")
	}
	if args[0] != hookActionInstall {
		return hookOptions{}, fmt.Errorf("hook: unknown action %q; expected 'install'", args[0])
	}

	flags := flag.NewFlagSet("hook install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	agent := flags.String("agent", "all", "agent to configure: all, claude-code, or codex")
	if err := flags.Parse(args[1:]); err != nil {
		return hookOptions{}, fmt.Errorf("hook install: %w", err)
	}
	if flags.NArg() != 0 {
		return hookOptions{}, fmt.Errorf("hook install: unexpected argument %q", flags.Arg(0))
	}

	switch strings.ToLower(strings.TrimSpace(*agent)) {
	case "", "all":
		return hookOptions{agent: "all"}, nil
	case "claude", "claude-code":
		return hookOptions{agent: "claude-code"}, nil
	case "codex":
		return hookOptions{agent: "codex"}, nil
	default:
		return hookOptions{}, fmt.Errorf("hook install: unsupported agent %q; use all, claude-code, or codex", *agent)
	}
}
