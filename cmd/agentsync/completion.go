package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

var completionSubcommands = map[string][]string{
	"completion": {"bash", "zsh", "fish", "powershell", "pwsh"},
	"device":     {"status", "mode", "list", "rename", "remove", "rotate-key", "invite"},
	"history":    {"cleanup", "prune"},
	"passphrase": {"change", "reset"},
	"project":    {"bind", "unbind", "mode", "list"},
	"remote":     {"delete-session", "delete-project", "delete-all"},
}

var completionValues = map[string][]string{
	"device":  {"normal", "push-only", "disabled"},
	"project": {"normal", "push-only", "excluded"},
}

var completionOptions = map[string][]string{
	"init":                  {"--backend", "--path", "--endpoint", "--bucket", "--region", "--prefix", "--path-style", "--device-name", "--device-mode", "--no-hook", "--expect-domain-fingerprint", "--invite"},
	"install":               {"--dir", "--no-path"},
	"status":                {"--json", "--remote"},
	"list":                  {"--json"},
	"resume":                {"--json", "--allow-limited", "--allow-divergent", "--no-workspace-context", "--replace-existing", "--version"},
	"history":               {"--json"},
	"history prune":         {"--yes", "--remote-id", "--path", "--keep", "--before"},
	"stats":                 {"--json"},
	"push":                  {"--agentsync-hook", "--session"},
	"watch":                 {"--interval", "--once", "--json"},
	"doctor":                {"--json"},
	"pull":                  {"--check", "--json"},
	"device status":         {"--json"},
	"device list":           {"--json"},
	"device remove":         {"--yes"},
	"device rotate-key":     {},
	"device invite":         {"--output"},
	"project bind":          {"--path", "--identity", "--name"},
	"project unbind":        {"--path", "--identity"},
	"project mode":          {"--path", "--identity"},
	"project list":          {"--json"},
	"remote delete-session": {"--yes", "--remote-id", "--path"},
	"remote delete-project": {"--yes", "--path"},
	"remote delete-all":     {"--yes"},
}

func init() {
	for i := range commands {
		if commands[i].name == "completion" {
			commands[i].run = runCompletion
		}
	}
}

func runCompletion(args []string) error {
	return runCompletionWithIO(args, os.Stdout)
}

func runCompletionWithIO(args []string, output io.Writer) error {
	if output == nil {
		return errors.New("completion: output is required")
	}
	shell, err := parseCompletionShell(args)
	if err != nil {
		return err
	}
	var script string
	switch shell {
	case "bash":
		script = renderBashCompletion()
	case "zsh":
		script = renderZshCompletion()
	case "fish":
		script = renderFishCompletion()
	case "powershell":
		script = renderPowerShellCompletion()
	default:
		return fmt.Errorf("completion: unsupported shell %q", shell)
	}
	_, err = io.WriteString(output, script)
	return err
}

func parseCompletionShell(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("completion: expected one shell: bash, zsh, fish, or powershell")
	}
	shell := strings.ToLower(strings.TrimSpace(args[0]))
	if shell == "pwsh" {
		shell = "powershell"
	}
	switch shell {
	case "bash", "zsh", "fish", "powershell":
		return shell, nil
	default:
		return "", fmt.Errorf("completion: unsupported shell %q", args[0])
	}
}

func completionCommandNames() []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.name)
	}
	sort.Strings(names)
	return names
}

func completionCandidates(command string) []string {
	values := append([]string(nil), completionSubcommands[command]...)
	values = append(values, completionValues[command]...)
	values = append(values, completionOptions[command]...)
	for _, action := range completionSubcommands[command] {
		values = append(values, completionOptions[command+" "+action]...)
	}
	return uniqueCompletionValues(values)
}

func uniqueCompletionValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func completionWords(values []string) string {
	return strings.Join(values, " ")
}

func completionZshWords(values []string) string {
	return strings.Join(values, " ")
}

func completionPowerShellWords(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return "@(" + strings.Join(quoted, ", ") + ")"
}

func renderBashCompletion() string {
	var out strings.Builder
	out.WriteString(`# bash completion for agentsync
_agentsync_completion() {
    local cur="${COMP_WORDS[COMP_CWORD]}"
    local cmd="${COMP_WORDS[1]}"
    local candidates=""
    if (( COMP_CWORD == 1 )); then
`)
	fmt.Fprintf(&out, "        candidates=\"%s\"\n", completionWords(completionCommandNames()))
	out.WriteString(`    else
        case "$cmd" in
`)
	for _, command := range completionCommandNames() {
		candidates := completionCandidates(command)
		if len(candidates) == 0 {
			continue
		}
		fmt.Fprintf(&out, "            %s) candidates=\"%s\" ;;\n", command, completionWords(candidates))
	}
	out.WriteString(`        esac
    fi
    COMPREPLY=( $(compgen -W "$candidates" -- "$cur") )
}

complete -F _agentsync_completion agentsync
`)
	return out.String()
}

func renderZshCompletion() string {
	var out strings.Builder
	out.WriteString(`#compdef agentsync

_agentsync() {
    local -a candidates
    if (( CURRENT == 2 )); then
`)
	fmt.Fprintf(&out, "        candidates=(%s)\n", completionZshWords(completionCommandNames()))
	out.WriteString(`    else
        case "$words[2]" in
`)
	for _, command := range completionCommandNames() {
		candidates := completionCandidates(command)
		if len(candidates) == 0 {
			continue
		}
		fmt.Fprintf(&out, "            %s) candidates=(%s) ;;\n", command, completionZshWords(candidates))
	}
	out.WriteString(`        esac
    fi
    _describe 'agentsync values' candidates
}

compdef _agentsync agentsync
`)
	return out.String()
}

func renderFishCompletion() string {
	var out strings.Builder
	out.WriteString(`# fish completion for agentsync
complete -c agentsync -f -n '__fish_use_subcommand' -a '`)
	out.WriteString(completionWords(completionCommandNames()))
	out.WriteString(`'
`)
	for _, command := range completionCommandNames() {
		candidates := completionCandidates(command)
		if len(candidates) == 0 {
			continue
		}
		fmt.Fprintf(&out, "complete -c agentsync -f -n '__fish_seen_subcommand_from %s' -a '%s'\n", command, completionWords(candidates))
	}
	return out.String()
}

func renderPowerShellCompletion() string {
	var out strings.Builder
	out.WriteString(`# PowerShell completion for agentsync
Register-ArgumentCompleter -Native -CommandName agentsync -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $commandElements = $commandAst.CommandElements | ForEach-Object { $_.Extent.Text }
    $candidates = `)
	fmt.Fprintf(&out, "%s\n", completionPowerShellWords(completionCommandNames()))
	out.WriteString(`    if ($commandElements.Count -gt 1) {
        switch ($commandElements[1]) {
`)
	for _, command := range completionCommandNames() {
		candidates := completionCandidates(command)
		if len(candidates) == 0 {
			continue
		}
		fmt.Fprintf(&out, "            '%s' { $candidates = %s }\n", command, completionPowerShellWords(candidates))
	}
	out.WriteString(`        }
    }
    $candidates | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
`)
	return out.String()
}
