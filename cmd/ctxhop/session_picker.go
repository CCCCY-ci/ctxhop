package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/CCCCY-ci/ctxhop/internal/syncer"
	"golang.org/x/term"
)

const sessionPickerEscapeTimeout = 40 * time.Millisecond

var errSessionPickerCancelled = errors.New("resume: session selection cancelled")

type interactivePickerOptions struct {
	errorPrefix  string
	heading      string
	help         string
	itemNoun     string
	emptyMessage string
	cancelError  error
}

func defaultSessionPickerOptions() interactivePickerOptions {
	return interactivePickerOptions{
		errorPrefix:  "resume",
		heading:      "Resume a session",
		help:         "Type to search  |  Up/Down move  |  Enter select  |  Esc cancel",
		itemNoun:     "session",
		emptyMessage: "No matching sessions. Keep typing or press Esc.",
		cancelError:  errSessionPickerCancelled,
	}
}

func (options interactivePickerOptions) withDefaults() interactivePickerOptions {
	defaults := defaultSessionPickerOptions()
	if options.errorPrefix == "" {
		options.errorPrefix = defaults.errorPrefix
	}
	if options.heading == "" {
		options.heading = defaults.heading
	}
	if options.help == "" {
		options.help = defaults.help
	}
	if options.itemNoun == "" {
		options.itemNoun = defaults.itemNoun
	}
	if options.emptyMessage == "" {
		options.emptyMessage = defaults.emptyMessage
	}
	if options.cancelError == nil {
		options.cancelError = defaults.cancelError
	}
	return options
}

type pickerKeyKind uint8

const (
	pickerKeyNone pickerKeyKind = iota
	pickerKeyRune
	pickerKeyEnter
	pickerKeyBackspace
	pickerKeyClear
	pickerKeyUp
	pickerKeyDown
	pickerKeyHome
	pickerKeyEnd
	pickerKeyPageUp
	pickerKeyPageDown
	pickerKeyEscape
	pickerKeyCancel
)

type pickerKey struct {
	kind pickerKeyKind
	rune rune
}

type sessionPickerItem struct {
	id        string
	title     string
	agent     string
	detail    string
	updatedAt time.Time
	records   uint64
}

type sessionPicker struct {
	input         *os.File
	output        io.Writer
	items         []sessionPickerItem
	options       interactivePickerOptions
	query         string
	selected      int
	renderedLines int
}

func runSessionPicker(input io.Reader, output io.Writer, items []sessionPickerItem) (selected string, err error) {
	return runInteractivePicker(input, output, items, defaultSessionPickerOptions())
}

func runInteractivePicker(input io.Reader, output io.Writer, items []sessionPickerItem, pickerOptions interactivePickerOptions) (selected string, err error) {
	pickerOptions = pickerOptions.withDefaults()
	if input == nil {
		return "", fmt.Errorf("%s: interactive selection requires input", pickerOptions.errorPrefix)
	}
	if output == nil {
		return "", fmt.Errorf("%s: interactive selection requires output", pickerOptions.errorPrefix)
	}
	if len(items) == 0 {
		return "", fmt.Errorf("%s: no %s options are available to select", pickerOptions.errorPrefix, pickerOptions.itemNoun)
	}
	file, ok := terminalInput(input)
	if !ok {
		return "", fmt.Errorf("%s: interactive selection requires a terminal; specify an explicit ID for non-interactive use", pickerOptions.errorPrefix)
	}
	oldState, err := term.MakeRaw(int(file.Fd()))
	if err != nil {
		return "", fmt.Errorf("%s: enable interactive selection: %w", pickerOptions.errorPrefix, err)
	}
	restoreOutput, outputErr := prepareSessionPickerOutput(output)
	if outputErr != nil {
		_ = term.Restore(int(file.Fd()), oldState)
		return "", fmt.Errorf("%s: enable interactive rendering: %w", pickerOptions.errorPrefix, outputErr)
	}
	defer func() {
		if _, cursorErr := io.WriteString(output, "\x1b[?25h"); err == nil && cursorErr != nil {
			err = fmt.Errorf("%s: restore picker cursor: %w", pickerOptions.errorPrefix, cursorErr)
		}
		if restoreErr := term.Restore(int(file.Fd()), oldState); err == nil && restoreErr != nil {
			err = fmt.Errorf("%s: restore terminal: %w", pickerOptions.errorPrefix, restoreErr)
		}
		if restoreErr := restoreOutput(); err == nil && restoreErr != nil {
			err = fmt.Errorf("%s: restore terminal rendering: %w", pickerOptions.errorPrefix, restoreErr)
		}
	}()

	picker := &sessionPicker{
		input:   file,
		output:  output,
		items:   append([]sessionPickerItem(nil), items...),
		options: pickerOptions,
	}
	if _, err := io.WriteString(output, "\x1b[?25l"); err != nil {
		return "", err
	}
	if err := picker.render(); err != nil {
		return "", err
	}

	for {
		key, readErr := readSessionPickerKey(file)
		if readErr != nil {
			return "", fmt.Errorf("%s: read interactive selection: %w", pickerOptions.errorPrefix, readErr)
		}
		selected, done, handleErr := picker.handle(key)
		if handleErr != nil {
			return "", handleErr
		}
		if done {
			if _, err := fmt.Fprintln(output); err != nil {
				return "", err
			}
			return selected, nil
		}
		if key.kind != pickerKeyNone {
			if err := picker.render(); err != nil {
				return "", err
			}
		}
	}
}

