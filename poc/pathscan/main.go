// Command pathscan reports where absolute paths appear inside an agent session
// file, so PoC-1 can enumerate everything a cross-device restore must rewrite.
//
// It deliberately prints only field paths and counts, never values: session
// bodies contain source code, terminal output and credentials, and PoC findings
// end up in documents and issues.
//
// Usage:
//
//	go run ./poc/pathscan -file session.jsonl -root 'D:\Projects\Example'
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// maxLine bounds a single JSONL record. Session records embed whole file
// contents and command output, so the default scanner buffer is far too small.
const maxLine = 64 << 20

// driveOrPosixRoot matches strings that look like an absolute path on either
// platform: a Windows drive prefix, a UNC prefix, or a POSIX root.
var driveOrPosixRoot = regexp.MustCompile(`(?i)(^|[^a-z0-9])([a-z]:[\\/]|\\\\[^\\]+\\|/(?:Users|home|var|tmp|opt|etc)/)`)

// fieldStat accumulates what we learned about one JSON field path.
type fieldStat struct {
	// occurrences counts how many times the field appeared across records.
	occurrences int
	// strings counts occurrences whose value was a string.
	strings int
	// rootHits counts values containing the project root path.
	rootHits int
	// slugHits counts values containing the encoded project directory name.
	slugHits int
	// absHits counts values that look like some absolute path.
	absHits int
	// maxLen is the longest value seen, used to spot fields that carry file
	// contents rather than metadata.
	maxLen int
}

func main() {
	file := flag.String("file", "", "path to the session .jsonl file")
	root := flag.String("root", "", "absolute project root as recorded on the source machine")
	slug := flag.String("slug", "", "encoded project directory name, if known")
	flag.Parse()

	if *file == "" || *root == "" {
		fmt.Fprintln(os.Stderr, "usage: pathscan -file <session.jsonl> -root <project root> [-slug <dir name>]")
		os.Exit(2)
	}

	stats, records, types, err := scan(*file, *root, *slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pathscan: %v\n", err)
		os.Exit(1)
	}

	report(stats, records, types)
}

func scan(file, root, slug string) (map[string]*fieldStat, int, map[string]int, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("open session: %w", err)
	}
	defer f.Close()

	stats := map[string]*fieldStat{}
	types := map[string]int{}
	records := 0

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	lowerRoot := strings.ToLower(root)
	// A path is also embedded with escaped or forward separators depending on
	// who wrote it, so compare against both spellings.
	altRoot := strings.ToLower(strings.ReplaceAll(root, `\`, `/`))

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		records++

		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, 0, nil, fmt.Errorf("record %d is not valid json: %w", records, err)
		}

		// Track the record "type" field if present: it tells us how many kinds
		// of record an adapter has to understand.
		if m, ok := v.(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				types[t]++
			}
		}

		walk("", v, func(path string, val any) {
			st := stats[path]
			if st == nil {
				st = &fieldStat{}
				stats[path] = st
			}
			st.occurrences++

			s, ok := val.(string)
			if !ok {
				return
			}
			st.strings++
			if len(s) > st.maxLen {
				st.maxLen = len(s)
			}

			ls := strings.ToLower(s)
			if strings.Contains(ls, lowerRoot) || strings.Contains(ls, altRoot) {
				st.rootHits++
			}
			if slug != "" && strings.Contains(ls, strings.ToLower(slug)) {
				st.slugHits++
			}
			if driveOrPosixRoot.MatchString(s) {
				st.absHits++
			}
		})
	}
	if err := sc.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("read session: %w", err)
	}

	return stats, records, types, nil
}

// walk visits every leaf value in a decoded JSON document, building a dotted
// field path. Array indices collapse to "[]" so that repeated elements
// aggregate into a single reported field.
func walk(prefix string, v any, visit func(path string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			walk(join(prefix, k), child, visit)
		}
	case []any:
		for _, child := range t {
			walk(prefix+"[]", child, visit)
		}
	default:
		visit(prefix, v)
	}
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func report(stats map[string]*fieldStat, records int, types map[string]int) {
	paths := make([]string, 0, len(stats))
	for p := range stats {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	fmt.Printf("records: %d\nfields:  %d\n\n", records, len(stats))

	fmt.Println("record types:")
	typeNames := make([]string, 0, len(types))
	for t := range types {
		typeNames = append(typeNames, t)
	}
	sort.Strings(typeNames)
	for _, t := range typeNames {
		fmt.Printf("  %-28s %d\n", t, types[t])
	}

	fmt.Printf("\n%-52s %6s %6s %6s %6s %8s\n", "FIELD", "N", "ROOT", "SLUG", "ABS", "MAXLEN")
	for _, p := range paths {
		st := stats[p]
		// Only fields that carry an absolute path anywhere are interesting for
		// PoC-1; the rest are noise.
		if st.rootHits == 0 && st.slugHits == 0 && st.absHits == 0 {
			continue
		}
		fmt.Printf("%-52s %6d %6d %6d %6d %8d\n", truncate(p, 52), st.occurrences, st.rootHits, st.slugHits, st.absHits, st.maxLen)
	}

	fmt.Println("\nlegend: N=occurrences ROOT=contains project root SLUG=contains encoded dir ABS=looks absolute")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}
