# AgentSync

[简体中文](README.zh-CN.md) | English

AgentSync is a local CLI for continuing a Claude Code session on another
installation without copying Claude credentials or synchronizing Claude's live
data directory.

> **Status: pre-alpha.** The encrypted session sync/restore path, local-directory
> and S3-compatible backends, project policies, device modes, workspace safety
> checks, history maintenance and shell completion are implemented. A real
> single-provider S3-compatible acceptance was completed using Cloudflare R2
> as the example provider on 2026-08-17. Cross-operating-system, third-party
> Remote and live-Agent acceptance remain separate pending matrices.

## What it does

AgentSync runs on the user's machines. It discovers Claude Code session JSONL,
canonicalizes machine-specific paths, encrypts records and metadata before
upload, maintains a device-local append cursor and retry queue, checks remote
metadata explicitly, and restores one selected session into Claude Code's
native session directory.

The backend can be a local directory or any compatible S3-style object store,
including Cloudflare R2. There is no AgentSync server, account service or
telemetry collector. The configured backend is the only network destination.

Each device writes only to its own opaque remote branch. Upload does not list or
download that device's branch again. Normal upload performs only the small
keyfile/identity check needed to verify the sync domain. Metadata inspection and
session-body restore are explicit operations.

## Important boundaries

AgentSync synchronizes session history, not an entire development machine.

It does not currently synchronize:

- Claude/API credentials, login sessions or other secrets;
- the whole Claude data directory, caches or live databases;
- skills, MCP servers, plugins, environment variables or other Agent
  installation state;
- project files, uncommitted Git changes, branches, worktrees or build
  artifacts.

The target machine must already have the Agent, dependencies, skills/MCP
configuration and project checkout. The workspace fingerprint is a safety
check, not a file synchronization mechanism.

Only the current project is processed by push and watch. AgentSync does not scan
every project on the machine. Multiple projects are supported, but every
project needs a stable identity and its own local policy.

An object-storage server is not an AgentSync device. S3-compatible storage is
only a transport backend. A headless machine without Claude Code can hold
objects or run some administrative checks, but cannot provide native Claude
Code resume by itself.

## Requirements

- Go 1.26 or newer when building from source.
- Claude Code on machines that discover or resume sessions.
- Git is recommended for stable project identity and broader workspace checks.
  A no-Git project can use a manual identity, with the limitation described
  below.
- For S3-compatible storage: endpoint, bucket, credentials and permissions for the selected bucket/prefix.

The code builds for Windows, macOS and Linux. Real cross-OS path localization
and native-Agent acceptance are separate external checks.

## Install from source

~~~bash
git clone https://github.com/CCCCY-ci/agentsync.git
cd agentsync

# macOS/Linux
go build -trimpath -o agentsync ./cmd/agentsync
./agentsync version

# Windows PowerShell
go build -trimpath -o agentsync.exe ./cmd/agentsync
.\agentsync.exe version
~~~

A development binary reports dev unless release metadata is injected. When a
release tag and published binary are available, Go tooling can be used:

~~~bash
go install github.com/CCCCY-ci/agentsync/cmd/agentsync@<VERSION>
agentsync version
~~~

Use agentsync help for the top-level command list. init intentionally refuses
to replace an existing valid configuration. Use a separate
AGENTSYNC_CONFIG_DIR for an isolated test installation.

## Configuration and local files

Default configuration directories:

| Platform | Directory |
|---|---|
| Windows | %APPDATA%\agentsync |
| macOS | ~/Library/Application Support/agentsync |
| Linux/Unix | $XDG_CONFIG_HOME/agentsync, falling back to ~/.config/agentsync |

Override the directory when testing, running hooks under another environment,
or maintaining multiple installations:

~~~bash
AGENTSYNC_CONFIG_DIR=/path/to/agentsync-config agentsync status
~~~

PowerShell:

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = 'D:\path\to\agentsync-config'
.\agentsync.exe status
~~~

Important files:

