# AgentSync

[简体中文](README.zh-CN.md) | English

AgentSync is a command-line tool for synchronizing Claude Code and Codex CLI session history
between computers. It encrypts session data locally, stores the encrypted data
in a local directory or S3-compatible object storage, and restores a selected
session on another device.

AgentSync syncs session records and a small encrypted manifest of structured
agent/tool dependencies observed in those sessions. For a Codex skill actually
referenced by a session, it may also include a filtered, non-sensitive `SKILL.md`
body. For an observed Codex MCP server, it may include an allowlisted non-secret
intent record with a command basename, safe arguments and startup timeout. Project
files are not included in normal `push`, Hook or `watch` runs. An explicit
`push --include-workspace` can add a bounded, filtered snapshot containing only
files selected by the session fingerprint for Git projects, or a filtered directory scan for projects without Git. It never includes credentials,
tokens, key material, `.env` files or `.git` data. For a Codex session, it may include
an allowlisted summary of the model, provider and effort recorded in structured session
metadata. The target device must already have the relevant agent and project checkout.

Status: pre-alpha. The current implementation covers directory and S3 storage,
project binding, device pairing, key rotation, Claude Code and Codex adapters, SessionEnd hooks,
restore safety checks, read-only environment previews, explicit application of filtered
Codex Skill files and bounded workspace snapshots with explicit upload/apply steps; MCP
intent and session-setting components remain preview-only.

## Quick start

This example uses Cloudflare R2 as shared storage. Device A uploads a session;
device B lists and restores it.

### Before you start

- Claude Code or Codex CLI is installed on both devices.
- Both devices have the relevant project checkout.
- You have an R2 bucket and an R2 S3 API token.
- The token can list objects and can put, read and delete objects. init uses a
  temporary object for its storage probe.

Use the account-level R2 endpoint. Pass the bucket separately:

~~~text
Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
Bucket:   <BUCKET_NAME>
Region:   auto
Prefix:   agentsync/demo
~~~

An administrator account on the computer is normally not required.

### 1. Install AgentSync

Build it from source if you do not already have a binary:

~~~bash
git clone https://github.com/CCCCY-ci/agentsync.git
cd agentsync
go build -trimpath -o agentsync ./cmd/agentsync
~~~

Register the binary as a user-level command:

~~~bash
./agentsync install
~~~

On Windows PowerShell:

~~~powershell
go build -trimpath -o agentsync.exe ./cmd/agentsync
.\agentsync.exe install
~~~

The command installs the binary into a user directory. Windows adds that
directory to the user PATH without requiring administrator access. On Unix,
follow the PATH command printed by `install` if `~/.local/bin` is not already
on PATH. Open a new terminal, then run:

~~~bash
agentsync version
~~~

Use `--dir DIR` to choose another install directory. Use `--no-path` to copy
the binary without changing PATH.

### 2. Initialize device A

Run this on device A. Replace the values in angle brackets:

~~~bash
./agentsync init --backend s3 \
  --endpoint "https://<ACCOUNT_ID>.r2.cloudflarestorage.com" \
  --bucket "<BUCKET_NAME>" \
  --region "auto" \
  --prefix "agentsync/demo" \
  --device-name "device-a"
~~~

init prompts for the R2 access key, secret key, optional session token and
encryption password. Secret input is hidden. For a normal R2 API token, leave
the session token empty.

The first device prints a Recovery Key. Save it offline and confirm by typing
`saved` when asked. Losing both the encryption password and Recovery Key means
the encrypted data cannot be recovered.

If Claude Code or Codex CLI is detected, init asks whether to install its SessionEnd hook.
Enter `y` for automatic push after a session ends, or press Enter to skip it.
Use `--no-hook` for non-interactive setup or when the hook is not wanted.

For Codex, restart Codex after installation, run /hooks, and trust the
AgentSync hook. If you already ran init, install the hook without
reinitializing the sync domain:

~~~bash
./agentsync hook install --agent codex
~~~

