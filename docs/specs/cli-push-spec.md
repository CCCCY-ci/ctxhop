# Spec: `agentsync push`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `syncer-push-spec.md`, `syncer-state-spec.md`, `syncflow-metadata-push-spec.md`, `syncflow-queue-spec.md` |

## 1. Invocation

`agentsync push` scans the current project's local Agent sessions and uploads
their complete canonical prefixes. `agentsync push <session>` and
`agentsync push --session <session>` restrict the run to one native session ID.
The hook marker `--agentsync-hook` is accepted by the installed SessionEnd
hook and suppresses human-oriented output.

Push never prompts for a passphrase and never needs the identity private key:
shards and metadata are encrypted to the pinned public key. This makes the
automatic path safe to run while the Agent is ending a session.

## 2. Durable ordering

For every session, the command derives the project, session, and local device
IDs, then loads the device-local cursor. It never uses a remote listing to
choose the next shard. The durable sequence is:

```text
strict adapter read -> canonical records -> immutable shard Put
-> local cursor Save -> encrypted session summary metadata -> queue Complete
```

The cursor and retry queue are stored below the AgentSync configuration root.
Remote keys are written only below the current device's opaque branch.

## 3. Failure behavior

Sessions are processed independently so one corrupt or incompatible session
does not prevent other local sessions from being backed up. Canonicalization
and local-history failures become terminal queue metadata; backend and cursor
publication failures use the finite retry policy. A hook invocation does not
propagate a push failure to the Agent process, while the manual command prints
a redacted aggregate and returns failure when any session was not synchronized.

No queue item stores session content, local paths, credentials, endpoint text,
or human-readable error strings.

## 4. Device-aware behavior

The local device ID is part of both the queue key and object layout. A device
publishes to its own branch only. Uploading on device A does not list, fetch,
or restore A's remote branch. It reads only the small remote keyfile first to
verify the pinned identity and persisted namespace binding, so the normal
upload path performs no redundant self-pull. Foreign branch inspection belongs to the metadata-only pull check
and explicit restore flow.

The effective local device mode is applied before backend setup. `push-only`
keeps this write path enabled; `disabled` returns a skipped summary.

## 5. Test plan

Tests cover hook argument parsing, one-session filtering, project exclusions,
canonical upload, cursor persistence, metadata summary publication, queue
retry behavior, and the absence of private-key or passphrase requirements.