func (p *sessionPicker) handle(key pickerKey) (selected string, done bool, err error) {
	if p == nil {
		return "", false, errors.New("resume: session picker is unavailable")
	}
	options := p.pickerOptions()
	matches := p.matches()
	switch key.kind {
	case pickerKeyRune:
		if key.rune != 0 && !unicode.IsControl(key.rune) {
			p.query += string(key.rune)
			p.selected = 0
		}
	case pickerKeyBackspace:
		if p.query != "" {
			_, size := utf8.DecodeLastRuneInString(p.query)
			p.query = p.query[:len(p.query)-size]
			p.selected = 0
		}
	case pickerKeyClear:
		p.query = ""
		p.selected = 0
	case pickerKeyUp:
		p.moveSelection(-1, len(matches))
	case pickerKeyDown:
		p.moveSelection(1, len(matches))
	case pickerKeyHome:
		if len(matches) != 0 {
			p.selected = 0
		}
	case pickerKeyEnd:
		if len(matches) != 0 {
			p.selected = len(matches) - 1
		}
	case pickerKeyPageUp:
		p.moveSelection(-p.pageSize(), len(matches))
	case pickerKeyPageDown:
		p.moveSelection(p.pageSize(), len(matches))
	case pickerKeyEnter:
		if len(matches) != 0 {
			return p.items[matches[p.selected]].id, true, nil
		}
	case pickerKeyEscape, pickerKeyCancel:
		return "", false, options.cancelError
	}
	p.clampSelection()
	return "", false, nil
}

func (p *sessionPicker) pickerOptions() interactivePickerOptions {
	if p == nil {
		return defaultSessionPickerOptions()
	}
	return p.options.withDefaults()
}

func (p *sessionPicker) moveSelection(delta, count int) {
	if count == 0 || delta == 0 {
		return
	}
	p.selected += delta
	if p.selected < 0 {
		p.selected = count - 1
	}
	if p.selected >= count {
		p.selected = 0
	}
}

func (p *sessionPicker) clampSelection() {
	matches := p.matches()
	if len(matches) == 0 {
		p.selected = 0
		return
	}
	if p.selected < 0 {
		p.selected = 0
	}
	if p.selected >= len(matches) {
		p.selected = len(matches) - 1
	}
}

func (p *sessionPicker) pageSize() int {
	_, height := p.dimensions()
	page := height - 8
	if page < 1 {
		return 1
	}
	return page
}

func (p *sessionPicker) matches() []int {
	type match struct {
		index int
		score int
	}
	matched := make([]match, 0, len(p.items))
	for index, item := range p.items {
		if score, ok := pickerMatchScore(item, p.query); ok {
			matched = append(matched, match{index: index, score: score})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		return matched[i].score < matched[j].score
	})
	matches := make([]int, 0, len(matched))
	for _, value := range matched {
		matches = append(matches, value.index)
	}
	return matches
}

func (p *sessionPicker) render() error {
	lines := p.lines()
	if p.renderedLines != 0 {
		if _, err := fmt.Fprintf(p.output, "\x1b[%dA\x1b[J", p.renderedLines); err != nil {
			return err
		}
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(p.output, "\x1b[2K\r%s\n", line); err != nil {
			return err
		}
	}
	p.renderedLines = len(lines)
	return nil
}

