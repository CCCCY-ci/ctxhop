package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
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
		"/Users/bob/Projects/Example":               "-Users-bob-Projects-Example",

		// Every non-alphanumeric character becomes a dash, not just the
		// separators. Getting this wrong points us at a directory the agent
		// never reads, and both resulting failures - nothing backed up, a
		// restore the agent cannot see - are silent.
		`D:\Work\my_app`: "D--Work-my-app",
		`D:\Work\my.app`: "D--Work-my-app",
		`D:\Work\my app`: "D--Work-my-app",

		// A trailing separator is part of the path, not trimmed away.
		`D:\Work\Example\`: "D--Work-Example-",

		// Observed directly from the agent: a path mixing an underscore, a
		// space, a dot and two CJK characters, each becoming one dash.
		`D:\CodeWorkSpace\poc_slug test.v2_名字`: "D--CodeWorkSpace-poc-slug-test-v2---",
	}
	for in, want := range tests {
		if got := EncodeProjectSlug(in); got != want {
			t.Errorf("EncodeProjectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeProjectSlugHashesLongPaths(t *testing.T) {
	long := `D:\` + strings.Repeat("segment\\", 40) + "end"
	slug := EncodeProjectSlug(long)

	if len(slug) <= slugMaxLen {
		t.Fatalf("expected a truncated slug, got %d characters", len(slug))
	}
	if got := slug[:slugMaxLen]; strings.ContainsAny(got, `:\/.`) {
		t.Errorf("prefix still contains raw path characters: %q", got)
	}
	if slug[slugMaxLen] != '-' {
		t.Errorf("expected a dash before the hash, got %q", slug[slugMaxLen])
	}

	// The hash distinguishes paths that share their first 200 characters, which
	// is the entire reason it exists.
	other := EncodeProjectSlug(long + "x")
	if slug == other {
		t.Error("paths sharing a truncated prefix produced the same slug")
	}
	if slug[:slugMaxLen] != other[:slugMaxLen] {
		t.Error("expected the truncated prefixes to match")
	}
}

func TestSlugHashMatchesTheAgentsArithmetic(t *testing.T) {
	// hash*31 + code unit, wrapping at 32 bits, rendered in base 36. Spot
	// values computed from the same definition the agent uses.
	tests := map[string]string{
		"":  "0",
		"a": "2p",
		"D": "1w",
	}
	for in, want := range tests {
		if got := slugHash(utf16.Encode([]rune(in))); got != want {
			t.Errorf("slugHash(%q) = %q, want %q", in, got, want)
		}
	}

	// Inputs whose accumulated hash goes negative must come out as the absolute
	// value, matching Math.abs, and never carry a minus sign into a directory
	// name. Values cross-checked against the agent's own arithmetic.
	negative := map[string]string{
		"abcdefghij": "ahnmmz",
		"zzzzzzzzzz": "q59vb4",
	}
	for in, want := range negative {
		got := slugHash(utf16.Encode([]rune(in)))
		if got != want {
			t.Errorf("slugHash(%q) = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "-") {
			t.Errorf("slugHash(%q) leaked a sign: %q", in, got)
		}
	}

	// A long input must still produce a value, i.e. the wraparound does not
	// blow up.
	if got := slugHash(utf16.Encode([]rune(strings.Repeat("x", 500)))); got == "" {
		t.Error("expected a hash for a long path")
	}
}

func TestSameProject(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`D:\Work\Example`, `D:\Work\Example`, true},
		{`D:\Work\Example`, `d:/work/example`, true},
		{`D:\Work\Example\`, `D:\Work\Example`, true},
		{`D:\Work\Example`, `D:\Work\Ex`, false},
		{`D:\Work\Example`, `D:\Work\Example2`, false},
		{"/Users/bob/x", "/Users/bob/x", true},
		{"/Users/bob/x", "/users/bob/x", false},
	}
	for _, tt := range tests {
		if got := sameProject(tt.a, tt.b); got != tt.want {
			t.Errorf("sameProject(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
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

func TestDiscoverSessionsSkipsAnotherProjectsSessions(t *testing.T) {
	// `my_app` and `my.app` encode to the same slug, so their sessions share a
	// directory. The session's own cwd decides which project it belongs to;
	// without that check the wrong project's sessions get pushed, and a restore
	// lands in the wrong place.
	home := t.TempDir()
	l := Layout{Home: home}
	const mine = `D:\Work\my_app`
	const theirs = `D:\Work\my.app`

	if EncodeProjectSlug(mine) != EncodeProjectSlug(theirs) {
		t.Fatal("expected these paths to collide; the test is not exercising anything")
	}

	dir := l.SessionDir(mine)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, cwd string) {
		t.Helper()
		rec := `{"type":"user","cwd":"` + strings.ReplaceAll(cwd, `\`, `\\`) + `","message":{"content":"hi"}}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("mine.jsonl", mine)
	write("theirs.jsonl", theirs)

	refs, err := l.DiscoverSessions(mine)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d sessions, want 1: %+v", len(refs), refs)
	}
	if refs[0].NativeID != "mine" {
		t.Errorf("listed the wrong project's session: %s", refs[0].NativeID)
	}
}

func TestDiscoverSessionsKeepsPartlyDamagedSessions(t *testing.T) {
	// A kill during a write leaves a partial line, and the agent then appends
	// after it - producing a malformed record in the middle. Dropping the whole
	// session would make it disappear from the user's view and never be backed
	// up, so listing tolerates it while anything that pushes stays strict.
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Damaged`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"ai-title","aiTitle":"Still visible"}` + "\n" +
		`{"type":"user","message":{"content":"tru` + "\n" +
		`{"type":"user","message":{"content":"after the damage"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "x.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	refs, err := l.DiscoverSessions(root)
	if err != nil {
		t.Fatalf("DiscoverSessions: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("a damaged session vanished from the listing: %+v", refs)
	}
	if refs[0].Title != "Still visible" {
		t.Errorf("title = %q", refs[0].Title)
	}

	// The strict read still refuses the same file, because pushing it would
	// make the damage permanent.
	if _, err := ReadSessionFile(filepath.Join(dir, "x.jsonl")); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("strict read should refuse damaged content, got %v", err)
	}
}

func TestReadRecordsLenientCountsWhatItSkipped(t *testing.T) {
	data, err := ReadRecordsLenient(strings.NewReader("{\"a\":1}\nbroken\n{\"b\":2}\nalso broken\n"))
	if err != nil {
		t.Fatalf("ReadRecordsLenient: %v", err)
	}
	if len(data.Records) != 2 {
		t.Errorf("got %d records, want 2", len(data.Records))
	}
	if data.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2", data.Skipped)
	}
}

func TestDiscoverSessionsReportsAnUnreadableDirectory(t *testing.T) {
	// A missing directory means "no sessions yet" and is normal. Anything else
	// is a real problem and must not be reported as an empty project, or the
	// user would be told their sessions are safe when they were never seen.
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Blocked`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the session directory belongs.
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := l.DiscoverSessions(root); err == nil {
		t.Fatal("expected an error, got none")
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
