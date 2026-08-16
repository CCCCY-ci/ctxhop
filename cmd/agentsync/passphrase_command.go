package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/CCCCY-ci/agentsync/internal/config"
	"github.com/CCCCY-ci/agentsync/internal/syncer"
)

const passphraseCommandTimeout = 30 * time.Second

const (
	passphraseActionChange = "change"
	passphraseActionReset  = "reset"
)

const recoveryResetReminder = "Recovery Key is unchanged by reset; keep your existing Recovery Key safe for future recovery."

func writeRecoveryResetReminder(prompt io.Writer) error {
	if prompt == nil {
		return errors.New("passphrase: prompt output is required")
	}
	_, err := fmt.Fprintln(prompt, recoveryResetReminder)
	return err
}

func init() {
	for i := range commands {
		if commands[i].name == "passphrase" {
			commands[i].run = runPassphrase
		}
	}
}

func runPassphrase(args []string) error {
	return runPassphraseWithStreams(args, os.Stdin, os.Stdout, os.Stderr)
}

func runPassphraseWithIO(args []string, input io.Reader, output io.Writer) error {
	return runPassphraseWithStreams(args, input, output, output)
}

func runPassphraseWithStreams(args []string, input io.Reader, output, prompt io.Writer) error {
	action, err := parsePassphraseAction(args)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("passphrase: input is required")
	}
	if output == nil {
		return errors.New("passphrase: output is required")
	}
	if prompt == nil {
		return errors.New("passphrase: prompt output is required")
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	c, err := config.Load(configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), passphraseCommandTimeout)
	defer cancel()

	if err := rotatePassphrase(ctx, c, configDir, action, input, prompt); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "passphrase: %s\n", action)
	return err
}

func parsePassphraseAction(args []string) (string, error) {
	if len(args) != 1 {
		if len(args) == 0 {
			return "", errors.New("passphrase: expected change or reset")
		}
		return "", fmt.Errorf("passphrase: expected one action, got %q", strings.Join(args, " "))
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case passphraseActionChange:
		return passphraseActionChange, nil
	case passphraseActionReset:
		return passphraseActionReset, nil
	default:
		return "", fmt.Errorf("passphrase: unknown action %q; expected change or reset", args[0])
	}
}

func rotatePassphrase(ctx context.Context, c *config.Config, configDir, action string, input io.Reader, prompt io.Writer) error {
	if c == nil {
		return errors.New("passphrase: configuration is unavailable")
	}
	store, keyfile, err := openDeviceRemote(ctx, c, configDir, "passphrase")
	if err != nil {
		return err
	}

	secretInput := bufio.NewReader(input)
	var next string
	switch action {
	case passphraseActionChange:
		current, err := readCommandSecretReader(secretInput, prompt, "passphrase", "Current passphrase: ")
		if err != nil {
			return err
		}
		next, err = readNewPassphraseFromReader(secretInput, prompt, "passphrase")
		if err != nil {
			return err
		}
		if err := keyfile.ChangePassphrase(current, next); err != nil {
			return fmt.Errorf("passphrase: change: %w", err)
		}
	case passphraseActionReset:
		if err := writeRecoveryResetReminder(prompt); err != nil {
			return err
		}
		recovery, err := readCommandSecretReader(secretInput, prompt, "passphrase", "Recovery key: ")
		if err != nil {
			return err
		}
		next, err = readNewPassphraseFromReader(secretInput, prompt, "passphrase")
		if err != nil {
			return err
		}
		if err := keyfile.ResetPassphrase(recovery, next); err != nil {
			return fmt.Errorf("passphrase: reset: %w", err)
		}
	default:
		return fmt.Errorf("passphrase: unsupported action %q", action)
	}

	if err := syncer.ReplaceKeyfile(ctx, store, keyfile); err != nil {
		return fmt.Errorf("passphrase: publish updated keyfile: %w", err)
	}
	return nil
}

func readNewPassphrase(input io.Reader, prompt io.Writer, command string) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	return readNewPassphraseFromReader(bufio.NewReader(input), prompt, command)
}

func readNewPassphraseFromReader(input *bufio.Reader, prompt io.Writer, command string) (string, error) {
	first, err := readCommandSecretReader(input, prompt, command, "New passphrase: ")
	if err != nil {
		return "", err
	}
	second, err := readCommandSecretReader(input, prompt, command, "Repeat new passphrase: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("%s: new passphrases do not match", command)
	}
	return first, nil
}
