# Durable pending queue specification

## Scope

The pending queue records work that should be retried after a sync attempt
cannot finish. It stores scheduling metadata only. Canonical session records,
local paths, credentials, remote addresses, and human-readable error messages
are deliberately outside the queue format.

The queue is an orchestration primitive. It does not discover sessions, read
Agent files, or decide how a task's canonical records are reconstructed. A
higher layer uses the opaque task key to locate the local source again before
calling the sync executor.

## Key and storage

Each task is identified by three opaque, lowercase ASCII identifiers:

- `projectId`
- `sessionId`
- `deviceId`

The queue is stored at `<config-root>/state/v1/queue.json`. The file is
versioned, decoded with unknown-field rejection, written atomically, and
created below restrictive (`0700`) directories. An absent file means an empty
queue.

Queue operations are intended to be serialized by the owning process. The
store provides load-modify-save helpers for the common operations, but it does
not claim to coordinate multiple writers.

## Item states

An item starts as `pending` with attempt `0`, no failure class, and no
`nextAttemptAt`, which means it is immediately eligible. A successful sync
removes the item.

Transient failures keep the item pending. The first transient failure changes
the attempt to `1`; the next eligible time is `baseDelay`. Each later failure
doubles the delay up to `maxDelay`. The policy is deterministic and has no
random jitter, so an interrupted process can resume without another source of
state.

Terminal failures change the item to `blocked`. Blocked items are retained for
status and manual recovery, but are never returned by the due-task query.

The failure class is an enum rather than an error string:

| Class | Retry behavior |
| --- | --- |
| `network` | retry with backoff |
| `unknown` | retry with backoff |
| `credentials` | block |
| `permission` | block |
| `storage-full` | block |
| `session-corrupt` | block |
| `excluded` | block |

This prevents credentials, filesystem paths, remote endpoints, and arbitrary
backend text from being persisted or exposed through queue status.

## Wire format

The current format is:

```json
{
  "version": 1,
  "items": [
    {
      "projectId": "project-id",
      "sessionId": "session-id",
      "deviceId": "device-id",
      "attempt": 1,
      "nextAttemptAt": "2026-08-14T00:00:05Z",
      "state": "pending",
      "failure": "network"
    }
  ]
}
```

`nextAttemptAt` is an RFC3339 timestamp in UTC, or the empty string when the
item is immediately eligible or blocked. Items are serialized in deterministic
key order. Unknown fields, duplicate keys, malformed timestamps, invalid
identifiers, and impossible state/failure combinations are rejected.
