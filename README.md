# AgentSync

Continue an AI coding session on another machine without copying the
Agent's credentials or synchronising its live data directory.

> **Status: pre-alpha.** The local core sync/restore path, encrypted `dir`/S3
> storage, workspace safety checks, device modes, history maintenance and
> CLI completion are implemented. Real multi-OS Agent/Remote acceptance and
> production release packaging are still in progress.

## What it does

AgentSync runs locally. It discovers Claude Code sessions, canonicalises
machine-specific paths, encrypts records before writing them to a storage
backend you own, and restores one selected session into the Agent's native
session directory.

There is no AgentSync server, account or telemetry. The configured backend is
the only network destination. Remote metadata and session bodies are encrypted
locally; credentials and private keys stay on the device.

Before restore, AgentSync compares the target workspace with the source
workspace fingerprint. A divergent workspace requires explicit
`--allow-divergent` consent. Accepted non-consistent restores include a
local-only explanation that asks the Agent to re-read affected files; use
`--no-workspace-context` to disable that explanation.

## Five-minute local-directory setup

Requirements: Go 1.26+, Claude Code, and a directory available to both
devices. The directory backend is useful for a first local test or for a
directory synchroniser you already trust.

```bash
# Build the binary
go build -trimpath -o agentsync ./cmd/agentsync

# On device A, initialise once; this prompts for the encryption password
agentsync init --backend dir --path /path/to/agentsync-remote
agentsync device invite --output agentsync-invite.json

# Copy agentsync-invite.json to device B
agentsync init --invite agentsync-invite.json --device-name laptop

# From a project directory on the source device
cd /path/to/project
agentsync push
agentsync list

# On the target device, from the same project checkout
agentsync list
agentsync resume
claude --resume
```

The second device gets its own device identity. A normal device may push
sessions and perform explicit metadata/list/resume operations. `push-only`
devices never restore remote sessions; `disabled` devices skip automatic
synchronisation.
The invitation is a portable, signed pairing document. It carries the Remote settings and non-secret domain fingerprint, but no credentials, encryption password or session content. The second device verifies the existing keyfile before saving its local configuration. Managed domains then issue a device-specific encrypted grant, so each installation can be authorized and revoked independently.

`agentsync device rotate-key` creates a new content-key generation, encryption password and
Recovery Key while retaining the stable project namespace. `agentsync device
remove DEVICE_ID` performs the same rotation, tombstones the target device, and
then removes its remote branch objects. The command prints a new Recovery Key
and requires an explicit `saved` confirmation before publishing the rotation.

A sync domain is currently implicit: the configured Remote namespace and its
keyfile/data-key identity define the domain, while each installation gets a
different device branch. A domain may contain multiple projects. The current
project is selected from the working directory; push and watch do not scan every
project on the machine. Init prints a non-secret domain fingerprint; a new
device can pass `--expect-domain-fingerprint VALUE` to reject an unexpected
namespace. New configurations persist that fingerprint locally; core Remote
commands reject a manually changed namespace, and push reads only the small
keyfile object to verify the pinned identity before uploading. Project policy
can be changed locally:

    agentsync project mode normal
    agentsync project mode push-only
    agentsync project mode excluded
    agentsync project list

A directory without a usable Git remote currently has no automatic
cross-device identity. The intended manual form is:

    agentsync project bind --name client-project --path /path/to/project

The same manual identity must be used on the other device. A manually bound
non-Git project can still sync: its workspace safety check uses the L3
fallback and hashes only files reported as touched by the session. This means
changes to unreported files cannot be detected; Git-backed projects retain
broader dirty-file coverage. The shared current-project resolver now consumes
this binding for project-consuming commands; see
[`docs/specs/sync-domain-project-scope-spec.md`](docs/specs/sync-domain-project-scope-spec.md).

Useful follow-up commands:

```bash
agentsync status
agentsync pull --check       # metadata-only remote check
agentsync watch              # watch and push changed sessions
agentsync watch --once       # one watch cycle
agentsync history <session>
agentsync history prune --keep 3 <session>
agentsync doctor
```

For an S3-compatible backend, initialise with `--backend s3`, `--endpoint`,
`--bucket`, `--region` and `--prefix`. The non-secret Remote settings are
persisted in `config.json`; virtual-hosted addressing is the default, and
`--path-style` is available for gateways that require the bucket in the URL
path.

For Cloudflare R2, use the account S3 endpoint and the `auto` signing region:

```bash
agentsync init --backend s3 \
  --endpoint https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
  --bucket <BUCKET_NAME> --region auto --prefix agentsync
```

The Endpoint is account-level. Do not append `/<BUCKET_NAME>` to it; provide the
Bucket separately with `--bucket`.

When no encrypted backend credentials exist yet, `init` prompts for the
access key, secret key and optional session token, then stores them in the
encrypted local `secrets` file. They are not written to `config.json` and
should not be put in the command line. Interactive secret prompts do not echo the entered value.

After `init`, the opt-in integration test can read the complete S3
configuration from `config.json` and the credentials from encrypted `secrets`:

```bash
AGENTSYNC_CONFIG_DIR=/path/to/agentsync-config \
AGENTSYNC_S3_INTEGRATION=1 \
go test ./internal/remote -run '^TestS3Integration$' -count=1
```

