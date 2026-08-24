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
	return readCommandSecretWithEmptyPolicy(input, output, command, prompt, false)
}

func readCommandOptionalSecret(input io.Reader, output io.Writer, command, prompt string) (string, error) {
	return readCommandSecretWithEmptyPolicy(input, output, command, prompt, true)
}

func readCommandSecretWithEmptyPolicy(input io.Reader, output io.Writer, command, prompt string, allowEmpty bool) (string, error) {
	if input == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	return newCommandSecretReader(input).readWithEmptyPolicy(command, output, prompt, allowEmpty)
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
	return r.readWithEmptyPolicy(command, output, prompt, false)
}

func (r *commandSecretReader) readOptional(command string, output io.Writer, prompt string) (string, error) {
	return r.readWithEmptyPolicy(command, output, prompt, true)
}

func (r *commandSecretReader) readWithEmptyPolicy(command string, output io.Writer, prompt string, allowEmpty bool) (string, error) {
	if r == nil || r.raw == nil {
		return "", fmt.Errorf("%s: input is required", command)
	}
	if output == nil {
		return "", fmt.Errorf("%s: prompt output is required", command)
	}
	if value, handled, err := readTerminalSecret(r.raw, output, command, prompt, allowEmpty); handled {
		return value, err
	}
	return readCommandSecretReaderWithEmptyPolicy(r.lines, output, command, prompt, allowEmpty)
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

func readTerminalSecret(input io.Reader, output io.Writer, command, prompt string, allowEmpty bool) (string, bool, error) {
	file, ok := terminalInput(input)
	if !ok {
		return "", false, nil
	}
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return "", true, err
	}
	value, readErr := readTerminalSecretLine(file, output)
	if _, newlineErr := fmt.Fprintln(output); newlineErr != nil && readErr == nil {
		readErr = newlineErr
	}
	if readErr != nil {
		return "", true, fmt.Errorf("%s: read secret: %w", command, readErr)
	}
	if len(value) == 0 && !allowEmpty {
		return "", true, fmt.Errorf("%s: secret cannot be empty", command)
	}
	return string(value), true, nil
}

func readTerminalSecretLine(file *os.File, output io.Writer) (value []byte, err error) {
	oldState, makeRawErr := term.MakeRaw(int(file.Fd()))
	if makeRawErr != nil {
		// Some redirected or legacy Windows consoles do not support raw mode.
		// Keep the input functional, even though masking is unavailable there.
		return term.ReadPassword(int(file.Fd()))
	}
	defer func() {
		if restoreErr := term.Restore(int(file.Fd()), oldState); err == nil && restoreErr != nil {
			err = fmt.Errorf("restore terminal: %w", restoreErr)
		}
	}()
	return readMaskedSecretInput(file, output)
}

func readMaskedSecretInput(input io.Reader, output io.Writer) ([]byte, error) {
	if input == nil {
		return nil, errors.New("secret input is required")
	}
	if output == nil {
		return nil, errors.New("secret output is required")
	}

	var value []byte
	var one [1]byte
	for {
		n, err := input.Read(one[:])
		if n > 0 {
			switch one[0] {
			case '\r', '\n':
				return value, nil
			case '\b', 0x7f:
				if len(value) > 0 {
					value = value[:len(value)-1]
					if _, writeErr := io.WriteString(output, "\b \b"); writeErr != nil {
						return nil, writeErr
					}
				}
			case 0x15: // Ctrl-U: erase the current input.
				for len(value) > 0 {
					value = value[:len(value)-1]
					if _, writeErr := io.WriteString(output, "\b \b"); writeErr != nil {
						return nil, writeErr
					}
				}
			case 0x03:
				return nil, errors.New("secret input interrupted")
			case 0x04: // Ctrl-D behaves like EOF when no value is pending.
				if len(value) == 0 {
					return nil, io.EOF
				}
				return value, nil
			default:
				value = append(value, one[0])
				if _, writeErr := io.WriteString(output, "*"); writeErr != nil {
					return nil, writeErr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(value) > 0 {
				return value, nil
			}
			return value, err
		}
	}
}

func readCommandSecretReader(input *bufio.Reader, output io.Writer, command, prompt string) (string, error) {
	return readCommandSecretReaderWithEmptyPolicy(input, output, command, prompt, false)
}

func readCommandOptionalSecretReader(input *bufio.Reader, output io.Writer, command, prompt string) (string, error) {
	return readCommandSecretReaderWithEmptyPolicy(input, output, command, prompt, true)
}

func readCommandSecretReaderWithEmptyPolicy(input *bufio.Reader, output io.Writer, command, prompt string, allowEmpty bool) (string, error) {
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
		if allowEmpty {
			return "", nil
		}
		return "", io.EOF
	}
	if value == "" && !allowEmpty {
		return "", fmt.Errorf("%s: secret cannot be empty", command)
	}
	return value, nil
}

func readListPassphrase(input io.Reader, output io.Writer) (string, error) {
	return readCommandPassphrase(input, output, "list")
}
