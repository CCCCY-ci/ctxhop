package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedVersion(v string) func(context.Context) string {
	return func(context.Context) string { return v }
}

func TestDefaultHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", `D:\elsewhere\.claude`)
	got, err := DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	// The agent honours this variable, so ignoring it would read and write a
	// different directory from the one actually in use.
	if got != `D:\elsewhere\.claude` {
		t.Errorf("got %q, want the override", got)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "   ")
	got, err = DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("", ".claude")) {
		t.Errorf("a blank override should fall back to the home directory, got %q", got)
	}
}

func TestDetect(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".claude")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	l := Layout{Home: home, version: fixedVersion("2.1.228")}
	inst, err := l.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inst.DataDir != home {
		t.Errorf("DataDir = %q", inst.DataDir)
	}
	if inst.Version != "2.1.228" {
		t.Errorf("Version = %q", inst.Version)
	}
	if inst.Compatibility != CompatFull {
		t.Errorf("Compatibility = %v, want CompatFull", inst.Compatibility)
	}
}

func TestDetectWhenAbsent(t *testing.T) {
	l := Layout{Home: filepath.Join(t.TempDir(), "never-created")}

	// An absent agent is an expected outcome, not a failure: it must not
	// produce an error message or affect any other agent.
	if _, err := l.Detect(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("got %v, want ErrNotInstalled", err)
	}
}

func TestDetectRejectsBadHomes(t *testing.T) {
	t.Run("no home configured", func(t *testing.T) {
		if _, err := (Layout{}).Detect(context.Background()); err == nil {
			t.Fatal("expected an error, got none")
		}
	})

	t.Run("home is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		l := Layout{Home: path}
		_, err := l.Detect(context.Background())
		if err == nil {
			t.Fatal("expected an error, got none")
		}
		// Not "not installed": something is there, and mistaking the two would
		// report a healthy absence when the setup is actually broken.
		if errors.Is(err, ErrNotInstalled) {
			t.Errorf("a file was reported as an absent agent: %v", err)
		}
	})
}

func TestCompatibilityBaselineIgnoresAgentVersion(t *testing.T) {
	for _, version := range []string{"2.1.226", "2.1.227", "2.1.228", "99.0.0", ""} {
		t.Run(version, func(t *testing.T) {
			home := t.TempDir()
			l := Layout{Home: home, version: fixedVersion(version)}
			inst, err := l.Detect(context.Background())
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if inst.Compatibility != CompatFull {
				t.Fatalf("compatibility for version %q = %v, want CompatFull", version, inst.Compatibility)
			}
			if inst.CompatibilityReason != structuralCompatibilityReason {
				t.Fatalf("reason for version %q = %q, want %q", version, inst.CompatibilityReason, structuralCompatibilityReason)
			}
			if strings.ContainsAny(inst.CompatibilityReason, "\\/") {
				t.Errorf("reason looks like it contains a path: %q", inst.CompatibilityReason)
			}
		})
	}
}
func TestGradeSession(t *testing.T) {
	t.Run("supported fields upgrade a provisional level", func(t *testing.T) {
		got, reason := GradeSession(CompatLimited, nil)
		if got != CompatFull || reason != structuralCompatibilityReason {
			t.Errorf("got %v %q", got, reason)
		}
	})

	t.Run("an unknown path field stops everything", func(t *testing.T) {
		// The risk is not that the version is new; it is that the session holds
		// a path we would fail to rewrite, which restores a session still
		// pointing at the machine that produced it.
		got, reason := GradeSession(CompatFull, []string{"someNewField"})
		if got != CompatStopped {
			t.Errorf("got %v, want CompatStopped", got)
		}
		if !strings.Contains(reason, "someNewField") {
			t.Errorf("reason should name the field, got %q", reason)
		}
	})

	t.Run("an installation already stopped remains stopped", func(t *testing.T) {
		got, reason := GradeSession(CompatStopped, nil)
		if got != CompatStopped || reason == "" {
			t.Errorf("got %v %q", got, reason)
		}
	})
}
func TestVersionComesFromTheNewestSession(t *testing.T) {
	// The agent's executable is never run, not even for --version: starting it
	// would hand our no-unrequested-network guarantee to somebody else's
	// startup path. The version is read from what the agent wrote.
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Example`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, version string, mod time.Time) {
		t.Helper()
		path := filepath.Join(dir, name)
		rec := `{"type":"user","version":"` + version + `"}` + "\n"
		if err := os.WriteFile(path, []byte(rec), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	write("old.jsonl", "1.0.0", now.Add(-time.Hour))
	write("new.jsonl", "2.1.227", now)

	inst, err := l.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inst.Version != "2.1.227" {
		t.Errorf("Version = %q, want the newest session's", inst.Version)
	}
	if inst.Compatibility != CompatFull {
		t.Errorf("Compatibility = %v", inst.Compatibility)
	}
}

func TestVersionIsEmptyWithNothingRecorded(t *testing.T) {
	home := t.TempDir()
	l := Layout{Home: home}

	inst, err := l.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inst.Version != "" {
		t.Errorf("Version = %q, want empty", inst.Version)
	}
	// Compatibility is based on session fields, not on whether a version was
	// available during installation detection.
	if inst.Compatibility != CompatFull {
		t.Errorf("Compatibility = %v, want CompatFull", inst.Compatibility)
	}
}
func TestVersionDiscoveryIgnoresWhatItCannotUse(t *testing.T) {
	home := t.TempDir()
	l := Layout{Home: home}
	const root = `D:\Work\Example`

	dir := l.SessionDir(root)
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A loose file beside the project directories, a non-session file inside
	// one, and a session whose records carry no version.
	if err := os.WriteFile(filepath.Join(l.ProjectsDir(), "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(`{"type":"system"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inst, err := l.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if inst.Version != "" {
		t.Errorf("Version = %q, want empty", inst.Version)
	}
}

func TestVersionDiscoveryHonoursCancellation(t *testing.T) {
	home := t.TempDir()
	l := Layout{Home: home}
	if err := os.MkdirAll(l.SessionDir(`D:\Work\Example`), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Walking someone's whole projects directory must stop when asked; a
	// cancelled scan reports no version rather than pressing on.
	if got := l.versionFromNewestSession(ctx); got != "" {
		t.Errorf("got %q, want empty after cancellation", got)
	}
}

func TestDefaultHomeResolvesARelativeOverride(t *testing.T) {
	// The agent resolves this against its own working directory, which is not
	// ours; leaving it relative would point us at a different directory than
	// the one in use.
	t.Setenv("CLAUDE_CONFIG_DIR", ".claude-relative")
	got, err := DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("got %q, want an absolute path", got)
	}
}
