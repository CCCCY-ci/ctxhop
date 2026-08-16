# Spec: `agentsync completion`

| | |
|---|---|
| Status | Implemented locally |
| Supports | Bash, Zsh, Fish, PowerShell (`powershell` or `pwsh`) |

## 1. Contract

`agentsync completion <shell>` writes a self-contained completion script to
stdout. It does not load configuration, contact the backend, read session
data or prompt for secrets.

The command exits with an error for a missing or unsupported shell. The
supported names are `bash`, `zsh`, `fish`, `powershell` and the `pwsh` alias.

## 2. Candidate policy

Top-level candidates come from the declarative CLI command table. Static
subcommands, modes and flags are supplied for the commands implemented in the
current build, including `history cleanup/prune`, remote lifecycle actions,
workspace-restore safety flags and project/device modes.

Completion never queries the remote or enumerates local session IDs. Session
and path arguments are left to the shell's normal file/argument behavior, so
installing completion cannot disclose remote metadata.

## 3. Installation examples

```sh
# Bash
source <(agentsync completion bash)

# Zsh
source <(agentsync completion zsh)

# Fish
agentsync completion fish | source

# PowerShell
agentsync completion powershell | Invoke-Expression
```

For persistent installation, redirect the output to the user's normal shell
completion directory and source it from that shell's startup configuration.

## 4. Test plan

Tests cover all supported shell names, the `pwsh` alias, command and safety
flag candidates, command registration and invalid shell handling.