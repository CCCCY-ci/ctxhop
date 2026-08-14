# Spec: Syncer local push cursor state

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Corresponding PRD | §9.4, §9.7, BR-03, BR-04, BR-12 |
| Depends on | `syncer-push-spec.md`, `config-layer-spec.md`, `atomicfile` |

## 1. Scope

This increment persists the local cursor that the push layer uses to resume an
interrupted device-owned append. It deliberately does not implement a queue,
retry backoff, session discovery, or CLI output.

The cursor is local bookkeeping, not session content. It belongs to `syncer`
because `config` only owns user settings and credentials, while `adapter` only
knows how an Agent stores local sessions.

## 2. Local layout

Under the configured AgentSync directory, one cursor is stored per opaque
project/session/device tuple:

```text
<config-root>/
  state/v1/projects/<project-id>/sessions/<session-id>/<device-id>/cursor.json
```

The three path segments are the same lowercase keyed identifiers used by
`ObjectLayout`. Native Git remotes, local paths, native session IDs, device
names, records, and credentials never appear in this path or file.

The directory and file are created with restrictive defaults. The state file
contains no secret, but it is still private local bookkeeping and is not part
of `config.json` or `secrets`.

## 3. Wire format

The deterministic JSON document is:

```json
{"version":1,"nextShard":1,"recordCount":0,"headDigest":"...64 lowercase hex..."}
```

Load rejects unknown fields, trailing JSON, unsupported versions, malformed
digests, and cursors that fail `PushCursor.Validate`. A higher version is an
upgrade error; any other version is damaged state and is never interpreted as
the current format.

If the file does not exist, `Load` returns `ErrNoPushCursor`. It does not
silently return an initial cursor: an absent local state file cannot prove that
the device has never published a shard, and remote listing is not a safe source
of that fact under eventual consistency. The caller must explicitly decide
when a new local session may start with `NewPushCursor`.

## 4. Atomic updates

`Save` validates the cursor before creating directories or touching the target.
It writes through `atomicfile`, which places the temporary file beside the
target, syncs it, and renames it into place. An interruption leaves either the
previous complete cursor or the new complete cursor; it never publishes a
partial JSON document.

The caller saves only the cursor returned by a successful `PutShard`. A failed
remote write leaves the old cursor authoritative, so the next run retries the
same logical shard and sequence.

Missing, malformed, or unsupported state is a hard error. The store never
rebuilds it, scans remote objects to guess a replacement, or deletes a damaged
file.

## 5. Failure boundaries

The state store never:

* stores canonical records or plaintext session data;
* stores credentials or private keys;
* accepts native IDs as path segments;
* treats a missing file as proof that remote data is absent;
* publishes a cursor that fails the digest and sequence invariants;
* exposes the absolute config path in its user-facing error text.

The read and write operations check context cancellation before filesystem work
and return wrapped errors. Caller-owned cursors are values and are never
modified.

## 6. Test plan

Local tests cover deterministic round trips, missing state, malformed JSON,
unknown fields, trailing values, version handling, invalid digests and cursor
invariants, path validation, cancellation, and atomic replacement of an older
cursor. Tests use temporary directories and synthetic opaque identifiers only.