| Path | Purpose |
|---|---|
| config.json | Backend, endpoint/bucket/prefix, device state, project bindings and policies |
| secrets | Encrypted backend credentials, identifier material and device authorization |
| device.key | Local key that unlocks the encrypted secrets file |
| state/ | Push cursors, retry queue, pull observations, restore statistics and diagnostics |

Do not commit this directory. config.json contains no backend credentials or
passphrases, but can contain endpoints, bucket names and local absolute project
paths. Secret prompts do not echo typed values.

Claude Code data is separate from AgentSync configuration. CLAUDE_CONFIG_DIR can
relocate Claude data for a test or non-default installation:

~~~bash
CLAUDE_CONFIG_DIR=/path/to/claude-data agentsync list
~~~

For isolated CI, temporary S3 credentials can be supplied without writing them
to disk:

~~~text
AGENTSYNC_ACCESS_KEY_ID
AGENTSYNC_SECRET_ACCESS_KEY
AGENTSYNC_SESSION_TOKEN
~~~

Use the access-key and secret-key pair together. Prefer encrypted local secrets
for a normal installation and short-lived credentials for external tests.

## Quick start with a local directory

The directory backend is the easiest single-device test and also works with a
directory synchronizer you already trust.

Initialize a clean first installation:

~~~bash
agentsync init --backend dir --path /path/to/agentsync-store --device-name laptop-a --no-hook
~~~

Windows PowerShell:

~~~powershell
.\agentsync.exe init --backend dir --path D:\data\agentsync-store --device-name laptop-a --no-hook
~~~

init prompts for an Encryption password twice with hidden input. When the
remote keyfile is created for the first device/domain, it prints a Recovery
Key. Save it offline before continuing. Losing both password and Recovery Key
is not recoverable.

If Claude Code is installed and --no-hook is omitted, init offers to register a
SessionEnd hook that invokes agentsync push after a session ends. The hook is
optional; manual push and watch work without it.

For a Git project with a stable remote identity:

~~~bash
cd /path/to/project
agentsync push
~~~

For a project without a usable Git identity:

~~~bash
agentsync project bind --name client-project --path /path/to/project
cd /path/to/project
agentsync push
~~~

Every installation representing the same logical no-Git project must use the
same manual name. Binding is local configuration and does not upload anything
by itself.

Inspect and restore:

~~~bash
agentsync list
agentsync status
agentsync pull --check
agentsync resume <SESSION_ID>
claude --resume
~~~

pull --check is metadata-only: it does not download encrypted shard bodies,
write Claude files, restore a session or advance the local pull marker.
resume is the explicit body-reading and restore operation.

## S3-compatible storage, with Cloudflare R2 as an example

Initialize any compatible S3 backend:

~~~bash
agentsync init --backend s3 --endpoint https://s3.example.com --bucket my-agent-sync --region us-east-1 --prefix agentsync
~~~

Cloudflare R2 uses the account-level S3 endpoint and normally signing region
auto:

~~~bash
agentsync init --backend s3 --endpoint https://<ACCOUNT_ID>.r2.cloudflarestorage.com --bucket <BUCKET_NAME> --region auto --prefix agentsync
~~~

R2 is an S3-compatible provider, not a separate AgentSync backend. Its endpoint
is account-level. Do not append the bucket name to endpoint; pass the bucket
separately. This is wrong:

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com/<BUCKET_NAME>
~~~

Endpoint, bucket and prefix are stored in config.json. Access key, secret key
and optional session token are entered through hidden prompts and stored in
encrypted secrets when no environment override is active. Never put them in
shell history or command-line arguments.

The init probe writes, reads and deletes a temporary object. Credentials must
allow object Put/Get/Delete for the selected bucket/prefix. Normal list,
pull --check, resume and cleanup operations also need the relevant list/read/
delete permissions. Use a dedicated bucket or prefix and a short-lived
credential for testing.

Virtual-hosted addressing is the default. Add --path-style for a gateway that
requires the bucket in the URL path:

~~~bash
agentsync init --backend s3 --endpoint https://s3.example.com --bucket my-agent-sync --region us-east-1 --path-style
~~~

Opt-in integration test after initialization:

