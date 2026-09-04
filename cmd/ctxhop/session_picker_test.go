package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPickerMatchesQueryByTitleAgentAndFuzzyText(t *testing.T) {
	items := []sessionPickerItem{
		{id: "one", title: "Storage boundary review", agent: "codex"},
		{id: "two", title: "Remote index refresh", agent: "claude-code"},
		{id: "three", title: "Session Hub migration", agent: "codex"},
	}
	cases := []struct {
		query string
		want  []string
	}{
		{query: "storage", want: []string{"one"}},
		{query: "claude", want: []string{"two"}},
		{query: "shm", want: []string{"three"}},
		{query: "TWO", want: []string{"two"}},
		{query: "missing", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			picker := &sessionPicker{items: items, query: tc.query}
			matches := picker.matches()
			got := make([]string, 0, len(matches))
			for _, index := range matches {
				got = append(got, items[index].id)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPickerRanksTitleMatchesAheadOfFuzzyDetails(t *testing.T) {
	picker := &sessionPicker{items: []sessionPickerItem{
		{id: "projects", title: "Projects and policies", detail: "Bind projects and choose sync behavior"},
		{id: "devices", title: "Devices", detail: "Connected devices and local identity"},
	}}
	picker.query = "dev"
	matches := picker.matches()
	if len(matches) != 2 || picker.items[matches[0]].id != "devices" {
		t.Fatalf("ranked matches = %v, want devices first", matches)
	}
}

func TestPickerNavigationUsesFilteredRows(t *testing.T) {
	picker := &sessionPicker{items: []sessionPickerItem{
		{id: "codex", title: "Build API", agent: "codex"},
		{id: "claude", title: "Review API", agent: "claude-code"},
		{id: "other", title: "Update docs", agent: "codex"},
	}}

	if _, done, err := picker.handle(pickerKey{kind: pickerKeyRune, rune: 'a'}); err != nil || done {
		t.Fatalf("filter key = done %t, err %v", done, err)
	}
	if picker.query != "a" || picker.selected != 0 {
		t.Fatalf("picker after filter = query %q selected %d", picker.query, picker.selected)
	}
	if _, done, err := picker.handle(pickerKey{kind: pickerKeyDown}); err != nil || done {
		t.Fatalf("navigation key = done %t, err %v", done, err)
	}
	selected, done, err := picker.handle(pickerKey{kind: pickerKeyEnter})
	if err != nil || !done || selected != "claude" {
		t.Fatalf("selected = %q, done %t, err %v; want claude", selected, done, err)
	}
}

func TestPickerBackspaceAndCancel(t *testing.T) {
	picker := &sessionPicker{items: []sessionPickerItem{{id: "one", title: "One"}}}
	picker.query = "中文"
	if _, _, err := picker.handle(pickerKey{kind: pickerKeyBackspace}); err != nil {
		t.Fatal(err)
	}
	if picker.query != "中" {
		t.Fatalf("query after backspace = %q, want 中", picker.query)
	}
	if _, _, err := picker.handle(pickerKey{kind: pickerKeyEscape}); !errors.Is(err, errSessionPickerCancelled) {
		t.Fatalf("cancel error = %v, want %v", err, errSessionPickerCancelled)
	}
}

func TestPickerViewportAndRows(t *testing.T) {
	if start, end := pickerViewport(0, 20, 5); start != 0 || end != 5 {
		t.Fatalf("first viewport = %d:%d, want 0:5", start, end)
	}
	if start, end := pickerViewport(10, 20, 5); start != 8 || end != 13 {
		t.Fatalf("middle viewport = %d:%d, want 8:13", start, end)
	}
	if start, end := pickerViewport(19, 20, 5); start != 15 || end != 20 {
		t.Fatalf("last viewport = %d:%d, want 15:20", start, end)
	}

	line := pickerItemLine(sessionPickerItem{
		id:        "remote-secret-id",
		title:     "Session title",
		agent:     "codex",
		updatedAt: time.Now().Add(-2 * time.Hour),
		records:   4,
	}, 80, false)
	if !strings.Contains(line, "Session title") || !strings.Contains(line, "Codex") || !strings.Contains(line, "4 records") {
		t.Fatalf("picker row = %q", line)
	}
	if strings.Contains(line, "remote-secret-id") {
		t.Fatalf("picker row exposes an ID: %q", line)
	}
}

func TestInteractivePickerRendersConfiguredHeadingAndDetails(t *testing.T) {
	picker := &sessionPicker{
		items: []sessionPickerItem{{id: "sync", title: "Sync workspace", detail: "Push sessions and workspace context"}},
		options: interactivePickerOptions{
			errorPrefix:  "ctxhop",
			heading:      "CtxHop",
			help:         "Enter open  |  Esc quit",
			itemNoun:     "action",
			emptyMessage: "No matching actions.",
		},
	}
	lines := picker.lines()
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"CtxHop", "Enter open  |  Esc quit", "Sync workspace", "Push sessions and workspace context", "1 action"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("picker lines do not contain %q: %q", want, joined)
		}
	}
}

func TestPickerRequiresTerminalForInteractiveUse(t *testing.T) {
	_, err := runSessionPicker(strings.NewReader("\n"), &strings.Builder{}, []sessionPickerItem{{id: "one", title: "One"}})
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("runSessionPicker error = %v, want terminal guidance", err)
	}
}

func TestInteractiveHistorySourcePrefersLegacySource(t *testing.T) {
	legacy := sessionSourceEntry{Agent: "codex", legacyID: "legacy-session"}
	native := sessionSourceEntry{Agent: "codex", NativeID: "native-session", ReplicaID: "replica", Complete: true}
	selected := interactiveHistorySessionSource([]sessionSourceEntry{native, legacy})
	if selected.legacyID != legacy.legacyID || selected.ReplicaID != "" {
		t.Fatalf("history source = %#v, want legacy source", selected)
	}
}

func TestInteractiveDefaultSourcePrefersCompleteNativeReplica(t *testing.T) {
	incomplete := sessionSourceEntry{Agent: "codex", NativeID: "old", ReplicaID: "old-replica"}
	complete := sessionSourceEntry{Agent: "claude-code", NativeID: "new", ReplicaID: "new-replica", Complete: true}
	selected := interactiveDefaultSessionSource([]sessionSourceEntry{incomplete, complete})
	if selected.ReplicaID != complete.ReplicaID {
		t.Fatalf("default source = %#v, want complete Replica", selected)
	}
}

func TestReadInteractiveLineDoesNotDiscardFollowingLine(t *testing.T) {
	input := strings.NewReader("yes\nnext\n")
	var output strings.Builder
	first, err := readInteractiveLine(input, &output, "first: ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := readInteractiveLine(input, &output, "second: ")
	if err != nil {
		t.Fatal(err)
	}
	if first != "yes" || second != "next" {
		t.Fatalf("lines = %q, %q; want yes, next", first, second)
	}
}