Use --agent claude-code for Claude Code or omit --agent to configure every
detected supported agent.

### 3. Bind the project and push

On device A:

~~~bash
cd /path/to/project
./agentsync project bind --path .
./agentsync push

# Optional: explicitly include a bounded workspace snapshot. Git projects use the session fingerprint; no-Git projects scan the safe project directory.
./agentsync push --include-workspace
~~~

Normal push also records a small encrypted Git state summary (repository HEAD,
branch, upstream and dirty paths). It does not upload Git objects or project
files. If you explicitly need to carry local-only commits or uncommitted
tracked/untracked work to another checkout, run:

~~~bash
./agentsync push --include-git-state
~~~

This creates Git-native bundles after a sensitive-content preflight. It never
uploads the whole .git directory. The target device must apply the transfer
explicitly; commits are imported into a hidden AgentSync ref and the current
branch is left unchanged.

If you want to transfer an existing stash instead of the current worktree,
inspect the available refs and select one explicitly:

~~~bash
git stash list
./agentsync push --git-stash 'stash@{0}'
~~~

`--git-stash` automatically enables the explicit Git transfer path. It replaces
the worktree part of the transfer with the selected stash; the original stash
is only read and is not applied, changed or deleted. The current worktree
changes are not included in that worktree bundle. The selected stash still
goes through the same path and content safety checks.

For a project without a usable Git identity:

~~~bash
./agentsync project bind --name "my-project" --path .
./agentsync push
~~~

Use the same manual name on device B. Binding is local; push only uploads
sessions for the current project. To sync another project, enter that project
and bind it separately.

### 4. Pair device B

Create an invitation on device A:

~~~bash
./agentsync device invite --output agentsync-device-b.json
~~~

Transfer the JSON file to device B over a trusted channel. It contains the
remote settings and a signed sync-domain proof. It does not contain R2
credentials, the encryption password or session contents.

Initialize device B:

~~~bash
./agentsync init --invite ./agentsync-device-b.json --device-name "device-b"
~~~

Enter the R2 credentials when prompted and use the same encryption password as
device A. Do not combine `--invite` with backend options such as
`--endpoint`, `--bucket`, `--region` or `--prefix`.

### 5. List and restore on device B

An authorized device can discover projects announced by other devices:

~~~bash
./agentsync project discover
~~~

This only lists project identities. It does not clone a repository or bind a
local directory. Prepare the project checkout on device B, then bind it:

~~~bash
cd /path/to/the/same/project
./agentsync project bind --path .
./agentsync pull --check
./agentsync list
~~~

Before restoring, you can inspect the dependency references recorded for the session:

~~~bash
./agentsync env preview <NATIVE_SESSION_ID>
~~~

This is a read-only preview. It does not install, apply or execute anything. If a safe
Codex Skill, MCP intent or session-setting component was captured, the preview shows only
its kind, scope, size and fingerprint; it never prints or applies the component body.

`env preview` is the read-only way to inspect local component differences. `env apply` also
supports a no-write confirmation step; without `--yes` it only prints the changes:

~~~bash
./agentsync env apply <NATIVE_SESSION_ID>
~~~

After checking the output, add `--yes` to explicitly apply filtered Codex Skill files. AgentSync
creates a backup before replacing an existing file:

~~~bash
./agentsync env apply --yes <NATIVE_SESSION_ID>
~~~

MCP and session-setting components remain preview-only. AgentSync does not install tools,
change raw MCP configuration or execute commands.

If device A used `push --include-workspace`, device B can inspect and explicitly apply
that filtered workspace snapshot. Preview does not write files; `--yes` writes only
available file bodies and backs up an existing file before replacement:

~~~bash
./agentsync workspace preview <NATIVE_SESSION_ID>
./agentsync workspace apply <NATIVE_SESSION_ID>
./agentsync workspace apply --yes <NATIVE_SESSION_ID>
~~~

