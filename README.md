# AgentSync

[简体中文](README.zh-CN.md) | English

AgentSync is a command-line tool for synchronizing Claude Code session history
between computers. It encrypts session data locally, stores the encrypted data
in a local directory or S3-compatible object storage, and restores a selected
session on another device.

AgentSync syncs session records, not development environments. It does not copy
project files, uncommitted changes, Claude Code settings, skills, MCP servers,
credentials or environment variables. The target device must already have
Claude Code and the relevant project checkout.

Status: pre-alpha. The current implementation covers directory and S3 storage,
project binding, device pairing, key rotation and restore safety checks.

## Quick start

This example uses Cloudflare R2 as shared storage. Device A uploads a session;
device B lists and restores it.

### Before you start

- Claude Code is installed on both devices.
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
./agentsync version
~~~

Copy the binary to both devices. On Windows PowerShell, use
`.\agentsync.exe` in place of `./agentsync`.

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

If Claude Code is detected, init asks whether to install the SessionEnd hook.
Enter `y` for automatic push after a session ends, or press Enter to skip it.
Use `--no-hook` for non-interactive setup or when the hook is not wanted.

### 3. Bind the project and push

On device A:

~~~bash
cd /path/to/project
./agentsync project bind --path .
./agentsync push
~~~

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

When simulating two devices on one computer, set a different configuration
directory before each initialization:

~~~bash
export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync-device-a"
# run device A init

export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync-device-b"
# run device B init
~~~

On two separate computers, the default per-user configuration directories are
normally enough.

### 5. List and restore on device B

Prepare the same project on device B and bind it:

~~~bash
cd /path/to/the/same/project
./agentsync project bind --path .
./agentsync pull --check
./agentsync list
~~~

Restore the session ID printed by `list`:

~~~bash
./agentsync resume <NATIVE_SESSION_ID>
claude --resume
~~~

`pull --check` reads remote metadata only. `resume` downloads the selected
encrypted session and restores it to Claude Code. It does not copy project
files, Git changes, skills, MCP servers or credentials.

The default restore checks the target workspace. If the workspace is different,
review the reported differences before using `--allow-divergent`.

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
| `agentsync init [backend options]` | Create or join the encrypted sync domain and write local configuration. Can install the Claude Code hook. |
| `agentsync status [--json] [--remote]` | Show local status; `--remote` checks remote metadata. |
| `agentsync doctor [--json]` | Diagnose configuration, backend access, Agent installation, project identity and recent local errors. |
| `agentsync project bind [--path DIR] [--name NAME or --identity ID]` | Bind a local project. Use `--name` for a no-Git project. |
| `agentsync project unbind [--path DIR or --identity ID]` | Remove a local project binding. |
| `agentsync project mode normal / push-only / excluded [--path DIR or --identity ID]` | Set a project's synchronization policy. |
| `agentsync project list [--json]` | List bound projects and their policies. |
| `agentsync push [SESSION_ID] [--session SESSION_ID] [--agentsync-hook]` | Upload new records for the current project. |
| `agentsync watch [--interval DURATION] [--once] [--json]` | Repeatedly scan and push the current project; `--once` performs one scan. |
| `agentsync pull --check [--json]` | Check encrypted remote metadata without downloading session bodies. |
| `agentsync list [--json]` | List sessions available for the current project. |
| `agentsync resume [SESSION_ID] [restore options]` | Restore one session. Options include `--version`, `--allow-limited`, `--allow-divergent`, `--no-workspace-context` and `--replace-existing`. |
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

Without `AGENTSYNC_CONFIG_DIR`, AgentSync uses:

| Platform | Default directory |
|---|---|
| Windows | `%APPDATA%\\agentsync` |
| macOS | `~/Library/Application Support/agentsync` |
| Linux and other Unix systems | `$XDG_CONFIG_HOME/agentsync`, falling back to `~/.config/agentsync` |

init prints the exact directory after a successful initialization. To use
`.agentsync` under the current user's home directory:

~~~bash
export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync"
~~~

PowerShell:

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = Join-Path $env:USERPROFILE '.agentsync'
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
- Project files, uncommitted Git changes, branches, skills, MCP servers,
  plugins, credentials and arbitrary environment state are not synchronized.
- The target device must already have Claude Code and the project prepared.
- Git projects provide stronger workspace checks. No-Git projects use a
  touched-file fallback.
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
