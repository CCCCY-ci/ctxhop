# Spec: Queued canonical session push

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncflow-spec.md`, `syncer-queue-spec.md`, `syncer-execution-spec.md` |

## 1. Scope

`syncflow.QueuedPusher` composes a canonical stream, a durable append executor,
and the syncer pending queue. It is the orchestration boundary for one task.
The queue contains only the opaque project/session/device key and retry
metadata; the stream remains the caller's current local snapshot and is never
serialized into queue state.

Canonicalization remains a separate step. A caller must not use this pusher as
a reason to push a lenient or path-unsafe adapter snapshot.

## 2. Execution state machine

Before remote work begins, the pusher loads the queue:

* a missing item is enqueued as immediately due;
* a pending item whose retry time is in the future returns
  `ErrQueuedPushNotDue` without remote I/O;
* a blocked item returns `syncer.ErrQueueItemBlocked` without remote I/O.

When the stream succeeds, the executor has already persisted every durable
cursor step. The pusher then removes the queue item. If queue cleanup fails,
the cursor remains authoritative and the item remains retryable; a subsequent
run can execute an empty suffix and retry cleanup safely.

When the stream fails, the pusher returns the executor's last durable cursor
and calls the queue's classified failure transition. Transient classes receive
backoff; terminal classes become blocked. If recording the failure also fails,
the returned error joins both failures and the item is left in its previous
durable state.

Context cancellation and deadline expiration are not classified as task
failures. The item remains due so an explicit later invocation can decide
whether to retry.

## 3. Failure classification

The pusher accepts a `FailureClassifier` callback. The callback returns one of
the finite `syncer.FailureClass` values and must not parse or persist an error
string. This is deliberate: storage backends know whether an error means
invalid credentials, missing permission, capacity exhaustion, or a transient
transport failure, while the queue only persists the resulting safe enum.

Returning `FailureNone` or an unknown class is rejected and does not mutate the
item's failure state. The callback is never called for context cancellation;
the task may already have been enqueued so the failed attempt is not forgotten.

## 4. Atomicity and retry safety

Cursor and queue files are separate atomic files. The ordering is intentionally
remote publication, cursor commit, then queue cleanup. Every interruption
therefore leaves either an old cursor with a queued task or a newer cursor with
an item that can be removed by an empty retry; neither state loses published
records or invents progress.

The pusher never chooses a cursor from the queue or remote listing, never
rewrites canonical records, and never sends queue metadata or plaintext
records to the remote backend.

## 5. Test plan

Tests cover first enqueue, transient failure and exponential backoff, terminal
blocking, not-due short-circuiting, successful cleanup, cancellation without a
failure transition, invalid classifiers, queue corruption, and retrying the
same logical task after an interrupted publication.
