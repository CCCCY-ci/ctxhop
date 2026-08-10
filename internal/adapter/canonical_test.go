package adapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// All fixtures here are synthetic. Real session bodies contain source code,
// terminal output and credentials, and this is a public repository.
var (
	winSpace = PathSpace{
		ProjectRoot: `D:\CodeWorkSpace\Example`,
		AgentHome:   `C:\Users\alice\.claude`,
	}
	posixSpace = PathSpace{
		ProjectRoot: "/Users/bob/Projects/Example",
		AgentHome:   "/Users/bob/.claude",
	}
)

func canonical(t *testing.T, space PathSpace, raw string) string {
	t.Helper()
	out, err := NewCanonicalizer(space).Record([]byte(raw))
	if err != nil {
		t.Fatalf("Canonicalize(%s): %v", raw, err)
	}
	return string(out)
}

func TestCanonicalizeTokenizesPaths(t *testing.T) {
	tests := []struct {
		name  string
		space PathSpace
		in    string
		want  string
	}{
		{
			name:  "cwd equal to project root becomes a bare token",
			space: winSpace,
			in:    `{"type":"user","cwd":"D:\\CodeWorkSpace\\Example"}`,
			want:  `{"cwd":"${AS_PROJECT}","type":"user"}`,
		},
		{
			name:  "path under the project keeps a slash-separated remainder",
			space: winSpace,
			in:    `{"file_path":"D:\\CodeWorkSpace\\Example\\src\\a.go"}`,
			want:  `{"file_path":"${AS_PROJECT}/src/a.go"}`,
		},
		{
			name:  "posix project path",
			space: posixSpace,
			in:    `{"cwd":"/Users/bob/Projects/Example/src"}`,
			want:  `{"cwd":"${AS_PROJECT}/src"}`,
		},
		{
			name:  "agent home is tokenized separately",
			space: winSpace,
			in:    `{"realParentDir":"C:\\Users\\alice\\.claude\\backups"}`,
			want:  `{"realParentDir":"${AS_AGENT_HOME}/backups"}`,
		},
		{
			name:  "allowlisted field may hold an agent home path",
			space: winSpace,
			in:    `{"file_path":"C:\\Users\\alice\\.claude\\skills\\x.md"}`,
			want:  `{"file_path":"${AS_AGENT_HOME}/skills/x.md"}`,
		},
		{
			name:  "prefix match is case insensitive",
			space: winSpace,
			in:    `{"cwd":"d:\\codeworkspace\\example\\src"}`,
			want:  `{"cwd":"${AS_PROJECT}/src"}`,
		},
		{
			name:  "windows separators are interchangeable",
			space: winSpace,
			in:    `{"cwd":"D:/CodeWorkSpace/Example/src"}`,
			want:  `{"cwd":"${AS_PROJECT}/src"}`,
		},
		{
			name:  "posix matching stays case sensitive",
			space: posixSpace,
			in:    `{"cwd":"/users/bob/projects/Example"}`,
			want:  `{"cwd":"/users/bob/projects/Example"}`,
		},
		{
			name:  "path with spaces and non-ascii",
			space: posixSpace,
			in:    `{"file_path":"/Users/bob/Projects/Example/my dir/文件.go"}`,
			want:  `{"file_path":"${AS_PROJECT}/my dir/文件.go"}`,
		},
		{
			name:  "path outside both roots is left verbatim",
			space: winSpace,
			in:    `{"file_path":"E:\\elsewhere\\x.go"}`,
			want:  `{"file_path":"E:\\elsewhere\\x.go"}`,
		},
		{
			name:  "sibling directory sharing a prefix is not matched",
			space: winSpace,
			in:    `{"cwd":"D:\\CodeWorkSpace\\Example2\\src"}`,
			want:  `{"cwd":"D:\\CodeWorkSpace\\Example2\\src"}`,
		},
		{
			name:  "non-allowlisted field is never rewritten",
			space: winSpace,
			in:    `{"text":"I edited D:\\CodeWorkSpace\\Example\\src\\a.go for you"}`,
			want:  `{"text":"I edited D:\\CodeWorkSpace\\Example\\src\\a.go for you"}`,
		},
		{
			name:  "nested arrays inherit the field name",
			space: winSpace,
			in:    `{"message":{"content":[{"input":{"file_path":"D:\\CodeWorkSpace\\Example\\a.go"}}]}}`,
			want:  `{"message":{"content":[{"input":{"file_path":"${AS_PROJECT}/a.go"}}]}}`,
		},
		{
			name:  "path-keyed container rewrites its keys",
			space: winSpace,
			in:    `{"trackedFileBackups":{"C:\\Users\\alice\\.claude\\x.md":{"realParentDir":"C:\\Users\\alice\\.claude"}}}`,
			want:  `{"trackedFileBackups":{"${AS_AGENT_HOME}/x.md":{"realParentDir":"${AS_AGENT_HOME}"}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonical(t, tt.space, tt.in); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

// TestCanonicalizeIsDeterministic is the test the whole storage design rests
// on. Prefix comparison across devices is only meaningful if the same logical
// record produces identical bytes regardless of how it was written.
func TestCanonicalizeIsDeterministic(t *testing.T) {
	variants := []string{
		`{"cwd":"D:\\CodeWorkSpace\\Example","type":"user","n":1}`,
		`{"type":"user","n":1,"cwd":"D:\\CodeWorkSpace\\Example"}`,
		`{  "n" : 1 ,  "type" : "user" ,  "cwd" : "D:\\CodeWorkSpace\\Example"  }`,
		`{"cwd":"D:/CodeWorkSpace/Example","n":1,"type":"user"}`,
	}

	want := canonical(t, winSpace, variants[0])
	for _, v := range variants[1:] {
		if got := canonical(t, winSpace, v); got != want {
			t.Errorf("variant produced different bytes\n got %s\nwant %s", got, want)
		}
	}
}

// TestCanonicalizeAcrossMachinesAgrees is the same property stated across
// platforms: two machines holding the same logical record must agree.
func TestCanonicalizeAcrossMachinesAgrees(t *testing.T) {
	fromWindows := canonical(t, winSpace,
		`{"cwd":"D:\\CodeWorkSpace\\Example","file_path":"D:\\CodeWorkSpace\\Example\\src\\a.go"}`)
	fromPosix := canonical(t, posixSpace,
		`{"cwd":"/Users/bob/Projects/Example","file_path":"/Users/bob/Projects/Example/src/a.go"}`)

	if fromWindows != fromPosix {
		t.Errorf("machines disagree\n windows %s\n posix   %s", fromWindows, fromPosix)
	}
}

func TestCanonicalizePreservesNumbers(t *testing.T) {
	// Decoding through float64 would rewrite these and change the bytes.
	in := `{"a":1000000,"b":9007199254740993,"c":0.1,"d":1e6,"e":-0}`
	want := `{"a":1000000,"b":9007199254740993,"c":0.1,"d":1e6,"e":-0}`

	if got := canonical(t, winSpace, in); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestCanonicalizeDoesNotEscapeHTML(t *testing.T) {
	in := `{"text":"a < b && c > d"}`
	want := `{"text":"a < b && c > d"}`

	if got := canonical(t, winSpace, in); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestCanonicalizeReportsUnknownPathFields(t *testing.T) {
	c := NewCanonicalizer(winSpace)

	records := []string{
		`{"someNewField":"D:\\CodeWorkSpace\\Example\\src\\a.go"}`,
		`{"cwd":"D:\\CodeWorkSpace\\Example"}`,
		`{"text":"mentions D:\\CodeWorkSpace\\Example mid-sentence"}`,
		`{"otherContainer":{"C:\\Users\\alice\\.claude\\x":1}}`,
	}
	for _, r := range records {
		if _, err := c.Record([]byte(r)); err != nil {
			t.Fatalf("Record(%s): %v", r, err)
		}
	}

	want := []string{"otherContainer.<key>", "someNewField"}
	if got := c.UnknownPathFields(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestCanonicalizeRejectsInvalidJSON(t *testing.T) {
	if _, err := NewCanonicalizer(winSpace).Record([]byte(`{"a":`)); err == nil {
		t.Fatal("expected an error for truncated input")
	}
}

func TestLocalize(t *testing.T) {
	tests := []struct {
		name  string
		space PathSpace
		in    string
		want  string
	}{
		{
			name:  "bare token becomes the root",
			space: winSpace,
			in:    `{"cwd":"${AS_PROJECT}"}`,
			want:  `{"cwd":"D:\\CodeWorkSpace\\Example"}`,
		},
		{
			name:  "remainder takes the target separator",
			space: winSpace,
			in:    `{"file_path":"${AS_PROJECT}/src/a.go"}`,
			want:  `{"file_path":"D:\\CodeWorkSpace\\Example\\src\\a.go"}`,
		},
		{
			name:  "posix target keeps forward slashes",
			space: posixSpace,
			in:    `{"file_path":"${AS_PROJECT}/src/a.go"}`,
			want:  `{"file_path":"/Users/bob/Projects/Example/src/a.go"}`,
		},
		{
			name:  "agent home token",
			space: posixSpace,
			in:    `{"realParentDir":"${AS_AGENT_HOME}/backups"}`,
			want:  `{"realParentDir":"/Users/bob/.claude/backups"}`,
		},
		{
			name:  "container keys are localized too",
			space: posixSpace,
			in:    `{"trackedFileBackups":{"${AS_AGENT_HOME}/x.md":1}}`,
			want:  `{"trackedFileBackups":{"/Users/bob/.claude/x.md":1}}`,
		},
		{
			name:  "untokenized values are untouched",
			space: posixSpace,
			in:    `{"text":"I edited D:\\CodeWorkSpace\\Example\\a.go"}`,
			want:  `{"text":"I edited D:\\CodeWorkSpace\\Example\\a.go"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Localize([]byte(tt.in), tt.space)
			if err != nil {
				t.Fatalf("Localize: %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("got  %s\nwant %s", out, tt.want)
			}
		})
	}
}

func TestLocalizeRefusesSuspiciousTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"token mid-value", `{"cwd":"prefix${AS_PROJECT}/src"}`},
		{"token in a field we never tokenize", `{"text":"${AS_PROJECT}/src"}`},
		{"token in a key of a plain object", `{"other":{"${AS_PROJECT}/x":1}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Localize([]byte(tt.in), winSpace); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestLocalizeRequiresTargetPath(t *testing.T) {
	_, err := Localize([]byte(`{"cwd":"${AS_PROJECT}"}`), PathSpace{})
	if err == nil {
		t.Fatal("expected an error when no target root is configured")
	}
}

func TestLocalizeRejectsInvalidJSON(t *testing.T) {
	if _, err := Localize([]byte("nonsense"), winSpace); err == nil {
		t.Fatal("expected an error for invalid input")
	}
}

// TestRoundTrip checks that a record survives canonicalisation and comes back
// equivalent on the same machine, and that crossing machines only changes the
// paths.
func TestRoundTrip(t *testing.T) {
	original := `{"cwd":"D:\\CodeWorkSpace\\Example","message":{"content":[{"input":{"file_path":"D:\\CodeWorkSpace\\Example\\src\\a.go"}}]},"n":42}`

	canon, err := NewCanonicalizer(winSpace).Record([]byte(original))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	back, err := Localize(canon, winSpace)
	if err != nil {
		t.Fatalf("localize: %v", err)
	}

	if !equivalentJSON(t, original, string(back)) {
		t.Errorf("round trip changed the record\n got %s\nwant %s", back, original)
	}

	// Localised for the other machine, then canonicalised again, it must land
	// on the identical canonical bytes - otherwise devices would fork forever.
	onPosix, err := Localize(canon, posixSpace)
	if err != nil {
		t.Fatalf("localize posix: %v", err)
	}
	again, err := NewCanonicalizer(posixSpace).Record(onPosix)
	if err != nil {
		t.Fatalf("recanonicalize: %v", err)
	}
	if string(again) != string(canon) {
		t.Errorf("canonical form not stable across machines\n got %s\nwant %s", again, canon)
	}
}

func TestSeparatorFor(t *testing.T) {
	tests := map[string]string{
		`D:\a\b`:         `\`,
		`\\server\share`: `\`,
		"/Users/bob":     "/",
		"":               "/",
	}
	for root, want := range tests {
		if got := separatorFor(root); got != want {
			t.Errorf("separatorFor(%q) = %q, want %q", root, got, want)
		}
	}
}

func TestReplaceRootTrailingSeparator(t *testing.T) {
	// A root configured with a trailing separator must behave identically.
	got, ok := replaceRoot(`D:\a\b\c.go`, `D:\a\`, TokenProject)
	if !ok || got != TokenProject+"/b/c.go" {
		t.Errorf("got %q ok=%v", got, ok)
	}
}

func equivalentJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}

func FuzzCanonicalize(f *testing.F) {
	f.Add(`{"cwd":"D:\\CodeWorkSpace\\Example"}`)
	f.Add(`{"a":[1,2,{"file_path":"D:\\CodeWorkSpace\\Example\\x"}]}`)
	f.Add(`{"trackedFileBackups":{"C:\\Users\\alice\\.claude\\x":{}}}`)
	f.Add(`[]`)
	f.Add(`"bare string"`)
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, raw string) {
		c := NewCanonicalizer(winSpace)
		out, err := c.Record([]byte(raw))
		if err != nil {
			return
		}
		// Whatever came out must itself be valid JSON and must canonicalise to
		// the same bytes a second time.
		if !json.Valid(out) {
			t.Fatalf("produced invalid json from %q: %s", raw, out)
		}
		again, err := NewCanonicalizer(winSpace).Record(out)
		if err != nil {
			t.Fatalf("second pass failed for %q: %v", raw, err)
		}
		if string(again) != string(out) {
			t.Fatalf("not idempotent for %q:\n first  %s\n second %s", raw, out, again)
		}
		if strings.Contains(string(out), "\n") {
			t.Fatalf("record contains a newline: %s", out)
		}
	})
}