func (p *sessionPicker) legacyLines() []string {
	options := p.pickerOptions()
	matches := p.matches()
	width, height := p.dimensions()
	rowLimit := height - 8
	if rowLimit < 1 {
		rowLimit = 1
	}
	start, end := pickerViewport(p.selected, len(matches), rowLimit)

	search := "Search  " + p.query
	if p.query == "" {
		search += "_"
	}
	count := pickerCountLabelForNoun(len(matches), len(p.items), options.itemNoun)
	if len(matches) > rowLimit {
		count += fmt.Sprintf("  ·  showing %d–%d", start+1, end)
	}
	lines := []string{
		"Resume a session",
		"Type to search  ·  ↑/↓ move  ·  Enter select  ·  Esc cancel",
		"",
		clipPickerText(search, width),
		clipPickerText(count, width),
	}
	if len(matches) == 0 {
		lines = append(lines, "No matching sessions. Keep typing or press Esc.")
		return lines
	}
	for position := start; position < end; position++ {
		item := p.items[matches[position]]
		line := pickerDisplayItemLine(item, width, position == p.selected)
		lines = append(lines, line)
	}
	return lines
}

func (p *sessionPicker) lines() []string {
	options := p.pickerOptions()
	matches := p.matches()
	width, height := p.dimensions()
	rowLimit := height - 8
	if rowLimit < 1 {
		rowLimit = 1
	}
	start, end := pickerViewport(p.selected, len(matches), rowLimit)

	search := "Search  " + p.query
	if p.query == "" {
		search += "_"
	}
	count := pickerCountLabelForNoun(len(matches), len(p.items), options.itemNoun)
	if len(matches) > rowLimit {
		count += fmt.Sprintf("  |  showing %d-%d", start+1, end)
	}
	lines := []string{
		options.heading,
		options.help,
		"",
		clipPickerText(search, width),
		clipPickerText(count, width),
	}
	if len(matches) == 0 {
		lines = append(lines, options.emptyMessage)
		return lines
	}
	for position := start; position < end; position++ {
		item := p.items[matches[position]]
		line := pickerDisplayItemLine(item, width, position == p.selected)
		lines = append(lines, line)
	}
	return lines
}

func (p *sessionPicker) dimensions() (width, height int) {
	if p != nil && p.input != nil {
		width, height, err := term.GetSize(int(p.input.Fd()))
		if err == nil && width > 0 && height > 0 {
			return width, height
		}
	}
	return 80, 24
}

func pickerViewport(selected, count, limit int) (start, end int) {
	if count == 0 {
		return 0, 0
	}
	if limit < 1 {
		limit = 1
	}
	if count <= limit {
		return 0, count
	}
	if selected < 0 {
		selected = 0
	}
	if selected >= count {
		selected = count - 1
	}
	start = selected - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > count {
		start = count - limit
	}
	return start, start + limit
}

func pickerCountLabel(matching, total int) string {
	return pickerCountLabelForNoun(matching, total, "session")
}

func pickerCountLabelForNoun(matching, total int, noun string) string {
	noun = strings.TrimSpace(noun)
	if noun == "" {
		noun = "item"
	}
	plural := noun + "s"
	if matching == total {
		return fmt.Sprintf("%d %s", total, pickerPlural(total, noun, plural))
	}
	return fmt.Sprintf("%d matching of %d %s", matching, total, plural)
}

func pickerPlural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func pickerDisplayItemLine(item sessionPickerItem, width int, selected bool) string {
	title := pickerItemTitle(item.title)
	meta := make([]string, 0, 3)
	if agent := pickerAgentLabel(item.agent); agent != "" {
		meta = append(meta, agent)
	}
	if detail := safeListText(item.detail); detail != "" {
		meta = append(meta, detail)
	}
	if !item.updatedAt.IsZero() {
		meta = append(meta, formatPickerAge(item.updatedAt))
	}
	if item.records != 0 {
		meta = append(meta, fmt.Sprintf("%d records", item.records))
	}
	line := title
	if len(meta) != 0 {
		line += "  |  " + strings.Join(meta, "  |  ")
	}
	prefix := "  "
	if selected {
		prefix = "> "
	}
	line = prefix + clipPickerText(line, width-len(prefix))
	if selected {
		return "\x1b[1;38;5;208m" + line + "\x1b[0m"
	}
	return line
}

// pickerItemLine is kept as the small, stable row helper used by the picker
// tests and older callers. The interactive renderer uses the display-safe
// variant above so a redirected terminal never receives accidental glyphs.
func pickerItemLine(item sessionPickerItem, width int, selected bool) string {
	title := pickerItemTitle(item.title)
	meta := make([]string, 0, 3)
	if agent := pickerAgentLabel(item.agent); agent != "" {
		meta = append(meta, agent)
	}
	if !item.updatedAt.IsZero() {
		meta = append(meta, formatPickerAge(item.updatedAt))
	}
	if item.records != 0 {
		meta = append(meta, fmt.Sprintf("%d records", item.records))
	}
	line := title
	if len(meta) != 0 {
		line += "  ·  " + strings.Join(meta, "  ·  ")
	}
	prefix := "  "
	if selected {
		prefix = "> "
	}
	line = prefix + clipPickerText(line, width-len(prefix))
	if selected {
		return "\x1b[1;38;5;208m" + line + "\x1b[0m"
	}
	return line
}

