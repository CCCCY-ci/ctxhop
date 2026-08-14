# Spec: Syncer durable append execution

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncer-push-spec.md`, `syncer-state-spec.md`, `remote-layer-spec.md` |

## 1. Scope

This increment connects append planning and publication to the local cursor
store. It defines the smallest execution unit that can be retried after a
process exit or a remote error:

* the caller supplies the last cursor it considers durable;
* the executor plans only the suffix after that cursor;
* shards are published in sequence;
* the cursor is atomically saved after every successful publication.

Adapter discovery, session reading, queue scheduling, backoff policy, and CLI
argument handling remain outside this layer. The executor accepts canonical
records so it can be used by any adapter without knowing an Agent's format.

## 2. Durable step

For each planned shard the executor performs:

1. validate the cursor-to-shard transition;
2. encrypt and publish the shard at its device-owned key;
3. atomically save the advanced cursor;
4. use the saved cursor as the input to the next shard.

The remote write happens before the local cursor save. If the save fails after
the remote accepted the object, the old cursor remains the returned durable
cursor. Retrying the same call therefore publishes the same logical shard key
again; it never allocates a second sequence or writes another device's prefix.

If a later shard fails, earlier cursor saves remain durable and the returned
cursor identifies that durable prefix. The caller may retry from that cursor
with the unchanged local record stream.

## 3. Missing state and initialization

The executor never turns a missing cursor into `NewPushCursor` implicitly. A
caller must explicitly establish the initial cursor after deciding that this is
a new local session. This prevents a deleted state file from causing a first
shard to overwrite an existing remote branch.

## 4. Failure boundaries

The execution layer never:

* infers progress from remote listing;
* saves a cursor before its shard has been accepted by the remote;
* reports an unsaved cursor as durable after a cursor write failure;
* sends plaintext records to the remote;
* logs record contents, local paths, credentials, or remote addresses.

`context.Context` cancellation is checked before planning, before each remote
step, and by the cursor store during its atomic write. A cancellation after a
remote write but before the cursor save is reported as a cursor commit error;
the caller retries with the returned old cursor.

## 5. API

```go
type AppendExecutor struct { /* validated remote, layout, and cursor store */ }

func NewAppendExecutor(
    store remote.Remote,
    recipient *ecdh.PublicKey,
    layout ObjectLayout,
    state CursorStore,
    options PlanOptions,
) (AppendExecutor, error)

func (e AppendExecutor) Execute(
    ctx context.Context,
    cursor PushCursor,
    records [][]byte,
) (PushCursor, error)
```

On a precondition or planning error the returned cursor is zero. Once remote
execution starts, an error returns the last cursor known to be durable in the
local state file. A nil error returns the final cursor; an empty suffix is a
successful no-op and does not rewrite the state file.

## 6. Test plan

Tests cover multi-shard progress, cursor saves after every part, empty suffixes,
remote failures, cursor-save failures, cancellation before planning and between
steps, retrying a failed save with the old cursor, constructor validation, and
the guarantee that plaintext never reaches the remote.
