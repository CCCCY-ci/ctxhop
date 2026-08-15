# Spec: `agentsync list`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `syncer-project-metadata-spec.md`, `syncflow-session-summary-spec.md`, `config-layer-spec.md` |

## 1. Behavior

`agentsync list` identifies the current project, unlocks the configured
remote keyfile, and lists sessions available to that project. It combines
local adapter discovery with remote encrypted metadata so a session can be
shown before its body is restored.

The command accepts `--json` for machine-readable output. A passphrase is read
interactively and is never accepted as a flag or written to configuration.

## 2. Read boundary

The command may read:

* the current project's Git identity;
* local session summaries from the configured Agent adapter;
* the remote keyfile and encrypted `meta` objects under the keyed project ID.

It does not read remote shards, write remote objects, write Agent files, or
persist observed pull tips. Remote session/device IDs are opaque and are only
used to merge metadata and identify source branches.

## 3. Output

Each item includes a title when the standard session summary is available, the
native session ID when known, creation/update times, whether it is present
locally, its source device identifiers, and the durable metadata record count.
No project path, Git remote, bucket, endpoint, credential, or passphrase is
printed. Unknown higher-layer payloads remain listable with a generic encrypted
metadata label.

## 4. Failure behavior

An unstable current project, invalid local configuration, keyfile identity
mismatch, unlock failure, metadata authentication failure, malformed metadata,
or backend failure stops the command. Missing remote metadata is an empty
remote result, not proof that a project has no sessions.