~~~bash
# macOS/Linux
AGENTSYNC_CONFIG_DIR=/path/to/agentsync-config AGENTSYNC_S3_INTEGRATION=1 go test ./internal/remote -run '^TestS3Integration$' -count=1 -v

# Windows PowerShell
$env:AGENTSYNC_CONFIG_DIR = 'D:\path\to\agentsync-config'
$env:AGENTSYNC_S3_INTEGRATION = '1'
go test .\internal\remote -run '^TestS3Integration$' -count=1 -v
~~~

The test is disabled by default and must never use production data. Older
AGENTSYNC_S3_* variables remain available as a fallback for isolated CI runs.

## Using a second device

A second installation joins the same sync domain with a signed invitation. It
gets a new opaque device ID and writes to a separate remote branch.

On device A:

~~~bash
agentsync device invite --output agentsync-invite.json
~~~

Copy the invitation through a trusted channel. It contains Remote settings and
a non-secret domain fingerprint, but not the encryption password, backend
credentials or session content.

On device B:

~~~bash
agentsync init --invite agentsync-invite.json --device-name laptop-b
~~~

Provide device B's S3-compatible credentials when prompted if needed. The
invite carries Remote settings; do not combine --invite with --backend, --path,
--endpoint, --bucket, --region, --prefix or --path-style.

Optionally require the fingerprint shown by device A:

~~~bash
agentsync init --invite agentsync-invite.json --device-name laptop-b --expect-domain-fingerprint <FINGERPRINT>
~~~

Bind the same project identity on device B. For no-Git projects, repeat the same
manual binding:

~~~bash
agentsync project bind --name client-project --path /path/to/project
~~~

Inspect membership:

~~~bash
agentsync device list
agentsync device status
~~~

An invitation is a pairing document, not a server-side account. The Remote
namespace and keyfile define the sync domain; each installation has its own
device identity and authorization.

## Normal upload and restore workflow

~~~text
Claude Code writes a session
        |
        | SessionEnd hook or manual push
        v
agentsync push
        |
        | encrypted device-owned shards + encrypted metadata
        v
configured directory or S3-compatible backend
        |
        | explicit metadata check
        v
agentsync pull --check
        |
        | explicit body download and safety checks
        v
agentsync resume [SESSION_ID]
        |
        v
Claude Code native session directory
~~~

On the target installation:

~~~bash
cd /path/to/the/same/project
agentsync pull --check
agentsync list
agentsync resume <SESSION_ID>
claude --resume
~~~

push processes only the current project and never pulls the local device branch
back. watch skips another push when the discovered session snapshot is
unchanged. Repeating push is safe: when there is no new session prefix, the
local cursor does not create a new logical shard.

If multiple forks exist:

~~~bash
agentsync history <SESSION_ID>
agentsync resume <SESSION_ID> --version 1
~~~

Do not use --replace-existing unless replacement is intentional. The default stops before overwriting an existing local session.

## Projects and synchronization policies

Selection is explicit: the current working directory is the default project
root, and push/watch/list/pull/resume/history/remote lifecycle commands operate
on that project. AgentSync does not scan all projects automatically.

Inspect bindings and policies:

~~~bash
agentsync project list
agentsync project list --json
~~~

Bind a Git or manual identity:

~~~bash
agentsync project bind --path .
agentsync project bind --identity https://github.com/example/client.git --path .
agentsync project bind --name client-project --path .
~~~

The identity and name forms are mutually exclusive. name creates a manual:name
identity. Repeating the same root/identity binding is idempotent.

Set a project policy:

~~~bash
agentsync project mode normal --path .
agentsync project mode push-only --path .
agentsync project mode excluded --path .
~~~

| Project mode | Push | Remote list/check/restore |
|---|---:|---:|
| normal | allowed | allowed |
| push-only | allowed | blocked |
| excluded | skipped/blocked | blocked |

A policy can target identity instead of path. normal removes the identity from
both restrictive lists.

## Device modes and device management

A device ID is independent from display name, hostname, path and mode. Renaming
does not create a new remote branch.