func pickerItemTitle(title string) string {
	title = safeListText(title)
	if title == "" || title == "encrypted session metadata" {
		return "Untitled session"
	}
	return title
}

func pickerAgentLabel(agent string) string {
	parts := strings.Split(agent, ",")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		switch part {
		case "codex":
			part = "Codex"
		case "claude-code":
			part = "Claude Code"
		default:
			part = safeListText(part)
		}
		labels = appendUnique(labels, part)
	}
	return strings.Join(labels, ", ")
}

func formatPickerAge(value time.Time) string {
	delta := time.Now().Sub(value)
	if delta < 0 {
		return "just now"
	}
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		return fmt.Sprintf("%dm ago", int(delta/time.Minute))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(delta/time.Hour))
	case delta < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(delta/(24*time.Hour)))
	default:
		return value.Local().Format("Jan 2")
	}
}

func clipPickerText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = safeListText(value)
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func pickerMatchesQuery(item sessionPickerItem, query string) bool {
	_, ok := pickerMatchScore(item, query)
	return ok
}

func pickerMatchScore(item sessionPickerItem, query string) (int, bool) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 0, true
	}
	fields := []string{item.title, item.agent, item.detail, item.id}
	best := 0
	found := false
	for fieldIndex, field := range fields {
		field = strings.ToLower(safeListText(field))
		if field == "" {
			continue
		}
		if strings.Contains(field, query) {
			score := fieldIndex * 10
			if !found || score < best {
				best = score
				found = true
			}
			continue
		}
		if pickerSubsequence(query, field) {
			score := 100 + fieldIndex*10
			if !found || score < best {
				best = score
				found = true
			}
		}
	}
	return best, found
}

func pickerSubsequence(query, value string) bool {
	queryRunes := []rune(query)
	valueRunes := []rune(value)
	if len(queryRunes) == 0 || len(queryRunes) > len(valueRunes) {
		return false
	}
	queryIndex := 0
	for _, valueRune := range valueRunes {
		if valueRune == queryRunes[queryIndex] {
			queryIndex++
			if queryIndex == len(queryRunes) {
				return true
			}
		}
	}
	return false
}

func readSessionPickerKey(input *os.File) (pickerKey, error) {
	first, err := readPickerByte(input)
	if err != nil {
		return pickerKey{}, err
	}
	switch first {
	case 0x03, 0x04:
		return pickerKey{kind: pickerKeyCancel}, nil
	case 0x09:
		return pickerKey{}, nil
	case 0x0a, 0x0d:
		return pickerKey{kind: pickerKeyEnter}, nil
	case 0x08, 0x7f:
		return pickerKey{kind: pickerKeyBackspace}, nil
	case 0x15:
		return pickerKey{kind: pickerKeyClear}, nil
	case 0x01:
		return pickerKey{kind: pickerKeyHome}, nil
	case 0x05:
		return pickerKey{kind: pickerKeyEnd}, nil
	case 0x1b:
		return readSessionPickerEscape(input)
	}
	if first < utf8.RuneSelf {
		if unicode.IsControl(rune(first)) {
			return pickerKey{}, nil
		}
		return pickerKey{kind: pickerKeyRune, rune: rune(first)}, nil
	}

	width := pickerUTF8Width(first)
	if width < 2 || width > utf8.UTFMax {
		return pickerKey{}, nil
	}
	encoded := make([]byte, width)
	encoded[0] = first
	for index := 1; index < width; index++ {
		encoded[index], err = readPickerByte(input)
		if err != nil {
			return pickerKey{}, err
		}
	}
	r, size := utf8.DecodeRune(encoded)
	if r == utf8.RuneError && size == 1 {
		return pickerKey{}, nil
	}
	return pickerKey{kind: pickerKeyRune, rune: r}, nil
}

func pickerUTF8Width(first byte) int {
	switch {
	case first >= 0xc2 && first <= 0xdf:
		return 2
	case first >= 0xe0 && first <= 0xef:
		return 3
	case first >= 0xf0 && first <= 0xf4:
		return 4
	default:
		return 0
	}
}