Git projects use files selected by the session fingerprint; no-Git projects use a filtered directory scan. Unavailable,
sensitive, binary or oversized bodies remain manual items. A no-Git snapshot may show local-only files as deletion candidates. It never deletes local files,
switches branches, commits, stashes or runs Git commands.

Before restoring, inspect the Git state that was recorded with the session:

~~~bash
./agentsync git preview <NATIVE_SESSION_ID>
./agentsync git apply <NATIVE_SESSION_ID>
./agentsync git apply --yes <NATIVE_SESSION_ID>
~~~

git preview is read-only. git apply without --yes is also a preview. With
--yes, an explicit transfer can import unpushed commits into a hidden
refs/agentsync/... ref and apply the worktree snapshot only when the target
worktree is clean and its HEAD matches the recorded base. Before applying,
AgentSync also checks the snapshot paths against existing files and directories.
An untracked or ignored path at a path the snapshot would write is reported as a
conflict, even when `git status` is otherwise clean, and the target is left
unchanged. It does not switch branches, merge, rebase, commit or push. If Git
fails after worktree application starts, the result is marked partial and the
command tells you to inspect `git status`; AgentSync does not run an automatic
reset or delete files.

When a commit bundle is imported, the output records the hidden commit ref,
source base and target branch for manual review. Inspect the ref with
`git log --oneline --reverse <COMMIT_REF>`, then use normal Git operations if
you decide to integrate it. AgentSync does not run that integration. Re-running
`git apply --yes` for the same transfer after a successful apply reports
`already-applied` and does not change the worktree again. A previous partial
apply that requires cleanup must be resolved manually before retrying. After
the same preflight passes, you can rerun `git apply --yes`; AgentSync still does not
reset or delete files automatically.
Restore the session ID printed by `list`:

~~~bash
# Use this when the target workspace matches the source workspace.
./agentsync resume <NATIVE_SESSION_ID>

# Use this when the project path or workspace differs on device B.
./agentsync resume --allow-divergent <NATIVE_SESSION_ID>
~~~

After a successful restore, run:

~~~bash
claude --resume
~~~

Claude Code will show its session list, including the session restored by
AgentSync.

`pull --check` reads remote metadata only. `env preview` shows structured dependency
references and local component differences when the session recorded them. `workspace preview`
shows the bounded workspace snapshot only when the source used `push --include-workspace`.
`resume` downloads the selected encrypted session and restores it to Claude Code. `env apply`
without `--yes` only reports changes; `env apply --yes` writes filtered Codex Skill files
with a backup. `workspace apply --yes` similarly writes only available, filtered workspace
bodies after a backup. Neither command installs tools, changes raw MCP configuration or
executes commands.

When restoring a session:

- AgentSync checks the related project files first. This check is about project
  files, not session contents. If those files differ from the source device,
  the restore stops and no session file is written.
- Add `--allow-divergent` to continue anyway. It only restores the session; it
  does not change or sync project files.
- `workspace: divergent` means the session was restored, but the related project
  files on this device are different from the source device.
- `workspace context: injected` means a local difference note was added to the
  restored conversation. It is not uploaded to the remote.
- `workspace verdict is divergent` means the restore was stopped and no session
  file was written.

For example:

~~~text
resumed: 将sidecar服务迁移到浏览器插件架构
session: b9dcdfcc-0470-4692-a9d9-cb3d9c6e8c6d
workspace: divergent (1 file differences)
workspace context: injected
~~~

### 6. Add another project

AgentSync does not scan every directory automatically. Bind each project that
you want to sync:

~~~bash
cd /path/to/another/project
./agentsync project bind --path .
./agentsync push
~~~

For a no-Git project, use the same `--name` on both devices.

## CLI

All currently supported commands are listed below. Session commands use the
current project unless the command provides a path option. Destructive commands
ask for confirmation unless `--yes` is supplied.

