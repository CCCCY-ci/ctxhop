package project

import (
	"strings"
	"testing"
)

// TestTheSameRepositoryCanonicalizesOneWay is the requirement the whole layer
// rests on: two devices that cloned the same project by different means must
// arrive at the same identity, or neither ever sees the other's sessions
// (PRD §9.3).
func TestTheSameRepositoryCanonicalizesOneWay(t *testing.T) {
	const want = "github.com/user/example"

	for name, raw := range map[string]string{
		"scp shorthand":      "git@github.com:user/example.git",
		"https":              "https://github.com/user/example.git",
		"https without .git": "https://github.com/user/example",
		"https trailing /":   "https://github.com/user/example/",
		"ssh url":            "ssh://git@github.com/user/example.git",
		"ssh default port":   "ssh://git@github.com:22/user/example.git",
		"https default port": "https://github.com:443/user/example.git",
		"git protocol":       "git://github.com/user/example.git",
		"host in capitals":   "https://GitHub.COM/user/example.git",
		"scp without user":   "github.com:user/example.git",
		"surrounded by ws":   "  https://github.com/user/example.git\n",
		"with a token":       "https://ghp_AAAABBBBCCCC@github.com/user/example.git",
		"with user and pass": "https://someone:hunter2@github.com/user/example.git",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CanonicalizeRemote(raw)
			if err != nil {
				t.Fatalf("CanonicalizeRemote(%q): %v", raw, err)
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestCredentialsNeverSurvive guards a leak with two independent causes: a
// remote URL may carry an access token, and this string reaches error messages
// and doctor output, which must be safe to paste into a public issue (BR-09,
// code_style §5.2). Separately, a URL with credentials on one machine and
// without on another would never match.
func TestCredentialsNeverSurvive(t *testing.T) {
	const secret = "ghp_SUPERSECRETTOKENVALUE"

	for _, raw := range []string{
		"https://" + secret + "@github.com/user/example.git",
		"https://someone:" + secret + "@github.com/user/example.git",
		"ssh://" + secret + "@github.com/user/example.git",
		"https://" + secret + "@github.com/user/example.git:99999999",
	} {
		got, err := CanonicalizeRemote(raw)
		if strings.Contains(got, secret) {
			t.Errorf("the canonical form leaks the credential: %q", got)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("the error leaks the credential: %v", err)
		}
	}
}

func TestNonDefaultPortsAreKept(t *testing.T) {
	// A self-hosted forge on another port is a different host, and dropping the
	// port would merge it with whatever runs on the default one.
	got, err := CanonicalizeRemote("ssh://git@git.example.com:2222/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if got != "git.example.com:2222/team/app" {
		t.Errorf("got %q", got)
	}
}

func TestPathsWithAwkwardCharacters(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"non-ascii": {"https://git.example.com/团队/项目.git", "git.example.com/团队/项目"},
		"space":     {"https://git.example.com/my%20team/app.git", "git.example.com/my team/app"},
		"deep path": {"https://git.example.com/a/b/c/d.git", "git.example.com/a/b/c/d"},
		"dots":      {"https://git.example.com/team/my.app.git", "git.example.com/team/my.app"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CanonicalizeRemote(tc.raw)
			if err != nil {
				t.Fatalf("CanonicalizeRemote(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyTheDotGitSuffixIsTrimmed keeps the trimming from eating a repository
// actually named something ending in those characters.
func TestOnlyTheDotGitSuffixIsTrimmed(t *testing.T) {
	got, err := CanonicalizeRemote("https://github.com/user/gitgit.git")
	if err != nil {
		t.Fatal(err)
	}
	if got != "github.com/user/gitgit" {
		t.Errorf("got %q, want github.com/user/gitgit", got)
	}
}

// TestPathCaseIsPreserved documents a deliberate choice rather than an
// accident. Lowercasing would merge two different repositories on a
// case-sensitive forge, which puts somebody else's sessions in your project.
// Failing to match merely means the feature does not fire, and manual binding
// repairs it (spec §2.2).
func TestPathCaseIsPreserved(t *testing.T) {
	lower, err := CanonicalizeRemote("https://github.com/user/example.git")
	if err != nil {
		t.Fatal(err)
	}
	upper, err := CanonicalizeRemote("https://github.com/User/Example.git")
	if err != nil {
		t.Fatal(err)
	}
	if lower == upper {
		t.Error("paths differing only in case were merged")
	}
}

func TestRemotesWithNoCrossDeviceMeaning(t *testing.T) {
	// Every one of these must be refused rather than turned into an identity:
	// a path on this machine says nothing about any other machine.
	for name, raw := range map[string]string{
		"empty":                   "",
		"whitespace":              "   ",
		"absolute posix":          "/home/someone/src/example",
		"relative":                "./example",
		"parent":                  "../example",
		"windows drive":           `C:/src/example`,
		"windows drive backslash": `C:\src\example`,
		"unc":                     `\\server\share\example`,
		"file url":                "file:///home/someone/src/example",
		"bare word":               "example",
		"scheme only":             "https://",
		"host only":               "https://github.com",
		"scp with empty path":     "git@github.com:",
		"unknown scheme":          "carrier-pigeon://github.com/user/example",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CanonicalizeRemote(raw)
			if err == nil {
				t.Errorf("CanonicalizeRemote(%q) = %q, want a refusal", raw, got)
			}
		})
	}
}
