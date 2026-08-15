# Spec: `agentsync watch`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `cli-push-spec.md`, `device-mode-spec.md`, `claude-code-adapter-spec.md` |

## 1. Invocation

```text
agentsync watch [--interval DURATION] [--once] [--json]
```

The command watches the current project only. The default polling interval is
30 seconds; values below one second or above 24 hours are rejected. `--once`
runs one scan and exits, which is useful for automation and verification.
Without it, Ctrl+C stops the process cleanly.

The configuration is loaded once when watch starts. Changing configuration,
device mode, project policy, or the current directory requires restarting watch.

## 2. Polling and push trigger

Each cycle identifies the current stable project and discovers local Claude
session references. The snapshot signature is derived from each session's
opaque native ID, size, creation time, and update time, in deterministic order.
The first cycle is considered changed and runs the normal push path.

An unchanged signature produces no backend setup and no push. A new session,
an append, or a changed session timestamp triggers a push of the current
project. Session deletion does not delete remote data; the next scan only
updates the local snapshot.

A failed push does not commit the new signature. The next interval retries the
same snapshot, so a transient backend or incomplete-session failure is not
lost. Pushes remain protected by the durable local cursor and are safe to
retry. Watch never starts a pull check, lists remote session objects, reads
remote shard bodies, restores sessions, or writes observed pull tips.

The effective device and project policies used by `push` are applied without
a second policy implementation. A disabled device produces a skipped push
summary; push-only remains a valid watch mode.

Watch is a polling fallback for installations that cannot use the Agent
SessionEnd hook. Running both is safe at the data layer because the local push
cursor makes unchanged pushes no-ops, but users should normally choose one
trigger to avoid unnecessary scans.

## 3. Output

Text output announces startup and emits only push or error events:

```text
watching: interval=30s
push: pushed: 1, failed: 0, skipped: 0
watch error: push cycle failed; run 'agentsync push' for details
```

Idle cycles are silent. JSON emits one newline-delimited object per startup,
push, or error event:

```json
{"scope":"project","event":"started","interval":"30s"}
{"scope":"project","event":"push","pushed":1,"failed":0,"skipped":0}
```

JSON and text output contain no session content, local paths, backend
credentials, passphrases, or remote object keys. Error output is categorized
when details could disclose local or backend information; manual `push` is the
diagnostic path.

## 4. Failure behavior

A malformed watch option fails before configuration or Agent access. A missing
stable project, missing Agent installation, or a cycle error is reported. In
resident mode, cycle errors are emitted and retried at the next interval. In
`--once` mode, cycle errors and a non-zero failed push summary return a
non-zero command result after emitting the event.

Cancellation stops before the next interval and returns success for a normal
Ctrl+C shutdown. Watch does not create a new persistent state file; all durable
push state remains owned by the existing queue and cursor stores.

## 5. Test plan

Tests cover option bounds, deterministic session signatures, first-cycle
triggering, unchanged-cycle suppression, retry after failed push, once-mode
failure behavior, JSON/text event output, cancellation, and the absence of
pull or remote-list operations.