~~~bash
agentsync device status
agentsync device mode normal
agentsync device mode push-only
agentsync device mode disabled
~~~

| Device mode | Push | Metadata/list/pull/resume |
|---|---:|---:|
| normal | allowed | explicit operations allowed |
| push-only | allowed | blocked |
| disabled | skipped | blocked |

Manage devices:

~~~bash
agentsync device list
agentsync device list --json
agentsync device rename workstation
agentsync device remove <DEVICE_ID>
~~~

device remove confirms by default unless --yes is supplied. It rotates the
content-key generation, revokes the target device for future generations and
removes that device's remote branch objects. Data or keys already copied by
that device cannot be recalled.

## Passwords, Recovery Key and key rotation

Passwords are never accepted as command-line flags. Interactive secret prompts
are hidden.

~~~bash
agentsync passphrase change
agentsync passphrase reset
agentsync device rotate-key
~~~

passphrase change needs the current password. passphrase reset uses the existing
Recovery Key and does not generate a new one. device rotate-key asks for the
current/new password, prints a new Recovery Key and requires explicit saved
confirmation before publishing the new generation. Save it before confirming.

Losing both the password and Recovery Key is unrecoverable.

## History and remote cleanup

Inspect forks and retain old versions:

~~~bash
agentsync history <SESSION_ID>
agentsync history <SESSION_ID> --json
agentsync history prune <SESSION_ID> --keep 3
agentsync history prune <SESSION_ID> --before 2026-08-15T00:00:00Z
~~~
Choose exactly one prune rule. Unknown timestamps are retained conservatively.
Use --yes only after reviewing the target and rule.

history cleanup is an alias for deleting one session:

~~~bash
agentsync history cleanup <SESSION_ID>
agentsync remote delete-session <SESSION_ID>
agentsync remote delete-session --remote-id <OPAQUE_REMOTE_ID>
agentsync remote delete-project --path .
agentsync remote delete-all
~~~

Deletion confirms by default. remote delete-all is irreversible for the
configured domain namespace; verify backend, bucket, prefix and config directory
before using --yes.

Restore statistics are local:

~~~bash
agentsync stats
agentsync stats --json
~~~

## Workspace safety

Before restore, AgentSync compares the target workspace with the source
fingerprint stored in encrypted session metadata.

Git-backed fingerprints can include repository HEAD/branch state,
tracked/untracked dirty state and content digests for session-touched files.

A manually bound no-Git project uses the L3 fallback and hashes only files
reported as touched by the session. Unreported file changes cannot be detected,
so Git-backed projects provide stronger coverage.

A divergent workspace is refused by default:

~~~bash
agentsync resume <SESSION_ID> --allow-divergent
~~~

When accepted, AgentSync adds a local-only explanation to the restored session
so the Agent can re-read affected files. Disable only the explanation, not the
safety gate:

~~~bash
agentsync resume <SESSION_ID> --allow-divergent --no-workspace-context
~~~

Other explicit restore gates:

~~~bash
agentsync resume <SESSION_ID> --allow-limited
agentsync resume <SESSION_ID> --replace-existing
~~~

allow-limited accepts an unverified Agent format/version. version selects a
zero-based fork. replace-existing permits replacing an existing local session.

## Command reference

~~~text
agentsync help
agentsync version
agentsync init [--backend dir|s3] [backend options] [--device-name NAME]
               [--device-mode normal|push-only|disabled] [--invite FILE]
agentsync status [--json] [--remote]
agentsync list [--json]
agentsync push [SESSION_ID] [--session SESSION_ID] [--agentsync-hook]
agentsync watch [--interval DURATION] [--once] [--json]
agentsync pull --check [--json]
agentsync resume [SESSION_ID] [--json] [--version N]
                  [--allow-limited] [--allow-divergent]
                  [--no-workspace-context] [--replace-existing]
agentsync history SESSION_ID [--json]
agentsync history cleanup SESSION_ID [--yes] [--remote-id] [--path DIR]
agentsync history prune SESSION_ID [--keep N | --before RFC3339]
                    [--yes] [--remote-id] [--path DIR]
