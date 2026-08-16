# Spec: `agentsync status`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `config-layer-spec.md`, `syncer-pull-state-spec.md`, `syncflow-device-policy-spec.md`, `syncer-queue-spec.md` |

## 1. Default behavior

`agentsync status` is local and non-interactive. It reports redacted
configuration and project readiness without contacting the configured backend
or asking for the encryption passphrase. This makes it safe for shell prompts,
health checks, and machines that are intentionally offline.

## 2. Metadata-only remote check

`agentsync status --remote` is an explicit, interactive operation. It unlocks
the configured keyfile, lists and decrypts project metadata, and compares each
authenticated remote tip with the local device cursor and the last observed
foreign tips. It never reads shard bodies, writes observed pull state, restores
a session, or writes Agent files.

The check uses the local `Config.Device.ID` as the branch identity. A local
branch is not counted as a foreign update. Only metadata from other devices is
counted as `foreignUpdates`; the user must still run the explicit `resume`
flow before any body read or restore.

Projects configured as excluded or push-only are not queried remotely. Their
mode is reported without prompting for the passphrase.

The local device mode is included in the redacted configuration report. A
push-only or disabled device reports its remote boundary without opening local
secrets or contacting the backend. The configuration report also exposes the
non-secret domain fingerprint and whether the persisted namespace binding is
`bound`, `mismatch`, `invalid`, or `unbound`.

## 3. Reported counters

The JSON `sync` object and its text equivalent contain only aggregate counts:

* local and remote session counts;
* sessions with no foreign update, local-only sessions, and remote-only
  sessions;
* sessions with newly changed foreign metadata or local cursor state requiring
  attention;
* pending, due, delayed, and blocked local queue items for this project and
  device.

Opaque project, session, and device identifiers are not printed. Human-readable
session content, paths, credentials, backend addresses, and error strings are
not persisted as status state.

## 4. Failure behavior

An invalid keyfile, wrong pinned identity, failed unlock, unavailable backend,
or malformed authenticated metadata stops the remote check before it produces
a potentially misleading report. A damaged local cursor is counted as an
attention item rather than being treated as an empty cursor.
