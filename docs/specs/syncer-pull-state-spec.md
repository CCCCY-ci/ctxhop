# Spec: local observed pull-tip state

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncer-state-spec.md`, `syncflow-device-policy-spec.md` |

## 1. Scope

The pull policy needs one small local marker to distinguish a foreign remote
tip that has already been inspected from a newly published tip. The marker is
not a cursor and never authorises a restore. It is only an optimisation for
metadata-only checks.

For one project/session/local-device tuple the file is:

```text
<config-root>/state/v1/projects/<project-id>/sessions/<session-id>/<device-id>/pull.json
```

## 2. Wire format

```json
{
  "version": 1,
  "tips": [
    {
      "deviceId": "opaque-device-id",
      "recordCount": 42,
      "headDigest": "64 lowercase hexadecimal characters"
    }
  ]
}
```

Tips are sorted by device ID and are unique. The state is bounded to 4096
devices, decoded with unknown-field and trailing-data rejection, and written
atomically below restrictive directories. An absent file is an empty observed
set.

## 3. Semantics

The state is advisory and can be deleted without data loss. Remote metadata
remains authoritative. A caller saves a tip after it has inspected the
corresponding foreign version; if the remote head changes, `PlanPull` returns
the new tip again.

The local device's own tip is not saved as a foreign observed tip. Its branch
is governed by the durable `PushCursor`, so deleting or rewriting pull state
cannot cause the local branch to be restored.

## 4. Safety boundary

The file contains no session records, payload JSON, local paths, credentials,
remote addresses, or error text. It does not replace workspace fingerprint
checks, compatibility gates, fork selection, or atomic restore writes.

## 5. Test plan

Tests cover absent state, deterministic sorted round trips, duplicate and
malformed entries, version handling, atomic replacement, cancellation, size
limits, and syncflow conversion to and from `RemoteTip` values.
