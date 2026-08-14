# Spec: Session snapshot entry for queued push

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncflow-spec.md`, `syncflow-queue-spec.md`, `claude-code-adapter-spec.md` |

## 1. Scope

`QueuedPusher.PushSessionAt` is the complete adapter-snapshot entry point for a
single queued task. It combines the existing strict canonicalization decision
with queued push orchestration. The adapter snapshot is supplied by the
caller; this method does not discover files or read an Agent directory.

## 2. Ordering

The method checks context, task identity, and queue scheduling before doing
canonicalization. A task that is blocked or not due therefore causes no remote
I/O and no unnecessary second interpretation of the snapshot.

For an eligible task:

1. canonicalize the strict snapshot and apply the compatibility policy;
2. on a safe canonical stream, call `QueuedPusher.PushAt`;
3. on a canonicalization refusal, record a terminal queue class and stop before
   the executor.

Canonicalization errors are never retried with exponential backoff. An invalid
or corrupt snapshot becomes `FailureSessionCorrupt`; an unknown path field,
missing path space, or stopped compatibility policy becomes `FailureExcluded`.
The queue retains the task so status and an explicit recovery action can see
why it was not published.

## 3. Safety boundaries

The method never pushes `adapter.SessionData` directly, never pushes a lenient
snapshot (`Skipped != 0`), and never sends a stream containing an unknown path
field to the executor. A canonicalization error may update only local queue
metadata; it must not write remote objects or Agent files.

Context cancellation before or during the entry point leaves the task due and
does not create a terminal failure classification.

## 4. Test plan

Tests cover a valid snapshot, skipped/corrupt records, unknown path fields,
stopped compatibility, missing path space, blocked and not-due tasks, context
cancellation, queue state retention, and the absence of remote writes on every
canonicalization refusal.
