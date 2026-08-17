package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

func readCommandPassphrase(input io.Reader, output io.Writer, command string) (string, error) {
	return readCommandSecret(input, output, command, "Encryption password: ")
}

func readCommandRecoveryKey(input io.Reader, output io.Writer, command string) (string, error) {
	return readCommandSecret(input, output, command, "Recovery key: ")
}

func readCommandSecret(input io.Reader, output io.Writer, command, prompt string) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	return newCommandSecretReader(input).read(command, output, prompt)
}

// commandSecretReader keeps the original input for terminal detection and a
// buffered reader for piped input and tests. A terminal must be read directly;
// otherwise bufio may consume bytes that term.ReadPassword expects to read.
type commandSecretReader struct {
	raw   io.Reader
	lines *bufio.Reader
}

func newCommandSecretReader(input io.Reader) *commandSecretReader {
	return &commandSecretReader{raw: input, lines: bufio.NewReader(input)}
}

func (r *commandSecretReader) read(command string, output io.Writer, prompt string) (string, error) {
	if r == nil || r.raw == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	if value, handled, err := readTerminalSecret(r.raw, output, command, prompt); handled {
		return value, err
	}
	return readCommandSecretReader(r.lines, output, command, prompt)
}

func terminalInput(input io.Reader) (*os.File, bool) {
	file, ok := input.(*os.File)
	if !ok {
		return nil, false
	}
	fd := int(file.Fd())
	if !term.IsTerminal(fd) {
		return nil, false
	}
	return file, true
}

func readTerminalSecret(input io.Reader, output io.Writer, command, prompt string) (string, bool, error) {
	file, ok := terminalInput(input)
	if !ok {
		return "", false, nil
	}
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", true, err
	}
	value, readErr := term.ReadPassword(int(file.Fd()))
	if _, newlineErr := fmt.Fprintln(output); newlineErr != nil && readErr == nil {
		return "", true, newlineErr
	}
	if readErr != nil {
		return "", true, fmt.Errorf("%s: read secret: %w", command, readErr)
	}
	if len(value) == 0 {
		return "", true, fmt.Errorf("%s: secret cannot be empty", command)
	}
	return string(value), true, nil
}

func readCommandSecretReader(input *bufio.Reader, output io.Writer, command, prompt string) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", err
	}
	value, err := input.ReadString('\n')
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s: read secret: %w", command, err)
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.EOF
	}
	if value == "" {
		return "", fmt.Errorf("%s: secret cannot be empty", command)
	}
	return value, nil
}

func readListPassphrase(input io.Reader, output io.Writer) (string, error) {
	return readCommandPassphrase(input, output, "list")
}
