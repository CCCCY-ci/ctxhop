# CtxHop

<p align="center">
  <img src="assets/ctxhop-logo.png" alt="CtxHop logo" width="180">
</p>

<p align="center">
  <a href="https://github.com/CCCCY-ci/ctxhop/releases/latest"><img src="https://img.shields.io/github/v/release/CCCCY-ci/ctxhop?sort=semver" alt="Latest release"></a>
  <a href="https://github.com/CCCCY-ci/ctxhop/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="Apache-2.0 License"></a>
</p>

English | [简体中文](README.zh-CN.md)

**Switch devices. Keep your context.**

CtxHop is a cross-device session and workspace synchronization tool for Claude
Code, Codex, and other AI coding agents. Start development on one device,
restore the original Session on another, and continue working without keeping
the source device online.

CtxHop synchronizes Agent Sessions by project and can optionally include a
limited set of workspace files and Git state. Data is encrypted locally before
it leaves the device. You control the storage backend, which can be a local
directory or an S3-compatible object store such as Cloudflare R2.

## Key Features

- **Continue Agent Sessions across devices**: Synchronize project-scoped Claude
  Code and Codex Sessions so an authorized device can restore and continue the
  existing development context.
- **Optional workspace handoff**: Use --workspace to include a limited set of
  project files and Git state, including work that has not been committed.
- **Project-scoped synchronization boundaries**: Only explicitly bound
  projects and authorized devices participate in synchronization. Unrelated
  projects are not scanned or transferred.
- **Self-hosted storage**: Use a local directory or an S3-compatible object
  store, including Cloudflare R2.
- **Safe restore**: Use resume --preview to inspect the restore before any
  files are written. A failed conflict check leaves the target workspace
  unchanged.

## Demo

[Watch the CtxHop demo video](assets/ctxhop-demo.mp4)

## Installation

### Windows

