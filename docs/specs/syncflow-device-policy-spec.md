# Spec: device identity and pull policy

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `config-layer-spec.md`, `syncer-metadata-spec.md`, `syncflow-restore-spec.md` |

## 1. Device identity

`Config.Device.ID` is the stable opaque identity of one AgentSync
installation. `config.EnsureDeviceID` creates it once from fresh local entropy
through the keyed `crypto.DeviceID` domain and saves it atomically with the
configuration. It contains no hostname, username, local path, or display
name.

An existing identifier is never silently replaced. A malformed value is
rejected because changing it would create a new remote branch and could make
the old branch look like another device. The display-only `Device.Name` is not
used in remote keys or pull decisions.

The device ID is session-independent: the same installation uses the same
branch component for every project and session. The session ID and project ID
remain separate keyed identifiers.

## 2. Push boundary

The automatic push path is one-way:

```text
adapter snapshot -> canonical stream -> local cursor -> shard Put
```

`CanonicalStream.Push` and the queued pusher do not list remote objects or
fetch metadata. Publishing a session on device A therefore does not cause A to
read its own branch back from the backend. The local cursor remains the only
source of truth for choosing A's next shard.

Metadata publication is a separate higher-layer step. A stale metadata object
is tolerated by readers; it never overrides the local cursor.

## 3. Automatic pull check

`syncflow.FetchPullPlan` performs a metadata-only check. It lists and decrypts
the small `meta` objects, but it does not read shard bodies or write Agent
files. `PlanPull` then applies these rules:

1. The local device ID is removed from the foreign candidate set.
2. A local metadata tip behind the local cursor is treated as stale.
3. A local tip ahead of, or divergent from, the local cursor is an error; it is
   never treated as a remote branch to restore automatically.
4. Foreign tips are compared with the caller's last observed opaque tips.
   Equal tips and older eventually-consistent listings are ignored.
5. Only changed foreign tips are returned as reasons for an explicit body
   read or a user-visible restore choice.

If no authenticated metadata exists, the automatic check returns a no-op. It
does not infer that a session is empty or complete from the absence of a
listing.

For a session whose only branch is device A, A's metadata matches its local
cursor and the foreign set is empty. The normal result is therefore no body
read and no restore attempt.

## 4. Explicit restore

Metadata availability is not permission to write. A caller must explicitly
request restore and pass through `FetchRestorePlan` and `ApplyRestore`.

The explicit restore planner reads all complete device branches, including the
local branch when one exists, because fork and common-prefix resolution must
see the complete history. This full read is intentional and is not part of
the automatic upload or metadata-only check.

## 5. Safety boundary

Device IDs and remote tips are opaque scheduling data. They never contain
session records, local paths, credentials, or backend addresses. The pull
policy does not interpret metadata payloads; a higher layer must still perform
compatibility, workspace fingerprint, fork-selection, and atomic-write checks
before applying a restore.

## 6. Test plan

Tests cover first-use ID generation and persistence, refusal to replace or
accept unsafe IDs, local-branch exclusion, stale local metadata, local
divergence, observed foreign-tip suppression, deterministic ordering,
metadata-only remote reads, missing-metadata no-ops, and cancellation before
remote access.
