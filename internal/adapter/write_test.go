package adapter

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProject = `D:\Work\Example`

func records(lines ...string) [][]byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = []byte(l)
	}
	return out
}

// leftovers reports temporary files the writer failed to clean up. The agent's
// data directory is not ours to litter, so this is asserted after every failure
// case rather than only the happy path.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestWriteSessionRoundTrips(t *testing.T) {
	l := Layout{Home: t.TempDir()}

	want := records(`{"a":1}`, `{"a":2}`)
	if err := l.WriteSession(testProject, "abc", want); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	data, err := ReadSessionFile(l.SessionFile(testProject, "abc"))
	if err != nil {
		t.Fatalf("ReadSessionFile: %v", err)
	}
	if len(data.Records) != 2 {
		t.Fatalf("got %d records, want 2", len(data.Records))
	}
	if data.DroppedTail {
		t.Error("the writer must terminate its last record")
	}
	for i := range want {
		if string(data.Records[i]) != string(want[i]) {
			t.Errorf("record %d = %s, want %s", i, data.Records[i], want[i])
		}
	}
	if got := leftovers(t, l.SessionDir(testProject)); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteSessionCreatesMissingDirectory(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), "nested", ".claude")}

	if err := l.WriteSession(testProject, "abc", records(`{"a":1}`)); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	if _, err := os.Stat(l.SessionFile(testProject, "abc")); err != nil {
		t.Errorf("session not written: %v", err)
	}
}

func TestWriteSessionRefusesToOverwrite(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	original := records(`{"original":true}`)

	if err := l.WriteSession(testProject, "abc", original); err != nil {
		t.Fatalf("first write: %v", err)
	}

	err := l.WriteSession(testProject, "abc", records(`{"replacement":true}`))
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("got %v, want ErrSessionExists", err)
	}

	// The refusal must not have touched the existing session.
	data, err := ReadSessionFile(l.SessionFile(testProject, "abc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data.Records[0]) != `{"original":true}` {
		t.Errorf("existing session was modified: %s", data.Records[0])
	}
	if got := leftovers(t, l.SessionDir(testProject)); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestReplaceSessionOverwrites(t *testing.T) {
	l := Layout{Home: t.TempDir()}

	if err := l.WriteSession(testProject, "abc", records(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.ReplaceSession(testProject, "abc", records(`{"n":1}`, `{"n":2}`)); err != nil {
		t.Fatalf("ReplaceSession: %v", err)
	}

	data, err := ReadSessionFile(l.SessionFile(testProject, "abc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Records) != 2 {
		t.Errorf("got %d records, want 2", len(data.Records))
	}
}

func TestWriteSessionRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		records [][]byte
		wantErr error
	}{
		{name: "no records", records: nil},
		{name: "embedded newline", records: records("{\"a\":1}\n{\"a\":2}"), wantErr: ErrInvalidRecord},
		{name: "embedded carriage return", records: records("{\"a\":1}\r"), wantErr: ErrInvalidRecord},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := Layout{Home: t.TempDir()}

			err := l.WriteSession(testProject, "abc", tt.records)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}

			// Validation happens before anything is created, so a rejected
			// write must leave no trace at all.
			if _, err := os.Stat(l.SessionFile(testProject, "abc")); !errors.Is(err, os.ErrNotExist) {
				t.Error("a rejected write created a session file")
			}
			if got := leftovers(t, l.SessionDir(testProject)); got != nil {
				t.Errorf("temporary files left behind: %v", got)
			}
		})
	}
}

// TestWriteSessionFailedRenameLeavesNoTrace covers the interrupted-write case
// BR-11 is about: the write itself succeeds but publishing it does not.
func TestWriteSessionFailedRenameLeavesNoTrace(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	dir := l.SessionDir(testProject)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A directory sitting where the session file belongs makes the rename fail
	// after the temporary file has been written and synced.
	blocker := l.SessionFile(testProject, "abc")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}

	err := l.ReplaceSession(testProject, "abc", records(`{"a":1}`))
	if err == nil {
		t.Fatal("expected the rename to fail")
	}
	if !strings.Contains(err.Error(), "install session") {
		t.Errorf("error should name the failing step, got %v", err)
	}
	if got := leftovers(t, dir); got != nil {
		t.Errorf("temporary files left behind: %v", got)
	}
}

