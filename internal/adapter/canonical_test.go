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
		ProjectRoot: `D:\Workspace\Example`,
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
			in:    `{"type":"user","cwd":"D:\\Workspace\\Example"}`,
			want:  `{"cwd":"${AS_PROJECT}","type":"user"}`,
		},
		{
			name:  "path under the project keeps a slash-separated remainder",
			space: winSpace,
			in:    `{"file_path":"D:\\Workspace\\Example\\src\\a.go"}`,
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
			in:    `{"cwd":"d:\\workspace\\example\\src"}`,
			want:  `{"cwd":"${AS_PROJECT}/src"}`,
		},
		{
			name:  "windows separators are interchangeable",
			space: winSpace,
			in:    `{"cwd":"D:/Workspace/Example/src"}`,
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
			in:    `{"cwd":"D:\\Workspace\\Example2\\src"}`,
			want:  `{"cwd":"D:\\Workspace\\Example2\\src"}`,
		},
		{
			name:  "non-allowlisted field is never rewritten",
			space: winSpace,
			in:    `{"text":"I edited D:\\Workspace\\Example\\src\\a.go for you"}`,
			want:  `{"text":"I edited D:\\Workspace\\Example\\src\\a.go for you"}`,
		},
		{
			name:  "nested arrays inherit the field name",
			space: winSpace,
			in:    `{"message":{"content":[{"input":{"file_path":"D:\\Workspace\\Example\\a.go"}}]}}`,
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
		`{"cwd":"D:\\Workspace\\Example","type":"user","n":1}`,
		`{"type":"user","n":1,"cwd":"D:\\Workspace\\Example"}`,
		`{  "n" : 1 ,  "type" : "user" ,  "cwd" : "D:\\Workspace\\Example"  }`,
		`{"cwd":"D:/Workspace/Example","n":1,"type":"user"}`,
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
		`{"cwd":"D:\\Workspace\\Example","file_path":"D:\\Workspace\\Example\\src\\a.go"}`)
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

