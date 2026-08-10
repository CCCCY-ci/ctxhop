// Command touched extracts the set of project files a session actually read or
// wrote, which is the scope the workspace consistency check operates on.
//
// Scoping matters more than it sounds. Comparing the whole working tree would
// flag every unrelated edit the user made on the target machine, users would
// learn to dismiss the warning, and the feature would be worthless. Restricting
// the check to files the session itself touched is what keeps it credible
// (§9.5). This tool measures whether that set can be extracted reliably.
//
// Tool names and counts are safe to print. File paths are only printed with
// -show-files, and always relative to the project root.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxLine = 64 << 20

// access records how a session interacted with one file.
type access struct {
	read  int
	write int
	// tools lists which tools touched it, for diagnosing misclassification.
	tools map[string]bool
}

// writeTools are tools whose use means the session changed the file. Anything
// not listed here is treated as a read, which is the conservative direction:
// over-reporting a read only costs a hash comparison, while missing a write
// would let a stale file slip through unnoticed.
var writeTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

func main() {
	in := flag.String("file", "", "session .jsonl")
	root := flag.String("root", "", "project root recorded in the session")
	showFiles := flag.Bool("show-files", false, "print project-relative file paths")
	flag.Parse()

	if *in == "" || *root == "" {
		fmt.Fprintln(os.Stderr, "usage: touched -file <session.jsonl> -root <project root> [-show-files]")
		os.Exit(2)
	}

	blockTypes := map[string]int{}
	toolUses := map[string]int{}
	toolWithPath := map[string]int{}
	files := map[string]*access{}
	outside := 0

	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "touched: open session: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			fmt.Fprintf(os.Stderr, "touched: bad record: %v\n", err)
			os.Exit(1)
		}

		msg, _ := rec["message"].(map[string]any)
		if msg == nil {
			continue
		}
		content, _ := msg["content"].([]any)
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if block == nil {
				continue
			}

			blockType, _ := block["type"].(string)
			blockTypes[blockType]++
			if blockType != "tool_use" {
				continue
			}

			name, _ := block["name"].(string)
			toolUses[name]++

			input, _ := block["input"].(map[string]any)
			if input == nil {
				continue
			}
			path, _ := input["file_path"].(string)
			if path == "" {
				continue
			}
			toolWithPath[name]++

			rel, ok := relativeTo(*root, path)
			if !ok {
				// Files outside the project are not rewritten and not checked:
				// the fingerprint describes the project, not the machine.
				outside++
				continue
			}

			a := files[rel]
			if a == nil {
				a = &access{tools: map[string]bool{}}
				files[rel] = a
			}
			a.tools[name] = true
			if writeTools[name] {
				a.write++
			} else {
				a.read++
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "touched: read session: %v\n", err)
		os.Exit(1)
	}

	report(blockTypes, toolUses, toolWithPath, files, outside, *showFiles)
}

// relativeTo converts an absolute path recorded in the session into a path
// relative to the project root, reporting whether it is inside the project at
// all. Comparison is case-insensitive to match Windows semantics.
func relativeTo(root, path string) (string, bool) {
	if len(path) < len(root) || !strings.EqualFold(path[:len(root)], root) {
		return "", false
	}
	rest := strings.TrimLeft(path[len(root):], `\/`)
	if rest == "" {
		return "", false
	}
	return filepath.ToSlash(rest), true
}

func report(blockTypes, toolUses, toolWithPath map[string]int, files map[string]*access, outside int, showFiles bool) {
	fmt.Println("content block types:")
	for _, k := range sorted(blockTypes) {
		fmt.Printf("  %-24s %d\n", k, blockTypes[k])
	}

	fmt.Printf("\n%-24s %8s %10s\n", "TOOL", "USES", "WITH PATH")
	for _, k := range sorted(toolUses) {
		fmt.Printf("  %-22s %8d %10d\n", k, toolUses[k], toolWithPath[k])
	}

	var written, readOnly int
	for _, a := range files {
		if a.write > 0 {
			written++
		} else {
			readOnly++
		}
	}

	fmt.Printf("\ndistinct project files touched: %d (written %d, read-only %d)\n", len(files), written, readOnly)
	fmt.Printf("paths outside the project root: %d\n", outside)

	if !showFiles {
		return
	}
	fmt.Println("\nfiles:")
	for _, rel := range sortedFiles(files) {
		a := files[rel]
		kind := "read"
		if a.write > 0 {
			kind = "WRITE"
		}
		fmt.Printf("  %-5s %-52s r=%d w=%d %v\n", kind, rel, a.read, a.write, sortedSet(a.tools))
	}
}

func sorted(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedFiles(m map[string]*access) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSet(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