func TestWriteSessionRejectsUnreadableTarget(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	dir := l.SessionDir(testProject)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory in place of the session file also makes the existence check
	// see something, so WriteSession must refuse rather than clobber it.
	if err := os.MkdirAll(l.SessionFile(testProject, "abc"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := l.WriteSession(testProject, "abc", records(`{"a":1}`)); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("got %v, want ErrSessionExists", err)
	}
}

func TestWriteSessionRejectsUnsafeSessionIDs(t *testing.T) {
	// A native id arrives as metadata from another device. Joined onto a path
	// unchecked, these walk straight out of the project's directory - and
	// MkdirAll would build the way there.
	unsafe := []string{
		"..",
		"../escape",
		`..\escape`,
		"sub/dir",
		`sub\dir`,
		"",
		".",
		"has space",
		"quote\"mark",
	}

	for _, id := range unsafe {
		t.Run(id, func(t *testing.T) {
			home := t.TempDir()
			l := Layout{Home: home}

			err := l.WriteSession(testProject, id, records(`{"a":1}`))
			if !errors.Is(err, ErrInvalidSessionID) {
				t.Fatalf("got %v, want ErrInvalidSessionID", err)
			}
			if err := l.ReplaceSession(testProject, id, records(`{"a":1}`)); !errors.Is(err, ErrInvalidSessionID) {
				t.Fatalf("ReplaceSession: got %v, want ErrInvalidSessionID", err)
			}

			// Nothing may have been created anywhere under the home directory.
			var created []string
			_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					created = append(created, p)
				}
				return nil
			})
			if len(created) != 0 {
				t.Errorf("a rejected id created files: %v", created)
			}
		})
	}
}

func TestWriteSessionAcceptsRealisticSessionIDs(t *testing.T) {
	l := Layout{Home: t.TempDir()}

	for _, id := range []string{
		"1ec04445-8626-4962-bded-d17fe30a8128",
		"abc123",
		"with_underscore",
		"with.dot",
	} {
		if err := l.WriteSession(testProject, id, records(`{"a":1}`)); err != nil {
			t.Errorf("WriteSession(%q): %v", id, err)
		}
	}
}

// failingWriter fails once it has accepted limit bytes, standing in for a disk
// that fills up or a device that disappears partway through a write.
type failingWriter struct {
	limit   int
	written int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written+len(p) > f.limit {
		n := f.limit - f.written
		f.written = f.limit
		return n, errors.New("device full")
	}
	f.written += len(p)
	return len(p), nil
}

func TestWriteRecordsPropagatesWriteFailures(t *testing.T) {
	big := `{"x":"` + strings.Repeat("y", 8000) + `"}`

	tests := []struct {
		name    string
		limit   int
		records [][]byte
	}{
		// Small sessions fit in bufio's buffer, so the failure surfaces at
		// Flush time.
		{name: "fails on flush", limit: 0, records: records(`{"a":1}`, `{"b":2}`)},
		{name: "fails on partial flush", limit: 4, records: records(`{"a":1}`, `{"b":2}`)},
		// A record larger than the buffer reaches the writer mid-loop, which is
		// the path a long session actually takes.
		{name: "fails mid record", limit: 100, records: records(big, big)},
		{name: "fails on the terminator", limit: 8005, records: records(big)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeRecords(&failingWriter{limit: tt.limit}, tt.records); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestWriteRecordsTerminatesEveryRecord(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRecords(&buf, records(`{"a":1}`, `{"b":2}`)); err != nil {
		t.Fatalf("writeRecords: %v", err)
	}
	// Our own reader discards an unterminated trailing record, so the final
	// newline is not cosmetic.
	if want := "{\"a\":1}\n{\"b\":2}\n"; buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestWriteSessionReportsUnusableHome(t *testing.T) {
	// A file where the data directory belongs makes MkdirAll fail, which must
	// surface as an error rather than a panic or a silent no-op.
	home := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := Layout{Home: home}
	err := l.WriteSession(testProject, "abc", records(`{"a":1}`))
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if !strings.Contains(err.Error(), "create session directory") {
		t.Errorf("error should name the failing step, got %v", err)
	}
}

// TestWriteSessionOutputIsReadableByOurOwnReader closes the loop: whatever the
// writer produces must satisfy the reader's rule that every record is
// newline-terminated.
func TestWriteSessionOutputIsReadableByOurOwnReader(t *testing.T) {
	l := Layout{Home: t.TempDir()}

	canon, err := NewCanonicalizer(winSpace).Record([]byte(`{"cwd":"D:\\Workspace\\Example","n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	local, err := Localize(canon, PathSpace{ProjectRoot: testProject, AgentHome: `C:\Users\alice\.claude`})
	if err != nil {
		t.Fatal(err)
	}

	if err := l.WriteSession(testProject, "abc", [][]byte{local}); err != nil {
		t.Fatalf("WriteSession: %v", err)
	}

	data, err := ReadSessionFile(l.SessionFile(testProject, "abc"))
	if err != nil {
		t.Fatal(err)
	}
	if data.DroppedTail || len(data.Records) != 1 {
		t.Fatalf("round trip lost data: %+v", data)
	}
	if want := `{"cwd":"D:\\Work\\Example","n":1}`; string(data.Records[0]) != want {
		t.Errorf("got %s, want %s", data.Records[0], want)
	}
}