func TestCanonicalizeReportsUnknownPathKeys(t *testing.T) {
	c := NewCanonicalizer(winSpace)

	records := []string{
		`{"someNewField":"D:\\Workspace\\Example\\src\\a.go"}`,
		`{"cwd":"D:\\Workspace\\Example"}`,
		`{"text":"mentions D:\\Workspace\\Example mid-sentence"}`,
		`{"otherContainer":{"C:\\Users\\alice\\.claude\\x":1}}`,
	}
	for _, r := range records {
		if _, err := c.Record([]byte(r)); err != nil {
			t.Fatalf("Record(%s): %v", r, err)
		}
	}

	want := []string{"otherContainer.<key>"}
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
			want:  `{"cwd":"D:\\Workspace\\Example"}`,
		},
		{
			name:  "remainder takes the target separator",
			space: winSpace,
			in:    `{"file_path":"${AS_PROJECT}/src/a.go"}`,
			want:  `{"file_path":"D:\\Workspace\\Example\\src\\a.go"}`,
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
			in:    `{"text":"I edited D:\\Workspace\\Example\\a.go"}`,
			want:  `{"text":"I edited D:\\Workspace\\Example\\a.go"}`,
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

func TestLocalizeRefusesAMisplacedTokenInATokenizedField(t *testing.T) {
	// In a field we do tokenize, a marker anywhere but the start means the
	// record did not come from our canonicaliser.
	if _, err := Localize([]byte(`{"cwd":"prefix${AS_PROJECT}/src"}`), winSpace); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestLocalizeFailsFromInsideNestedStructures(t *testing.T) {
	// A bad value buried in an array must abort the whole record. Localising
	// the rest and dropping this one would write a session that half refers to
	// another machine, which is worse than not restoring at all (BR-10).
	nested := []string{
		`{"message":{"content":[{"input":{"cwd":"pre${AS_PROJECT}/x"}}]}}`,
		`{"a":[[{"file_path":"mid${AS_AGENT_HOME}/y"}]]}`,
	}
	for _, in := range nested {
		t.Run(in, func(t *testing.T) {
			if _, err := Localize([]byte(in), winSpace); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestLocalizeLeavesTokenLikeContentAlone(t *testing.T) {
	// Tokens are only ever written into allowlisted positions, so the same
	// characters elsewhere are user content. Refusing there would make any
	// session that discusses CtxHop itself impossible to restore - including
	// the ones produced while building it.
	tests := []string{
		`{"text":"${AS_PROJECT}/src is the token we use"}`,
		`{"other":{"${AS_PROJECT}/x":1}}`,
		`{"message":{"content":[{"text":"set root to ${AS_AGENT_HOME}"}]}}`,
	}

	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			out, err := Localize([]byte(in), winSpace)
			if err != nil {
				t.Fatalf("Localize: %v", err)
			}
			if !strings.Contains(string(out), "${AS_") {
				t.Errorf("content was rewritten: %s", out)
			}
		})
	}
}

func TestLocalizeRequiresTargetPath(t *testing.T) {
	// Refused up front, not only when a token happens to appear: localising
	// against an unknown root cannot produce a session the agent will resolve.
	if _, err := Localize([]byte(`{"n":1}`), PathSpace{}); err == nil {
		t.Fatal("expected an error when no project root is configured")
	}

	_, err := Localize([]byte(`{"realParentDir":"${AS_AGENT_HOME}/x"}`),
		PathSpace{ProjectRoot: `D:\Work`})
	if err == nil {
		t.Fatal("expected an error when the agent home is needed but unset")
	}
}

func TestLocalizeUsesEachRootsOwnSeparator(t *testing.T) {
	// The agent directory can look nothing like the project path. Deriving one
	// separator from the project alone yields `C:\Users\alice\.claude/backups`.
	mixed := PathSpace{
		ProjectRoot: "/mnt/d/Work/Example",
		AgentHome:   `C:\Users\alice\.claude`,
	}

	out, err := Localize([]byte(`{"cwd":"${AS_PROJECT}/src","realParentDir":"${AS_AGENT_HOME}/backups"}`), mixed)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	want := `{"cwd":"/mnt/d/Work/Example/src","realParentDir":"C:\\Users\\alice\\.claude\\backups"}`
	if string(out) != want {
		t.Errorf("got  %s\nwant %s", out, want)
	}
}

func TestCanonicalizeIgnoresProseThatBeginsWithAPath(t *testing.T) {
	// The finding this guards downgrades compatibility, which stops sync
	// entirely, so prose starting with a path must not trigger it.
	c := NewCanonicalizer(winSpace)
	records := []string{
		`{"text":"D:\\Workspace\\Example\\src\\a.go has a bug in it"}`,
		`{"command":"D:\\Workspace\\Example\\build.exe --release"}`,
	}
	for _, r := range records {
		if _, err := c.Record([]byte(r)); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if got := c.UnknownPathFields(); len(got) != 0 {
		t.Errorf("prose was reported as schema drift: %v", got)
	}
}

func TestCanonicalizeTokenizesTopLevelPath(t *testing.T) {
	// A top-level path has no field name, but the structural fallback still
	// tokenizes it so it can be localized on another device.
	c := NewCanonicalizer(winSpace)
	if _, err := c.Record([]byte(`"D:\\Workspace\\Example\\a.go"`)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := c.UnknownPathFields()
	if len(got) != 0 {
		t.Errorf("got findings %v, want none", got)
	}
}

func TestUnknownPathFieldsNeverLeakContent(t *testing.T) {
	// Findings reach `ctxhop doctor`, whose output must be safe to paste
	// into a public issue: no paths, project names or session content (BR-09).
	c := NewCanonicalizer(winSpace)
	in := `{"otherContainer":{"C:\\Users\\alice\\.claude\\x.md":"D:\\Workspace\\Example\\y"}}`
	if _, err := c.Record([]byte(in)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	for _, f := range c.UnknownPathFields() {
		if strings.ContainsAny(f, `:\/`) {
			t.Errorf("finding leaks a path: %q", f)
		}
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
	original := `{"cwd":"D:\\Workspace\\Example","message":{"content":[{"input":{"file_path":"D:\\Workspace\\Example\\src\\a.go"}}]},"n":42}`

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

// TestTokenizePrefersTheMoreSpecificRoot pins down what happens when one root
// is nested inside the other, which is the case whenever the agent's data
// directory sits under the project. Matching the shorter root first would
// tokenize such a path as the project and lose the distinction.
func TestTokenizePrefersTheMoreSpecificRoot(t *testing.T) {
	nested := PathSpace{
		ProjectRoot: `D:\Work`,
		AgentHome:   `D:\Work\tools\.claude`,
	}

	got := canonical(t, nested, `{"file_path":"D:\\Work\\tools\\.claude\\x.md"}`)
	if want := `{"file_path":"${AS_AGENT_HOME}/x.md"}`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// A path under the project but outside the agent directory still resolves
	// to the project.
	got = canonical(t, nested, `{"file_path":"D:\\Work\\src\\a.go"}`)
	if want := `{"file_path":"${AS_PROJECT}/src/a.go"}`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestSeparatorFor(t *testing.T) {
	tests := map[string]string{
		`D:\a\b`:         `\`,
		`\\server\share`: `\`,
		`relative\path`:  `\`,
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
	f.Add(`{"cwd":"D:\\Workspace\\Example"}`)
	f.Add(`{"a":[1,2,{"file_path":"D:\\Workspace\\Example\\x"}]}`)
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

func TestCanonicalizeTokenizesUnknownLeafPaths(t *testing.T) {
	c := NewCanonicalizer(winSpace)
	out, err := c.Record([]byte(`{"newPathField":"D:\\Workspace\\Example\\src\\a.go","message":{"content":{"content":"C:\\Users\\alice\\.claude\\x"}}}`))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	want := `{"message":{"content":{"content":"${AS_AGENT_HOME}/x"}},"newPathField":"${AS_PROJECT}/src/a.go"}`
	if got := string(out); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got := c.UnknownPathFields(); len(got) != 0 {
		t.Errorf("got findings %v, want none", got)
	}
}

func TestLocalizeTokenizesUnknownLeafPaths(t *testing.T) {
	space := PathSpace{ProjectRoot: `D:\Target\Example`, AgentHome: `C:\Users\bob\.claude`}
	in := []byte(`{"message":{"content":{"content":"${AS_PROJECT}/src/a.go"}}}`)
	out, err := Localize(in, space)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	want := `{"message":{"content":{"content":"D:\\Target\\Example\\src\\a.go"}}}`
	if string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

func TestClaudeCode228StructuredPathFields(t *testing.T) {
	in := `{"attachment":{"content":{"file":{"filePath":"D:\\Workspace\\Example\\attachments\\file with spaces.txt"}},"filename":"D:\\Workspace\\Example\\attachments\\file with spaces.txt","planFilePath":"D:\\Workspace\\Example\\plans\\plan.md"},"backup":{"realParentDir":"C:\\Users\\alice\\.claude\\backups"},"cwd":"D:\\Workspace\\Example","message":{"content":[{"content":"D:\\Workspace\\Example\\src\\a.go is discussed here","input":{"file_path":"D:\\Workspace\\Example\\src\\file with spaces.go","path":"D:\\Workspace\\Example\\src\\path with spaces.go"}}]},"snapshot":{"trackedFileBackups":{"C:\\Users\\alice\\.claude\\src\\a.go":{"realParentDir":"C:\\Users\\alice\\.claude"}}},"toolUseResult":{"file":{"filePath":"D:\\Workspace\\Example\\src\\nested.go"},"filePath":"D:\\Workspace\\Example\\src\\result.go","filenames":["D:\\Workspace\\Example\\src\\one.go","C:\\Users\\alice\\.claude\\src\\two.go"]}}`
	want := `{"attachment":{"content":{"file":{"filePath":"${AS_PROJECT}/attachments/file with spaces.txt"}},"filename":"${AS_PROJECT}/attachments/file with spaces.txt","planFilePath":"${AS_PROJECT}/plans/plan.md"},"backup":{"realParentDir":"${AS_AGENT_HOME}/backups"},"cwd":"${AS_PROJECT}","message":{"content":[{"content":"D:\\Workspace\\Example\\src\\a.go is discussed here","input":{"file_path":"${AS_PROJECT}/src/file with spaces.go","path":"${AS_PROJECT}/src/path with spaces.go"}}]},"snapshot":{"trackedFileBackups":{"${AS_AGENT_HOME}/src/a.go":{"realParentDir":"${AS_AGENT_HOME}"}}},"toolUseResult":{"file":{"filePath":"${AS_PROJECT}/src/nested.go"},"filePath":"${AS_PROJECT}/src/result.go","filenames":["${AS_PROJECT}/src/one.go","${AS_AGENT_HOME}/src/two.go"]}}`

	c := NewCanonicalizer(winSpace)
	out, err := c.Record([]byte(in))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	got := string(out)
	if !equivalentJSON(t, got, want) {
		t.Errorf("canonical  %s\nwant       %s", got, want)
	}
	if findings := c.UnknownPathFields(); len(findings) != 0 {
		t.Errorf("unexpected findings from the 2.1.228 structured fields: %v", findings)
	}
}

func TestClaudeCode228NestedContentPath(t *testing.T) {
	in := `{"message":{"content":{"content":"D:\\Workspace\\Example\\folder with spaces\\a.txt"}}}`
	want := `{"message":{"content":{"content":"${AS_PROJECT}/folder with spaces/a.txt"}}}`

	got := canonical(t, winSpace, in)
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	localized, err := Localize([]byte(want), posixSpace)
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	wantLocalized := `{"message":{"content":{"content":"/Users/bob/Projects/Example/folder with spaces/a.txt"}}}`
	if string(localized) != wantLocalized {
		t.Errorf("localized  %s\nwant       %s", localized, wantLocalized)
	}
}

func TestClaudeCode228ContentArrayKeepsFreeText(t *testing.T) {
	in := `{"message":{"content":[{"content":"D:\\Workspace\\Example\\folder with spaces\\a.txt is mentioned in the conversation"}]}}`
	if got := canonical(t, winSpace, in); got != in {
		t.Errorf("got  %s\nwant %s", got, in)
	}
}
