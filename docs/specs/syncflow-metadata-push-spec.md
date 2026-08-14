# Spec: durable metadata publication during push

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncer-execution-spec.md`, `syncer-metadata-spec.md`, `syncflow-queue-spec.md` |

## 1. Scope

The metadata push entry points add a small authenticated tip object to the
existing append workflow. The payload remains opaque to the syncer and
syncflow layers; a higher layer may place a workspace fingerprint or another
format-versioned JSON value in it.

The entry points are:

* `syncer.AppendExecutor.PublishMetadata`;
* `CanonicalStream.PushWithMetadata`;
* `QueuedPusher.PushWithMetadataAt`;
* `QueuedPusher.PushSessionWithMetadataAt`.

The ordinary push methods remain available for callers that do not yet have a
metadata payload.

## 2. Ordering

One successful queued attempt follows this order:

```text
canonical records -> immutable shard Put -> cursor Save -> metadata Put -> queue Complete
```

Metadata is derived from the cursor returned by the durable append executor,
not from the remote listing or from the input stream length. It therefore
describes only progress that the local cursor has already committed.

If metadata publication fails after the shard and cursor succeed, the returned
cursor is the newer durable cursor and the queue item records the metadata
failure. A retry plans an empty shard suffix and retries only metadata before
removing the queue item. The immutable shard is never selected from the remote
listing and is not rewritten as a way to repair metadata.

Context cancellation is not classified as a task failure. Other metadata
errors use the same finite classifier and backoff rules as shard errors.

## 3. Validation and privacy

The payload is validated as compact JSON before remote work begins. The
metadata envelope repeats the cursor record count and head digest and is
encrypted and authenticated to the exact local device metadata key.

Neither the queue nor error paths persist payload bytes, record contents, local
paths, credentials, or backend addresses. The metadata object itself is the
only remote write added by these entry points.

## 4. Test plan

Tests cover successful shard-then-metadata publication, metadata-only retry
entry behavior, invalid payload refusal before remote work, session
canonicalization before publication, cursor and context validation, and queue
cleanup after metadata success.