func readSessionPickerEscape(input *os.File) (pickerKey, error) {
	second, available, err := readPickerByteTimeout(input, sessionPickerEscapeTimeout)
	if err != nil {
		return pickerKey{}, err
	}
	if !available {
		return pickerKey{kind: pickerKeyEscape}, nil
	}
	if second != '[' && second != 'O' {
		return pickerKey{kind: pickerKeyEscape}, nil
	}

	sequence := make([]byte, 0, 8)
	for len(sequence) < 16 {
		value, readErr := readPickerByte(input)
		if readErr != nil {
			return pickerKey{}, readErr
		}
		sequence = append(sequence, value)
		switch value {
		case 'A':
			return pickerKey{kind: pickerKeyUp}, nil
		case 'B':
			return pickerKey{kind: pickerKeyDown}, nil
		case 'C', 'D':
			return pickerKey{}, nil
		case 'H':
			return pickerKey{kind: pickerKeyHome}, nil
		case 'F':
			return pickerKey{kind: pickerKeyEnd}, nil
		case '~':
			switch string(sequence) {
			case "1~", "7~":
				return pickerKey{kind: pickerKeyHome}, nil
			case "3~":
				return pickerKey{kind: pickerKeyBackspace}, nil
			case "4~", "8~":
				return pickerKey{kind: pickerKeyEnd}, nil
			case "5~":
				return pickerKey{kind: pickerKeyPageUp}, nil
			case "6~":
				return pickerKey{kind: pickerKeyPageDown}, nil
			default:
				return pickerKey{}, nil
			}
		}
	}
	return pickerKey{}, nil
}

func resumePickerItems(candidates []resumeCandidate) []sessionPickerItem {
	items := make([]sessionPickerItem, 0, len(candidates))
	for _, candidate := range candidates {
		items = append(items, sessionPickerItem{
			id:        candidate.Group.SessionID,
			title:     pickerItemTitle(candidate.Summary.Title),
			agent:     candidate.Summary.Agent,
			updatedAt: pickerSessionTime(candidate.Summary.UpdatedAt, candidate.Summary.CreatedAt),
			records:   pickerLegacyRecordCount(candidate.Group),
		})
	}
	sortSessionPickerItems(items)
	return items
}

func nativeResumePickerItems(candidates []nativeResumeCandidate, sessionIDs []string) []sessionPickerItem {
	items := make([]sessionPickerItem, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		item := sessionPickerItem{id: sessionID, title: "Untitled session"}
		agents := make([]string, 0)
		for _, candidate := range candidates {
			if candidate.Group.SessionID != sessionID {
				continue
			}
			if candidate.Group.SessionDescriptor != nil && item.title == "Untitled session" {
				item.title = pickerItemTitle(candidate.Group.SessionDescriptor.Title)
			}
			if item.title == "Untitled session" && candidate.Summary.Title != "" {
				item.title = pickerItemTitle(candidate.Summary.Title)
			}
			agents = appendUnique(agents, candidate.Replica.Descriptor.Source.Agent)
			item.updatedAt = pickerSessionTime(item.updatedAt, candidate.Summary.CreatedAt)
			item.updatedAt = pickerSessionTime(item.updatedAt, candidate.Summary.UpdatedAt)
			item.updatedAt = pickerSessionTime(item.updatedAt, candidate.Replica.Descriptor.CreatedAt)
			if candidate.Group.SessionDescriptor != nil {
				item.updatedAt = pickerSessionTime(item.updatedAt, candidate.Group.SessionDescriptor.CreatedAt)
			}
			if candidate.Replica.Tip != nil {
				item.updatedAt = pickerSessionTime(item.updatedAt, candidate.Replica.Tip.UpdatedAt)
				if candidate.Replica.Tip.RecordCount > item.records {
					item.records = candidate.Replica.Tip.RecordCount
				}
			}
		}
		sort.Strings(agents)
		item.agent = strings.Join(agents, ",")
		items = append(items, item)
	}
	sortSessionPickerItems(items)
	return items
}

func pickerLegacyRecordCount(group syncer.ProjectMetadataRef) uint64 {
	var count uint64
	for _, device := range group.Devices {
		if device.Metadata.RecordCount > count {
			count = device.Metadata.RecordCount
		}
	}
	return count
}

func pickerSessionTime(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.After(current) {
		return candidate
	}
	return current
}

func sortSessionPickerItems(items []sessionPickerItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].updatedAt.Equal(items[j].updatedAt) {
			if strings.EqualFold(items[i].title, items[j].title) {
				return items[i].id < items[j].id
			}
			return strings.ToLower(items[i].title) < strings.ToLower(items[j].title)
		}
		return items[i].updatedAt.After(items[j].updatedAt)
	})
}