`AGENTSYNC_CONFIG_DIR` may be omitted when the platform-default AgentSync
configuration directory is being used. The older `AGENTSYNC_S3_*` variables
remain supported as a fallback for isolated CI runs without an initialized
AgentSync configuration. Use a dedicated bucket or prefix and short-lived
credentials for every external test.

## Shell completion

Completion scripts are generated without loading configuration or contacting
the backend:

```bash
source <(agentsync completion bash)
source <(agentsync completion zsh)
agentsync completion fish | source
agentsync completion powershell | Invoke-Expression
```

See [`docs/specs/cli-completion-spec.md`](docs/specs/cli-completion-spec.md)
for persistent installation examples.

## Why not synchronise `~/.claude` directly?

Agent data is not a safe generic file-sync target:

- sessions contain absolute project paths and need adapter-aware rewriting;
- append-only conversations need fork/version semantics instead of
  last-write-wins conflict resolution;
- live databases can be corrupted while the Agent is running;
- credentials, caches and unrelated local state should not be copied to
  another machine.

AgentSync uses a storage backend as transport, but owns the session and
workspace semantics.

## Project status

| Area | State | Evidence |
|---|---|---|
| Encrypted local sync/restore path | Implemented locally | `internal/syncflow`, `internal/syncer`, CLI commands and tests |
| Cross-device device identity and modes | Implemented locally | `device`, `device-mode-spec.md` and sync-flow guards |
| Sync domain membership | Implemented locally | Domain fingerprint, persisted namespace binding, signed `device invite` packages, managed per-device grants, key rotation and `device remove` revocation are implemented; external multi-device acceptance remains pending |
| Multi-project scope | Implemented locally | `push`/`watch` process the current project; project policies are `normal`, `push-only` or `excluded` |
| No-Git manual project identity | Implemented locally | `project bind --name` is consumed by the shared current-project resolver across project-consuming commands |
| Workspace fingerprint safety | Implemented locally | PoC-2, restore checks and local-only difference context |
| MVP acceptance matrix | Reproducible locally | `go run ./poc/mvp` |
| Real Windows/macOS/Linux Agent matrix | Pending | Requires installed Agents and separate devices |
| Real S3/third-party Remote acceptance | Pending | Local `dir` Remote is covered; external credentials are required |
| Production release channels | In progress | CI, cross-build and release scripts are being established |

## Building and testing

The project requires Go 1.26 or newer and has no cgo dependency.

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/build.sh
```

Install a tagged source version with Go tooling when a release tag is available:

```bash
go install github.com/CCCCY-ci/agentsync/cmd/agentsync@v0.1.0
agentsync version
```

On Windows PowerShell, use `./scripts/build.ps1`. Cross-built binaries are
written to the ignored `dist/` directory. A tagged release workflow packages
the six supported targets and publishes checksums.

## Extension points

- [`internal/adapter`](internal/adapter) defines Agent discovery, strict
  session reading, path localisation and atomic writing.
- [`internal/remote`](internal/remote) defines the storage contract; the
  built-in implementations are local directory and S3-compatible storage.
- [`docs/specs/`](docs/specs) records the versioned format and safety
contracts. Start with the adapter and remote specs before adding an external
implementation.

### External implementation sketch

An Agent adapter implements `adapter.Adapter` and must preserve strict reads,
path-space rewriting and atomic writes. A Remote implementation satisfies the
content-agnostic `remote.Remote` contract and should also implement
`remote.Prober` when it can verify permissions during init.

```go
var _ adapter.Adapter = (*MyAdapter)(nil)
var _ remote.Remote = (*MyRemote)(nil)
```

The interface comments and the relevant specs are the acceptance checklist;
an external implementation must not inspect plaintext records or invent a
fallback for failed atomic operations. A complete contract example is available
in
[`examples/remote-memory`](examples/remote-memory/README.md).

## Known limitations

- Losing both the encryption password and Recovery Key is not recoverable.
- Managed domains provide cryptographic forward revocation: a removed device
  cannot unlock the next key generation. Plaintext or historical keys already
  copied by that device cannot be recalled, and a dumb backend can still accept
  destructive writes from anyone who holds its storage credentials; provider
  ACLs or credential rotation are still needed to control backend tampering.
- Object counts, sizes and timing remain metadata side channels.
- Agent format/version changes may downgrade an adapter to limited support;
  restore then requires explicit compatibility consent.
- Real cross-OS, real Agent and third-party Remote acceptance is not claimed
by the local test suite.

## Design documents

- [`docs/`](docs/) - PRD, TODO, PoC records and module specifications
- [`docs/TODO.md`](docs/TODO.md) - current implementation and acceptance
  ledger
- [`docs/acceptance/`](docs/acceptance/) - external Agent/Remote matrix and
  redacted failure-report template
- [`docs/specs/mvp-acceptance-matrix.md`](docs/specs/mvp-acceptance-matrix.md)
  - reproducible local MVP checks
- [`docs/specs/sync-domain-project-scope-spec.md`](docs/specs/sync-domain-project-scope-spec.md)
  - sync domains, multi-project scope and manual identities

## License

[Apache-2.0](LICENSE). If you redistribute a modified version, keep the
[`NOTICE`](NOTICE) file and mark the files you changed.
