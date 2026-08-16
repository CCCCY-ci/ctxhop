package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

func readCommandPassphrase(input io.Reader, output io.Writer, command string) (string, error) {
	return readCommandSecret(input, output, command, "Passphrase: ")
}

func readCommandRecoveryKey(input io.Reader, output io.Writer, command string) (string, error) {
	return readCommandSecret(input, output, command, "Recovery key: ")
}

func readCommandSecret(input io.Reader, output io.Writer, command, prompt string) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	return readCommandSecretReader(bufio.NewReader(input), output, command, prompt)
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