Download the installer for your CPU architecture from
[Releases](https://github.com/CCCCY-ci/ctxhop/releases):

- CtxHop-Setup_<version>_windows_amd64.exe
- CtxHop-Setup_<version>_windows_arm64.exe

Run the installer. When installation completes, CtxHop opens a Command Prompt
that displays its ASCII logo and the next step. You can continue directly with:

~~~powershell
ctxhop init
~~~

The installer places CtxHop in %USERPROFILE%\.ctxhop\bin and adds that
directory to the current user's PATH. Administrator privileges are not
required. Windows releases provide an installer rather than a portable
standalone ctxhop.exe.

### macOS / Linux

Download the ZIP archive for the target platform and install it from a
terminal:

~~~bash
unzip ctxhop_<version>_<os>_<arch>.zip
sh install.sh
~~~

The installer uses $XDG_BIN_HOME when set, otherwise $HOME/.local/bin.
Set CTXHOP_INSTALL_DIR to select another user-level installation directory:

~~~bash
CTXHOP_INSTALL_DIR=/path/to/bin sh install.sh
~~~

If the installation directory is not on PATH, the script prints the required
shell configuration. Run the script from a terminal rather than double-clicking
the binary, then open a new shell and verify the installation:

~~~bash
ctxhop version
~~~

### Go Install

~~~bash
go install github.com/CCCCY-ci/ctxhop/cmd/ctxhop@latest
~~~

Use a release tag instead of latest when a reproducible installation is
required.

### Uninstall

~~~bash
ctxhop uninstall
~~~

Uninstalling removes the CLI, the local `~/.ctxhop` configuration directory,
device keys, local state, logs, and CtxHop-installed agent hooks. Remote S3
objects and a configured local directory backend are left untouched.
If a directory backend overlaps the local CtxHop directory, uninstall stops
and asks you to move the backend first.

## Quick Start: Cloudflare R2

This example uses Cloudflare R2 as the shared backend. The same workflow works
with other S3-compatible object stores. The complete workflow has four steps:
initialize device A, bind and push a project, authorize device B, and restore a
Session.

Before starting, prepare an R2 bucket, its Access Key and Secret Access Key,
and a working copy of the project on both devices.

Example R2 configuration:

~~~text
Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
Bucket:   <BUCKET_NAME>
Region:   auto
Prefix:   ctxhop/demo     # optional
~~~

All devices in the same sync domain must use the same bucket and prefix. When
multiple sync domains share one bucket, use different prefixes to keep their
data separate.

### 1. Initialize device A

~~~bash
ctxhop init --backend s3 --endpoint "https://<ACCOUNT_ID>.r2.cloudflarestorage.com" --bucket "<BUCKET_NAME>" --region "auto" --prefix "ctxhop/demo" --device-name "device-a"
~~~

Follow the prompts for the R2 credentials and encryption password. A standard
R2 API token does not use a session token, so leave that prompt empty.

The first initialization generates a **Recovery Key**. Store it offline. If
both the encryption password and Recovery Key are lost, the encrypted remote
data cannot be recovered.

During initialization, CtxHop checks for Claude Code and Codex before asking
about Agent-specific options. If at least one of them is detected, choose the
automatic SessionEnd Hook scope by entering `1` or `2`:

- `1` — `session`: the Session and its filtered environment;
- `2` — `session+workspace`: the same data plus the project workspace and Git state.

After the Hook step (or after `--no-hook` skips Hook installation), CtxHop asks
whether to synchronize filtered configuration for the detected Agent(s). The
default is enabled. The same choice applies to Claude Code and Codex when both
are installed. Only allowlisted settings and non-sensitive MCP transport
intents are eligible for synchronization.

CtxHop never uploads a raw Agent configuration file or authentication file.
This includes Claude Code credentials and login state, Codex auth data, tokens,
headers, environment variables, and MCP `env` values. A Claude user
configuration file may be read locally to extract a referenced MCP command,
arguments, or URL, but the file itself is never uploaded. Answer `no` to
keep the Session and other allowed environment components while excluding
settings and MCP configuration from future uploads.

The Hook runs `push` with the selected scope when a Session ends. After it is
installed, each completed conversation is uploaded automatically, so you do
not need to run `ctxhop push` after every conversation. You can install the
Hook later:

~~~bash
ctxhop hook install --agent codex
# or
ctxhop hook install --agent claude-code
~~~

Use --no-hook during initialization when the Hook should not be installed.

To switch an initialized device to another sync domain, run `ctxhop init`
again. CtxHop asks whether to leave the current domain first:

- Enter `y` to leave it and continue initialization. This removes the local
  configuration, device keys, and CtxHop-installed Hooks. Remote S3 objects and
  an independent directory backend are kept.
- Enter `n` or press Enter to keep the current domain and cancel initialization.

If the configured directory backend overlaps the local CtxHop configuration
directory, move the backend first; CtxHop will stop before removing the local
domain state.

### 2. Bind the project and push

Run these commands from the project directory on device A:

~~~bash
cd /path/to/project
ctxhop project bind --path .
ctxhop push
~~~

The default push synchronizes the current project's Session and filtered
environment. To include uncommitted workspace files and Git state, use:

~~~bash
ctxhop push --workspace
~~~

For a project without a usable Git identity, use the same manual project name
on both devices:

~~~bash
ctxhop project bind --name "my-project" --path .
~~~

The manual name identifies the project and is independent of its local path.

`pull`, `list`, and `resume` query only the project bound to the current
directory. `ctxhop project discover` is the exception: it intentionally lists
all projects announced in the sync domain so an authorized device can find a
new project before binding it.

### 3. Authorize device B

Create an invitation on device A:

~~~bash
ctxhop device invite --output ctxhop-device-b.json
~~~

Transfer the invitation file to device B through a trusted channel, then run:

~~~bash
ctxhop init --invite ./ctxhop-device-b.json --device-name "device-b"
~~~

Device B must enter its own R2 credentials and the same encryption password.
The invitation does not contain storage credentials, the encryption password,
or Session contents.

### 4. Restore the Session

To see projects announced by authorized devices:

~~~bash
ctxhop project discover
~~~

Prepare the corresponding project working copy on device B and bind it:

~~~bash
cd /path/to/project
ctxhop project bind --path .
ctxhop list
~~~

list displays the Sessions available for the current project. When a
SessionEnd Hook is installed on the source device, the latest completed
conversation is already uploaded. Run `list` or `resume` on the target device
to read the latest remote metadata; a separate `ctxhop pull` is not required
for the normal handoff flow. Use `pull` when you specifically want a
metadata-only remote check.

Restore a Session:

~~~bash
ctxhop resume <SESSION_ID>
~~~

Preview the restore without writing anything:

~~~bash
ctxhop resume --preview <SESSION_ID>
~~~

If device A used push --workspace, restore the uploaded workspace and Git
state as well:

~~~bash
ctxhop resume --workspace <SESSION_ID>
~~~

resume --workspace checks the target workspace and Git state before writing.
If a conflict is found, it stops without forcing changes to the target.

If you only need the Session and do not want differences in the target
workspace to block the restore:

~~~bash
ctxhop resume --allow-divergent <SESSION_ID>
~~~

After the restore, open the Session with the native Agent command:

~~~bash
# Codex
codex resume <SESSION_ID>

# Claude Code
claude --resume <SESSION_ID>
~~~

You can then continue the existing Session without starting a new conversation
or repeating its previous context.

## Synchronized Data

| Data | Default | Description |
|---|---|---|
| Agent Session | Synchronized | Compressed when useful and encrypted locally. |
| Project identity and Git summary | Synchronized | Used to identify the project across devices; project files and complete Git objects are not included. |
| Session-related environment | Filtered; configuration selectable during `init` | Claude Code and Codex use their adapters to restore only allowlisted Skills, MCP transport intents, and Session settings. Global settings remain user-scoped and project MCP remains project-scoped. Raw Agent configuration files and authentication data are never uploaded. |
| Workspace and Git state | Optional | Processed only with push --workspace and resume --workspace. |
| Tokens, credentials, and .env files | Never synchronized | Login state, Claude Code/Codex auth files, private keys, headers, MCP `env` values, and secrets are excluded. |

Normal push, Hooks, and watch do not upload project file contents. When
--workspace is enabled, CtxHop also does not delete local files, switch
branches, or run merge, rebase, commit, push, or reset.

Sensitive files, binary files, files over the size limit, unavailable files,
and conflicting paths are left for manual handling.

## CLI

Run `ctxhop <command> --help` for the complete options of a command. Running
`ctxhop` without arguments prints the command index. Use `ctxhop help
<command>` for command details. The table includes the supported top-level
commands and their second-level actions.

### Setup and navigation

| Command | Description |
|---|---|
| `ctxhop init` | Configure the storage backend, encryption password, device, and Agent hooks. |
| `ctxhop install` | Install CtxHop as a user-level command. |
| `ctxhop uninstall` | Remove local CtxHop files without deleting remote or backend data. |
| `ctxhop help [<command>]` | Show the command index or details and available options. |
| `ctxhop version` | Show the installed version. |

### Project and synchronization

| Command | Description |
|---|---|
| `ctxhop project bind` | Bind a local project to a stable project identity. |
| `ctxhop project unbind` | Remove a local project binding. |
| `ctxhop project mode <mode>` | Set a project to `normal`, `push-only`, or `excluded` mode. |
| `ctxhop project list` | List local project bindings. |
| `ctxhop project discover` | View projects announced by other devices. |
| `ctxhop push` | Upload the current project's Session and filtered environment. |
| `ctxhop push --workspace` | Also upload the limited workspace and Git state. |
| `ctxhop pull` | Check remote metadata without restoring anything. |
| `ctxhop list` | List Sessions available for the current project. |
| `ctxhop resume` | Restore a Session and its filtered environment. Use `--preview`, `--workspace`, or `--allow-divergent` as needed. |
| `ctxhop watch` | Watch local Session changes and upload them. |

### Devices and security

| Command | Description |
|---|---|
| `ctxhop device invite` | Create an invitation for a new device. |
| `ctxhop device status` | Show the local device identity and mode. |
| `ctxhop device mode <mode>` | Set the device to `normal`, `push-only`, or `disabled` mode. |
| `ctxhop device list` | List authorized devices. |
| `ctxhop device rename <name>` | Change the local device display name. |
| `ctxhop device remove <device-id>` | Revoke a device's future access. |
| `ctxhop device rotate-key` | Rotate the encryption key. |
| `ctxhop passphrase change` | Change the encryption password. |
| `ctxhop passphrase reset` | Reset the encryption password with the Recovery Key. |

### History and remote data

| Command | Description |
|---|---|
| `ctxhop history <SESSION_ID>` | List recoverable versions for a Session. |
| `ctxhop history cleanup <SESSION_ID>` | Delete all remote versions for a Session. |
| `ctxhop history prune <SESSION_ID>` | Keep selected Session versions or remove versions before a date. |
| `ctxhop remote delete-session <SESSION_ID>` | Delete one remote Session after confirmation. |
| `ctxhop remote delete-project` | Delete all remote data for the current project after confirmation. |
| `ctxhop remote delete-all` | Delete all remote data in the current sync domain after confirmation. |

### Status and maintenance

| Command | Description |
|---|---|
| `ctxhop status` | Show local status; `--remote` also checks the backend. |
| `ctxhop doctor` | Check configuration, backend, Agent, and project status. |
| `ctxhop stats` | Show cross-device restore statistics. |
| `ctxhop hook install` | Install the Claude Code or Codex SessionEnd Hook. |

## Configuration

CtxHop stores local configuration, device keys, and synchronization state in
~/.ctxhop by default:

| System | Default directory |
|---|---|
| Windows | %USERPROFILE%\.ctxhop |
| macOS / Linux | ~/.ctxhop |

Set CTXHOP_CONFIG_DIR to use another directory:

~~~bash
export CTXHOP_CONFIG_DIR="$HOME/.ctxhop-custom"
~~~

PowerShell:

~~~powershell
$env:CTXHOP_CONFIG_DIR = Join-Path $env:USERPROFILE '.ctxhop-custom'
~~~

This directory contains local encrypted data and device keys. Do not commit it
to a repository or share it publicly.

## Development

Build from source:

~~~bash
git clone https://github.com/CCCCY-ci/ctxhop.git
cd ctxhop
go build -trimpath -o ctxhop ./cmd/ctxhop
./ctxhop install
~~~

On Windows PowerShell:

~~~powershell
go build -trimpath -o ctxhop.exe ./cmd/ctxhop
.\ctxhop.exe install
~~~

Run checks:

~~~bash
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o ctxhop ./cmd/ctxhop
~~~

Do not commit real Session files, tokens, or backend credentials.

## License

CtxHop is licensed under the [Apache License 2.0](LICENSE). Retain the
[NOTICE](NOTICE) file when redistributing the project.
