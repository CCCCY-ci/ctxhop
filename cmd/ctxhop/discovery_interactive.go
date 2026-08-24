package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/term"
)

type discoveryCandidate struct {
	name       string
	summary    string
	hasActions bool
}

type pickerKey uint8

const (
	pickerKeyUnknown pickerKey = iota
	pickerKeyEnter
	pickerKeyUp
	pickerKeyDown
	pickerKeyLeft
	pickerKeyRight
	pickerKeyBackspace
	pickerKeyEscape
	pickerKeyCharacter
)

type pickerInput struct {
	kind pickerKey
	char byte
}

func commandDiscoveryTerminal() bool {
	if os.Getenv("CTXHOP_NO_INTERACTIVE") == "1" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func runInteractiveCommandDiscovery(path []string, input *os.File, output io.Writer) (err error) {
	if input == nil || output == nil {
		return errors.New("command discovery: interactive input and output are required")
	}
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return fmt.Errorf("command discovery: enter interactive mode: %w", err)
	}
	defer func() {
		if restoreErr := term.Restore(int(input.Fd()), state); err == nil && restoreErr != nil {
			err = fmt.Errorf("command discovery: restore terminal: %w", restoreErr)
		}
	}()

	current := append([]string(nil), path...)
	query := ""
	selected := 0
	for {
		candidates := discoveryCandidates(current, query)
		if selected >= len(candidates) {
			selected = len(candidates) - 1
		}
		if selected < 0 {
			selected = 0
		}
		if err := renderCommandPicker(output, current, query, candidates, selected); err != nil {
			return err
		}

		key, err := readPickerInput(input)
		if err != nil {
			return fmt.Errorf("command discovery: read selection: %w", err)
		}
		switch key.kind {
		case pickerKeyUp:
			if len(candidates) != 0 {
				selected = (selected - 1 + len(candidates)) % len(candidates)
			}
		case pickerKeyDown:
			if len(candidates) != 0 {
				selected = (selected + 1) % len(candidates)
			}
		case pickerKeyBackspace:
			if query != "" {
				query = query[:len(query)-1]
				selected = 0
			}
		case pickerKeyLeft, pickerKeyEscape:
			if len(current) == 0 {
				return clearPicker(output)
			}
			current = current[:len(current)-1]
			query = ""
			selected = 0
		case pickerKeyCharacter:
			if key.char == 'q' && query == "" {
				return clearPicker(output)
			}
			query += string(key.char)
			selected = 0
		case pickerKeyEnter, pickerKeyRight:
			if len(candidates) == 0 {
				continue
			}
			candidate := candidates[selected]
			if candidate.hasActions {
				current = append(current, candidate.name)
				query = ""
				selected = 0
				continue
			}
			selectedPath := append(append([]string(nil), current...), candidate.name)
			if err := clearPicker(output); err != nil {
				return err
			}
			return writeCommandDiscovery(output, selectedPath)
		}
	}
}

func discoveryCandidates(path []string, query string) []discoveryCandidate {
	var candidates []discoveryCandidate
	if len(path) == 0 {
		candidates = make([]discoveryCandidate, 0, len(commands))
		for _, command := range commands {
			candidates = append(candidates, discoveryCandidate{
				name:       command.name,
				summary:    command.summary,
				hasActions: len(completionSubcommands[command.name]) != 0,
			})
		}
	} else if len(path) == 1 {
		for _, action := range completionSubcommands[path[0]] {
			name := path[0] + " " + action
			candidates = append(candidates, discoveryCandidate{name: action, summary: discoverySummary(name), hasActions: len(completionSubcommands[name]) != 0})
		}
	} else if len(path) >= 2 {
		name := strings.Join(path, " ")
		for _, action := range completionSubcommands[name] {
			candidateName := name + " " + action
			candidates = append(candidates, discoveryCandidate{name: action, summary: discoverySummary(candidateName), hasActions: len(completionSubcommands[candidateName]) != 0})
		}
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return candidates
	}
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if strings.Contains(strings.ToLower(candidate.name), needle) || strings.Contains(strings.ToLower(candidate.summary), needle) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func renderCommandPicker(output io.Writer, path []string, query string, candidates []discoveryCandidate, selected int) error {
	if _, err := io.WriteString(output, "\x1b[2J\x1b[H"); err != nil {
		return err
	}
	if len(path) == 0 {
		if _, err := fmt.Fprintf(output, "%s commands\n", productName); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(output, "%s %s commands\n", productName, strings.Join(path, " ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(output, "Filter: %s\n\n", query); err != nil {
		return err
	}
	for index, candidate := range candidates {
		marker := "  "
		if index == selected {
			marker = "> "
		}
		suffix := ""
		if candidate.hasActions {
			suffix = "  [commands]"
		}
		if _, err := fmt.Fprintf(output, "%s%-14s %s%s\n", marker, candidate.name, candidate.summary, suffix); err != nil {
			return err
		}
	}
	if len(candidates) == 0 {
		if _, err := fmt.Fprintln(output, "  (no matching commands)"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(output, "\nType to filter · ↑/↓ select · Enter open · Backspace edit · ←/Esc back · q quit")
	return err
}

func clearPicker(output io.Writer) error {
	_, err := io.WriteString(output, "\x1b[2J\x1b[H")
	return err
}

func readPickerInput(input *os.File) (pickerInput, error) {
	var first [1]byte
	if _, err := input.Read(first[:]); err != nil {
		return pickerInput{}, err
	}
	switch first[0] {
	case '\r', '\n':
		return pickerInput{kind: pickerKeyEnter}, nil
	case 3, 4:
		return pickerInput{kind: pickerKeyEscape}, nil
	case 8, 127:
		return pickerInput{kind: pickerKeyBackspace}, nil
	case 0, 0xe0:
		var extended [1]byte
		if _, err := input.Read(extended[:]); err != nil {
			return pickerInput{}, err
		}
		return extendedPickerInput(extended[0]), nil
	case 0x1b:
		var sequence [2]byte
		n, err := input.Read(sequence[:])
		if err != nil {
			return pickerInput{}, err
		}
		if n == len(sequence) && sequence[0] == '[' {
			switch sequence[1] {
			case 'A':
				return pickerInput{kind: pickerKeyUp}, nil
			case 'B':
				return pickerInput{kind: pickerKeyDown}, nil
			case 'C':
				return pickerInput{kind: pickerKeyRight}, nil
			case 'D':
				return pickerInput{kind: pickerKeyLeft}, nil
			}
		}
		return pickerInput{kind: pickerKeyEscape}, nil
	default:
		return pickerInput{kind: pickerKeyCharacter, char: first[0]}, nil
	}
}

func extendedPickerInput(value byte) pickerInput {
	switch value {
	case 72:
		return pickerInput{kind: pickerKeyUp}
	case 80:
		return pickerInput{kind: pickerKeyDown}
	case 75:
		return pickerInput{kind: pickerKeyLeft}
	case 77:
		return pickerInput{kind: pickerKeyRight}
	default:
		return pickerInput{kind: pickerKeyUnknown}
	}
}
