package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadRecordsKeepsOnlyFinishedRecords(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantRecords int
		wantDropped bool
	}{
		{
			name:        "well formed session",
			in:          "{\"a\":1}\n{\"a\":2}\n",
			wantRecords: 2,
		},
		{
			name:        "truncated tail is dropped",
			in:          "{\"a\":1}\n{\"a\":2}\n{\"a\":",
			wantRecords: 2,
			wantDropped: true,
		},
		{
			name: "a complete record without its newline is not trusted yet",
			// The bytes parse, but the writer may not be finished. Shards are
			// immutable, so waiting one round is cheaper than being wrong.
			in:          "{\"a\":1}\n{\"a\":2}",
			wantRecords: 1,
			wantDropped: true,
		},
		{
			name:        "blank lines are ignored",
			in:          "{\"a\":1}\n\n\n{\"a\":2}\n",
			wantRecords: 2,
		},
		{
			name:        "trailing blank line is not a dropped tail",
			in:          "{\"a\":1}\n\n",
			wantRecords: 1,
		},
		{
			name:        "carriage returns are tolerated",
			in:          "{\"a\":1}\r\n{\"a\":2}\r\n",
			wantRecords: 2,
		},
		{
			name:        "empty input",
			in:          "",
			wantRecords: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReadRecords(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("ReadRecords: %v", err)
			}
			if len(got.Records) != tt.wantRecords {
				t.Errorf("got %d records, want %d", len(got.Records), tt.wantRecords)
			}
			if got.DroppedTail != tt.wantDropped {
				t.Errorf("DroppedTail = %v, want %v", got.DroppedTail, tt.wantDropped)
			}
		})
	}
}

func TestReadRecordsRejectsCorruptionInTheMiddle(t *testing.T) {
	// A terminated record that does not parse was fully written, so it is
	// corruption rather than a write in progress and must not pass silently.
	_, err := ReadRecords(strings.NewReader("{\"a\":1}\nnot json\n{\"a\":2}\n"))
	if !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("got %v, want ErrCorruptSession", err)
	}
}

// erroringReader yields some data and then fails, standing in for a file on a
// disconnected drive or a filesystem error partway through a read.
type erroringReader struct {
	data []byte
	done bool
}

func (e *erroringReader) Read(p []byte) (int, error) {
	if e.done {
		return 0, errors.New("input/output error")
	}
	e.done = true
	return copy(p, e.data), nil
}

func TestReadRecordsPropagatesReadFailures(t *testing.T) {
	// A read failure is not the same as a short session: we must not quietly
	// treat whatever arrived as the whole file, because the sync layer would
	// then push a truncated session as if it were complete.
	_, err := ReadRecords(&erroringReader{data: []byte("{\"a\":1}\n")})
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "read session") {
		t.Errorf("error should name the failing step, got %v", err)
	}
}

func TestReadSessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(path, []byte("{\"a\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, err := ReadSessionFile(path)
	if err != nil {
		t.Fatalf("ReadSessionFile: %v", err)
	}
	if len(data.Records) != 1 {
		t.Errorf("got %d records, want 1", len(data.Records))
	}

	if _, err := ReadSessionFile(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestEncodeProjectSlug(t *testing.T) {
	tests := map[string]string{
		`D:\CodeWorkSpace\VSCodeProjects\AgentSync`: "D--CodeWorkSpace-VSCodeProjects-AgentSync",
		`D:\CodeWorkSpace\Example\`:                 "D--CodeWorkSpace-Example",
		"/Users/bob/Projects/Example":               "-Users-bob-Projects-Example",
		`D:/CodeWorkSpace/Example`:                  "D--CodeWorkSpace-Example",
	}
	for in, want := range tests {
		if got := EncodeProjectSlug(in); got != want {
			t.Errorf("EncodeProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLayoutPaths(t *testing.T) {
	l := Layout{Home: filepath.Join("home", ".claude")}

	wantDir := filepath.Join("home", ".claude", "projects", "D--Example")
	if got := l.SessionDir(`D:\Example`); got != wantDir {
		t.Errorf("SessionDir = %q, want %q", got, wantDir)
	}

	wantFile := filepath.Join(wantDir, "abc.jsonl")
	if got := l.SessionFile(`D:\Example`, "abc"); got != wantFile {
		t.Errorf("SessionFile = %q, want %q", got, wantFile)
	}
}

func TestSummarizeTitlePriority(t *testing.T) {
	const root = `D:\Work\Example`

	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{
			name: "agent title wins",
			records: []string{
				`{"type":"user","isMeta":false,"message":{"content":"do the thing"}}`,
				`{"type":"ai-title","aiTitle":"Refactor the parser"}`,
			},
			want: "Refactor the parser",
		},
		{
			name: "later agent title supersedes earlier",
			records: []string{
				`{"type":"ai-title","aiTitle":"First guess"}`,
				`{"type":"ai-title","aiTitle":"Better title"}`,
			},
			want: "Better title",
		},
		{
			name: "falls back to the opening prompt",
			records: []string{
				`{"type":"user","message":{"content":"fix the flaky test"}}`,
			},
			want: "fix the flaky test",
		},
		{
			name: "meta messages are not the opening prompt",
			records: []string{
				`{"type":"user","isMeta":true,"message":{"content":"session bootstrap"}}`,
				`{"type":"user","message":{"content":"the real question"}}`,
			},
			want: "the real question",
		},
		{
			name: "block-structured content is understood",
			records: []string{
				`{"type":"user","message":{"content":[{"type":"text","text":"from a block"}]}}`,
			},
			want: "from a block",
		},
		{
			name: "whitespace is collapsed",
			records: []string{
				`{"type":"user","message":{"content":"line one\n\n   line two"}}`,
			},
			want: "line one line two",
		},
		{
			name:    "no usable text falls back to project and time",
			records: []string{`{"type":"system"}`},
			want:    "Example 2026-08-10 09:00",
		},
		{
			name: "unparseable records are skipped, not fatal",
			records: []string{
				`{"type":"ai-title","aiTitle":`,
				`{"type":"ai-title","aiTitle":"Survived"}`,
			},
			want: "Survived",
		},
	}

	fallback := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records := make([][]byte, len(tt.records))
			for i, r := range tt.records {
				records[i] = []byte(r)
			}
			got := summarize(records).Title(root, fallback)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTitleTruncatesOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("字", 200)
	records := [][]byte{[]byte(`{"type":"ai-title","aiTitle":"` + long + `"}`)}

	got := summarize(records).Title(`D:\Work\Example`, time.Now())
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected an ellipsis, got %q", got)
	}
	if r := []rune(got); len(r) != maxTitleLen {
		t.Errorf("got %d runes, want %d", len(r), maxTitleLen)
	}
	if !strings.ContainsRune(got, '字') || strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("truncation split a rune: %q", got)
	}
}

func TestTitleKeepsShortMultibyteTitlesWhole(t *testing.T) {
	// 30 CJK characters are 90 bytes but well under the rune limit. A length
	// check that looked only at bytes would truncate a title that is actually
	// short, so both measures have to agree before anything is cut.
	title := strings.Repeat("字", 30)
	records := [][]byte{[]byte(`{"type":"ai-title","aiTitle":"` + title + `"}`)}

	got := summarize(records).Title(`D:\Work\Example`, time.Now())
	if got != title {
		t.Errorf("short multibyte title was altered:\n got  %q\n want %q", got, title)
	}
}

func TestPromptTextToleratesOddShapes(t *testing.T) {
	tests := []string{
		`{"type":"user"}`,
		`{"type":"user","message":"not an object"}`,
		`{"type":"user","message":{"content":42}}`,
		`{"type":"user","message":{"content":[]}}`,
		`{"type":"user","message":{"content":["not an object"]}}`,
		`{"type":"user","message":{"content":[{"type":"image"}]}}`,
	}

	for _, rec := range tests {
		t.Run(rec, func(t *testing.T) {
			// None of these carry usable text, so the title must fall through
			// to the project-and-time form rather than panicking or returning
			// something malformed.
			got := summarize([][]byte{[]byte(rec)}).Title(`D:\Work\Example`,
				time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC))
			if got != "Example 2026-01-02 03:04" {
				t.Errorf("got %q", got)
			}
		})
	}
}

func TestSummarizeTimestampsAndCwd(t *testing.T) {
	records := [][]byte{
		[]byte(`{"type":"user","timestamp":"2026-08-10T12:00:00Z","cwd":"D:\\Work\\Example","version":"2.1.0"}`),
		[]byte(`{"type":"assistant","timestamp":"2026-08-10T09:30:00Z"}`),
		[]byte(`{"type":"assistant","timestamp":"2026-08-10T15:45:00Z","version":"2.1.1"}`),
		[]byte(`{"type":"assistant","timestamp":"not a time"}`),
	}

	s := summarize(records)
	if want := "2026-08-10T09:30:00Z"; s.created.Format(time.RFC3339) != want {
		t.Errorf("created = %v, want %s", s.created, want)
	}
	if want := "2026-08-10T15:45:00Z"; s.updated.Format(time.RFC3339) != want {
		t.Errorf("updated = %v, want %s", s.updated, want)
	}
	if s.cwd != `D:\Work\Example` {
		t.Errorf("cwd = %q", s.cwd)
	}
	if s.version != "2.1.1" {
		t.Errorf("version = %q, want the last one seen", s.version)
	}
}

func TestDiscoverSessions(t *testing.T) {
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Example`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("aaa.jsonl", `{"type":"ai-title","aiTitle":"First session","timestamp":"2026-08-10T10:00:00Z"}`+"\n")
	write("bbb.jsonl", `{"type":"user","message":{"content":"second"},"timestamp":"2026-08-10T11:00:00Z"}`+"\n")
	// Neither of these should appear in the listing.
	write("notes.txt", "ignored")
	write("broken.jsonl", "garbage\n")
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}

	refs, err := l.DiscoverSessions(root)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(refs), refs)
	}

	byID := map[string]SessionRef{}
	for _, r := range refs {
		byID[r.NativeID] = r
	}
	if got := byID["aaa"].Title; got != "First session" {
		t.Errorf("title = %q", got)
	}
	if got := byID["bbb"].Title; got != "second" {
		t.Errorf("title = %q", got)
	}
	if byID["aaa"].ProjectPath != root {
		t.Errorf("project = %q", byID["aaa"].ProjectPath)
	}
	if byID["aaa"].Size == 0 {
		t.Error("size not populated")
	}
	if byID["aaa"].CreatedAt.IsZero() || byID["aaa"].UpdatedAt.IsZero() {
		t.Error("timestamps not populated")
	}
}

func TestDiscoverSessionsMissingProjectIsNotAnError(t *testing.T) {
	l := Layout{Home: t.TempDir()}

	refs, err := l.DiscoverSessions(`D:\Never\Used`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if refs != nil {
		t.Errorf("expected no sessions, got %v", refs)
	}
}

func TestDiscoverSessionsFallsBackToFileTimes(t *testing.T) {
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Timeless`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No timestamps anywhere in the records.
	if err := os.WriteFile(filepath.Join(dir, "x.jsonl"), []byte(`{"type":"system"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	refs, err := l.DiscoverSessions(root)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d sessions, want 1", len(refs))
	}
	if refs[0].CreatedAt.IsZero() || refs[0].UpdatedAt.IsZero() {
		t.Error("expected file times to stand in for missing timestamps")
	}
	if !strings.HasPrefix(refs[0].Title, "Timeless ") {
		t.Errorf("title = %q, want the project-and-time fallback", refs[0].Title)
	}
}

func FuzzReadRecords(f *testing.F) {
	f.Add("{\"a\":1}\n")
	f.Add("{\"a\":1}\n{\"b\":")
	f.Add("\n\n\n")
	f.Add("garbage\n")

	f.Fuzz(func(t *testing.T, raw string) {
		data, err := ReadRecords(strings.NewReader(raw))
		if err != nil {
			return
		}
		// Anything returned must be a complete record, and summarising it must
		// never panic however strange the content.
		for _, rec := range data.Records {
			if len(rec) == 0 {
				t.Fatalf("returned an empty record from %q", raw)
			}
		}
		_ = summarize(data.Records).Title(`D:\X`, time.Unix(0, 0))
	})
}
