package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

func readCommandPassphrase(input io.Reader, output io.Writer, command string) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	if _, err := fmt.Fprint(output, "Passphrase: "); err != nil {
		return "", err
	}
	value, err := bufio.NewReader(input).ReadString('\n')
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%s: read passphrase: %w", command, err)
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.EOF
	}
	if value == "" {
		return "", fmt.Errorf("%s: passphrase cannot be empty", command)
	}
	return value, nil
}

func readListPassphrase(input io.Reader, output io.Writer) (string, error) {
	return readCommandPassphrase(input, output, "list")
}
