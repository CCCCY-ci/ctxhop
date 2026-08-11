// Command restore moves a session from one machine's path space into another's
// using the production adapter, so PoC-1b exercises the code that will ship
// rather than a one-off script.
//
// A second machine is simulated with CLAUDE_CONFIG_DIR, which relocates the
// agent's whole data directory. That covers everything except a genuinely
// different operating system: a different project path, a different agent home,
// and a real resume at the end.
//
// Output reports counts and verdicts, never session content.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/CCCCY-ci/agentsync/internal/adapter"
)

func main() {
	var (
		fromRoot = flag.String("from-root", "", "project root on the source machine")
		fromHome = flag.String("from-home", "", "agent data directory on the source machine")
		toRoot   = flag.String("to-root", "", "project root on the target machine")
		toHome   = flag.String("to-home", "", "agent data directory on the target machine")
		writeTo  = flag.String("write-to", "", "agent data directory to install into (defaults to -to-home)")
		id       = flag.String("id", "", "native session id")
	)
	flag.Parse()

	if *fromRoot == "" || *fromHome == "" || *toRoot == "" || *toHome == "" || *id == "" {
		fmt.Fprintln(os.Stderr, "usage: restore -from-root X -from-home Y -to-root Z -to-home W -id ID [-write-to V]")
		os.Exit(2)
	}
	if *writeTo == "" {
		writeTo = toHome
	}

	if err := run(*fromRoot, *fromHome, *toRoot, *toHome, *writeTo, *id); err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		os.Exit(1)
	}
}

func run(fromRoot, fromHome, toRoot, toHome, writeTo, id string) error {
	source := adapter.Layout{Home: fromHome}
	target := adapter.Layout{Home: writeTo}

	inst, err := source.Detect(context.Background())
	if err != nil {
		return fmt.Errorf("detect source agent: %w", err)
	}
	fmt.Printf("source agent: version=%q compatibility=%v (%s)\n",
		inst.Version, inst.Compatibility, inst.CompatibilityReason)

	data, err := adapter.ReadSessionFile(source.SessionFile(fromRoot, id))
	if err != nil {
		return fmt.Errorf("read source session: %w", err)
	}
	fmt.Printf("read: %d records, droppedTail=%v\n", len(data.Records), data.DroppedTail)

	fromSpace := adapter.PathSpace{ProjectRoot: fromRoot, AgentHome: fromHome}
	toSpace := adapter.PathSpace{ProjectRoot: toRoot, AgentHome: toHome}

	canon := adapter.NewCanonicalizer(fromSpace)
	canonical := make([][]byte, 0, len(data.Records))
	var projectTokens, homeTokens int

	for i, rec := range data.Records {
		out, err := canon.Record(rec)
		if err != nil {
			return fmt.Errorf("canonicalize record %d: %w", i+1, err)
		}
		projectTokens += strings.Count(string(out), adapter.TokenProject)
		homeTokens += strings.Count(string(out), adapter.TokenAgentHome)
		canonical = append(canonical, out)
	}

	findings := canon.UnknownPathFields()
	level, reason := adapter.GradeSession(inst.Compatibility, findings)
	fmt.Printf("canonicalize: %d project tokens, %d agent-home tokens, %d unknown path fields\n",
		projectTokens, homeTokens, len(findings))
	if len(findings) > 0 {
		fmt.Printf("  findings: %v\n", findings)
	}
	if level == adapter.CompatStopped {
		return fmt.Errorf("refusing to restore: %s", reason)
	}

	// Determinism is what the whole storage design rests on, so assert it here
	// against real records rather than only against fixtures.
	recheck := adapter.NewCanonicalizer(fromSpace)
	for i, rec := range data.Records {
		again, err := recheck.Record(rec)
		if err != nil || string(again) != string(canonical[i]) {
			return fmt.Errorf("canonical form is not stable at record %d", i+1)
		}
	}
	fmt.Println("canonicalize: stable across repeated runs")

	localized := make([][]byte, 0, len(canonical))
	for i, rec := range canonical {
		out, err := adapter.Localize(rec, toSpace)
		if err != nil {
			return fmt.Errorf("localize record %d: %w", i+1, err)
		}
		localized = append(localized, out)
	}

	// Assert on decoded field values, not on the raw bytes: a record is JSON,
	// so a Windows path appears there with its separators escaped and a naive
	// substring check reports "absent" for a path that is plainly present.
	cwds := map[string]int{}
	for _, rec := range localized {
		var doc map[string]any
		if err := json.Unmarshal(rec, &doc); err != nil {
			return fmt.Errorf("localized record is not valid json: %w", err)
		}
		if cwd, ok := doc["cwd"].(string); ok {
			cwds[cwd]++
		}
	}
	for cwd, n := range cwds {
		verdict := "UNEXPECTED"
		if cwd == toRoot {
			verdict = "ok"
		}
		fmt.Printf("localize: cwd=%q in %d records [%s]\n", redact(cwd, fromRoot, toRoot), n, verdict)
	}
	if len(cwds) == 0 {
		fmt.Println("localize: no cwd fields in this session")
	}

	// The property the whole version model depends on: after a record has
	// crossed to another machine, canonicalising it there must reproduce the
	// identical bytes. If it does not, prefix comparison sees divergence on
	// every record and fast-forward never fires.
	roundTrip := adapter.NewCanonicalizer(toSpace)
	for i, rec := range localized {
		again, err := roundTrip.Record(rec)
		if err != nil {
			return fmt.Errorf("recanonicalize record %d on the target: %w", i+1, err)
		}
		if string(again) != string(canonical[i]) {
			// Report where they diverge, never the records themselves. This is
			// the output most likely to be pasted into a document or an issue,
			// and a record holds conversation text, tool output and file
			// contents.
			return fmt.Errorf("record %d does not survive the round trip: differs at byte %d (%d bytes before, %d after)",
				i+1, firstDifference(canonical[i], again), len(canonical[i]), len(again))
		}
	}
	fmt.Printf("round trip: all %d records canonicalize identically on the target\n", len(localized))

	touched := adapter.TouchedFiles(data.Records, fromRoot)
	written := 0
	for _, f := range touched {
		if f.Written {
			written++
		}
	}
	fmt.Printf("touched files: %d (%d written)\n", len(touched), written)

	if err := target.ReplaceSession(toRoot, id, localized); err != nil {
		return fmt.Errorf("install session: %w", err)
	}
	fmt.Printf("installed: %s\n", target.SessionFile(toRoot, id))

	// Read it back through the same reader the sync layer would use.
	back, err := adapter.ReadSessionFile(target.SessionFile(toRoot, id))
	if err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if len(back.Records) != len(localized) || back.DroppedTail {
		return fmt.Errorf("read back mismatch: %d records, droppedTail=%v", len(back.Records), back.DroppedTail)
	}
	fmt.Printf("read back: %d records, intact\n", len(back.Records))
	return nil
}

// firstDifference returns the index of the first differing byte, or the length
// of the shorter input when one is a prefix of the other.
func firstDifference(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// redact replaces the machine's real paths with labels, so the output stays
// safe to paste into a document or an issue.
func redact(s, fromRoot, toRoot string) string {
	s = strings.ReplaceAll(s, toRoot, "<TARGET_PROJECT>")
	s = strings.ReplaceAll(s, fromRoot, "<SOURCE_PROJECT>")
	return s
}