| Command | Description |
|---|---|
| `agentsync help` | Show command usage. |
| `agentsync version` | Show version, commit, build time and runtime information. |
| `agentsync completion bash, zsh, fish, powershell, pwsh` | Generate shell completion. `pwsh` is an alias for `powershell`. |
| `agentsync init [--invite FILE or backend options]` | Create or join the encrypted sync domain and write local configuration. Use `--invite` to join from another device; it can install the Claude Code or Codex SessionEnd hook. |
| `agentsync hook install [--agent all|claude-code|codex]` | Install a supported agent SessionEnd hook for automatic push. Existing hooks are preserved; restart Codex and trust it in /hooks after installation. |
| `agentsync install [--dir DIR] [--no-path]` | Install the current executable in a user-level command directory; Windows updates the user PATH. |
| `agentsync status [--json] [--remote]` | Show local status; `--remote` checks remote metadata. |
| `agentsync doctor [--json]` | Diagnose configuration, backend access, Agent installation, project identity and recent local errors. |
| `agentsync project bind [--path DIR] [--name NAME or --identity ID]` | Bind a local project. Use `--name` for a no-Git project. |
| `agentsync project unbind [--path DIR or --identity ID]` | Remove a local project binding. |
| `agentsync project mode normal / push-only / excluded [--path DIR or --identity ID]` | Set a project's synchronization policy. |
| `agentsync project list [--json]` | List bound projects and their policies. |
| `agentsync project discover [--json]` | List projects announced by authorized devices. It does not bind or clone them. |
| `agentsync push [--include-workspace] [--include-git-state] [--git-stash STASH_REF] [--session SESSION_ID or SESSION_ID] [--agentsync-hook]` | Upload new records and encrypted Git metadata for the current project. --include-git-state explicitly uploads Git-native commit/worktree transfer data; --git-stash selects an existing `stash@{N}` and implies --include-git-state; --include-workspace is the separate bounded file snapshot. |
| `agentsync watch [--interval DURATION] [--once] [--json]` | Repeatedly scan and push the current project; `--once` performs one scan. |
| `agentsync pull --check [--json]` | Check encrypted remote metadata without downloading session bodies. |
| `agentsync list [--json]` | List sessions available for the current project. |
| `agentsync env preview [--json] SESSION_ID` | Show structured dependency references and local component differences. This is read-only. |
| `agentsync env apply [--yes] [--json] SESSION_ID` | Show component changes; only with `--yes` write filtered Codex Skill files, with a backup before replacement. MCP/settings remain preview-only. |
| `agentsync workspace preview [--json] SESSION_ID` | Compare the explicit, bounded workspace snapshot with the current project; read-only and never prints file bodies. |
| `agentsync workspace apply [--yes] [--json] SESSION_ID` | Show workspace changes; only with `--yes` write available filtered file bodies, backing up existing files first. It never deletes files or runs Git commands. |
| `agentsync git preview/apply [--yes] [--json] SESSION_ID` | Inspect or explicitly apply the recorded Git state. preview and apply without --yes are read-only; apply --yes only imports hidden refs and applies a matching clean worktree. |
| `agentsync resume [restore options] [SESSION_ID]` | Download and restore one session. Options include `--version`, `--allow-limited`, `--allow-divergent`, `--no-workspace-context` and `--replace-existing`; put options before the session ID. |
| `agentsync history SESSION_ID [--json]` | Show recoverable versions and forks for a session. |
| `agentsync history cleanup SESSION_ID [cleanup options]` | Delete one session; an alias for `remote delete-session`. |
| `agentsync history prune SESSION_ID --keep N or --before RFC3339` | Delete old session versions using one retention rule. |
| `agentsync stats [--json]` | Show local cross-device restore statistics. |
| `agentsync device status [--json]` | Show this device's local mode. |
| `agentsync device mode normal / push-only / disabled` | Change this device's mode. |
| `agentsync device list [--json]` | List authorized devices in the sync domain. |
| `agentsync device rename NAME` | Change this device's display name. |
| `agentsync device invite [--output FILE]` | Create a signed invitation for another device. |
| `agentsync device rotate-key` | Publish a new encryption-key generation after saving its Recovery Key. |
| `agentsync device remove DEVICE_ID [--yes]` | Revoke a device for future generations and remove its remote branch. |
| `agentsync passphrase change` | Change the encryption password using the current password. |
| `agentsync passphrase reset` | Reset the encryption password with the existing Recovery Key. |
| `agentsync remote delete-session SESSION_ID [--remote-id] [--yes]` | Delete one remote session. |
| `agentsync remote delete-project [--path DIR] [--yes]` | Delete all remote sessions for one project. |
| `agentsync remote delete-all [--yes]` | Delete all objects in the configured sync-domain namespace. |