agentsync stats [--json]
agentsync doctor [--json]
agentsync passphrase change
agentsync passphrase reset
agentsync device status [--json]
agentsync device mode normal|push-only|disabled
agentsync device list [--json]
agentsync device rename NAME
agentsync device invite [--output FILE]
agentsync device rotate-key
agentsync device remove DEVICE_ID [--yes]
agentsync project bind [--path DIR] [--name NAME | --identity ID]
agentsync project unbind [--path DIR | --identity ID]
agentsync project mode normal|push-only|excluded [--path DIR | --identity ID]
agentsync project list [--json]
agentsync remote delete-session SESSION_ID [--yes] [--remote-id] [--path DIR]
agentsync remote delete-project [--yes] [--path DIR]
agentsync remote delete-all [--yes]
agentsync completion bash|zsh|fish|powershell|pwsh
~~~

Use --json where available for automation. Secret prompts go to the prompt
stream so JSON output can remain parseable.

## Shell completion

Completion generation does not load configuration or contact the backend.

~~~bash
source <(agentsync completion bash)
source <(agentsync completion zsh)
agentsync completion fish | source
agentsync completion powershell | Invoke-Expression
~~~

pwsh is accepted as a PowerShell alias. For persistent installation, use the completion mechanism documented by your shell.

## Troubleshooting

### init: backend probe failed

Check that the endpoint has an http/https scheme and no bucket path, that the
bucket is supplied separately, the signing region is correct (auto for R2),
credentials can Put/Get/Delete the prefix, list permission is available for
later workflows, and the system clock/network are working.

For the R2 example, use:

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com
~~~

not:

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com/<BUCKET_NAME>
~~~

### init says the machine is already configured

Inspect with status or doctor, or point AGENTSYNC_CONFIG_DIR at a new directory
for an isolated installation. Do not delete the old configuration until its
Recovery Key and backend data are no longer needed.

### Project identity cannot be resolved

A Git project needs a stable Git remote identity or explicit project bind
identity. A no-Git project needs project bind name or an explicit identity.
Use project list to inspect local bindings.

### No session is found

Confirm Claude Code is installed, CLAUDE_CONFIG_DIR points to the intended data
directory and the current directory is the same logical project. push and
watch inspect only the current project.

### Remote updates are not shown

Run pull --check from the target project. Confirm device/project modes are not
push-only or excluded. A device does not automatically pull its own branch;
remote bodies are read only by explicit resume.

### Restore is blocked

Read the reason before adding a flag: inspect workspace divergence before
allow-divergent, review Agent compatibility before allow-limited, use
replace-existing only for an intentional replacement, and inspect history
before selecting version N.

## Security model and limitations

- Session bodies, project metadata and device metadata are encrypted before
  reaching the backend.
- Backend credentials stay in encrypted local secrets and are never written to
  config.json or printed by diagnostics.
- Each device has an independent ID and disjoint remote branch.
- Signed invitations, domain fingerprints, per-device grants, key rotation and
  revocation protect future key generations.
- Remote object counts, sizes and timing remain possible metadata side channels.
- A storage credential can still perform provider-allowed destructive operations.
  Use bucket/prefix ACLs, short-lived credentials and provider-side rotation.
- Plaintext or historical keys already copied by a device cannot be recalled.
- Losing both encryption password and Recovery Key is unrecoverable.
- Live Agent files and machine environments are outside the current format.

## Development and verification

~~~bash
go test ./...
go test -race ./...
go vet ./...
go run ./poc/mvp
~~~

Focused PoCs are under poc/. Package tests cover adapter, crypto, config,
project, remote, syncer and syncflow.

Build release targets:

~~~bash
# macOS/Linux
./scripts/build.sh

# Windows PowerShell
.\scripts\build.ps1
~~~

Cross-built artifacts go to dist/. Release workflows, package templates and
format/versioning specifications are included, while production signing and
external package channels remain opt-in.

## License

[Apache-2.0](LICENSE). Keep NOTICE and mark modified files when redistributing.