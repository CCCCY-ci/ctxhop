# Spec: `agentsync pull --check`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `config-layer-spec.md`, `syncer-project-metadata-spec.md`, `syncer-pull-state-spec.md`, `syncflow-device-policy-spec.md` |

## 1. Invocation

`agentsync pull --check` performs an explicit project-level remote check. The
current working directory identifies the project. The command reads the
encryption passphrase from stdin and writes its prompt to the interactive
prompt stream. `--json` changes only the report format; it does not skip
authentication.

The command is intentionally a check, not a restore operation. A successful
check reports aggregate metadata state and leaves the user to run the explicit
session restore flow.

## 2. Metadata-only boundary

The check authenticates the configured backend, reads the encrypted project
keyfile, and lists and decrypts encrypted session metadata. It compares each
remote session's authenticated device tips with the local device cursor and
read-only observed-tip state.

The check:

- does not read encrypted shard bodies;
- does not write Agent session data;
- does not restore, merge, or resume a session;
- does not publish remote objects;
- does not advance the local push cursor;
- does not save observed foreign tips.

Not saving observed tips is deliberate. Repeating the check without restoring
the update must continue to report the same foreign change.

## 3. Device and project boundaries

The effective local device mode is checked before project identification,
secret loading, or backend setup. A `push-only` or `disabled` device cannot
run this pull check.

A project configured as excluded or push-only is rejected before the command
opens the keyfile or prompts for its passphrase. The local device ID is used
to exclude the current device's branch from foreign-update counts.

The report contains only aggregate counters. It never prints project, session,
or device identifiers, session content, paths, credentials, or backend
configuration.

## 4. Report

The default output has this shape:

```text
scope: project
check: metadata-only
result: updates-available
sessions: checked=3 foreign-updates=1 foreign-branches=1 unchanged=2 attention=0
```

The machine-readable output has this shape:

```json
{
  "scope": "project",
  "mode": "metadata-only",
  "result": "updates-available",
  "sessions": {
    "checked": 3,
    "foreignUpdates": 1,
    "foreignBranches": 1,
    "unchanged": 2,
    "attention": 0
  }
}
```

`checked` counts remote session metadata records. `foreignUpdates`
counts sessions with at least one foreign branch newer than the local cursor
and not already covered by read-only observed-tip state. `foreignBranches`
counts those changed foreign branches. `unchanged` counts checked sessions
without a currently actionable foreign update. `attention` counts sessions
whose local cursor or observed-tip state could not be read safely.

The result is:

- `up-to-date` when no foreign update or attention item exists;
- `updates-available` when at least one foreign update exists;
- `attention-required` when any session state cannot be inspected safely.

An empty remote metadata listing is a valid `up-to-date` result with all
counters set to zero.

## 5. Failure behavior

An unavailable backend, invalid authenticated metadata, unreadable keyfile,
wrong pinned identity, or failed passphrase unlock stops the check with an
error instead of returning a partial aggregate. A damaged local cursor or
pull-state record is isolated to its session and counted as
`attention-required`, unless cancellation interrupts the entire operation.

All remote reads run under the command timeout and honor context cancellation.
The command never persists a result merely because the check succeeded.

## 6. Test plan

Tests cover option parsing and command registration, device and project
boundaries, text and JSON reports, no-remote-metadata behavior, foreign-tip
detection, observed-tip suppression, and the metadata-only guarantee using an
invalid shard body that must never be read.