Run `agentsync <command> --help` for command-specific flags. Use `--json`
where available for automation.

## Configuration

### Configuration directory

Without `AGENTSYNC_CONFIG_DIR`, AgentSync uses the visible per-user directory
`~/.agentsync` on every platform:

| Platform | Default directory |
|---|---|
| Windows | `%USERPROFILE%\.agentsync` |
| macOS | `~/.agentsync` |
| Linux and other Unix systems | `~/.agentsync` |

`init` prints the exact directory after a successful initialization. Override it
when you need a custom configuration directory:

~~~bash
export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync-custom"
~~~

PowerShell:

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = Join-Path $env:USERPROFILE '.agentsync-custom'
~~~

The directory contains configuration, encrypted secrets, the device key and
local state. Do not commit or publish it. Claude Code's data directory is
separate and can be selected with `CLAUDE_CONFIG_DIR`.

For CI or short-lived tests, set `AGENTSYNC_ACCESS_KEY_ID` and
`AGENTSYNC_SECRET_ACCESS_KEY`; `AGENTSYNC_SESSION_TOKEN` is optional.
Environment credentials are not written to disk.

## Limitations and safety

- Session data and metadata are encrypted before upload.
- Each device has its own device ID and remote branch.
- `push` writes the current device's branch; it does not pull that branch back.
- `pull --check` reads metadata. `resume` is the explicit body download and
  restore operation.
- Project file bodies are not uploaded by default: normal push, Hook and watch runs only
  synchronize session, environment and small Git-state metadata. push --include-workspace
  adds a bounded snapshot: Git projects use fingerprint-selected files, while no-Git projects scan the safe project directory.
  push --include-git-state is a separate explicit operation for Git-native commit and
  worktree transfer bundles. It runs a sensitive-content preflight and fails closed when
  the content cannot be safely inspected. No whole .git directory, token, credential,
  key material or .env file is uploaded. git preview is read-only; git apply --yes
  imports commits into a hidden ref and applies a worktree snapshot only on a clean matching
  base. It never switches branches, commits, merges, rebases or pushes.
- The target device must already have Claude Code and the project prepared.
- Git projects provide stronger workspace checks. No-Git projects use a manual identity;
  normal workspace context uses a touched-file fallback, while explicit --include-workspace uses a bounded directory scan.
- A server without Claude Code can store data and run administrative checks, but
  it cannot push or natively restore Claude sessions.
- If both the encryption password and Recovery Key are lost, encrypted data is
  unrecoverable.

### Common setup errors

- **Backend probe failed:** use the account-level R2 endpoint, pass the bucket
  separately, set region to `auto`, and check object list/read/write/delete
  permissions.
- **Passphrase does not unlock storage:** use the encryption password belonging
  to the existing sync domain.
- **Already configured:** use the existing configuration or set a new
  `AGENTSYNC_CONFIG_DIR`; init does not overwrite a valid configuration.
- **No sessions on device B:** bind the same project identity on both devices
  and make sure device A has completed `push`.

## Development

~~~bash
go test ./...
go build -trimpath -o agentsync ./cmd/agentsync
~~~

## License

AgentSync is licensed under the [Apache License 2.0](LICENSE). Keep the
[NOTICE](NOTICE) file when redistributing the project.
