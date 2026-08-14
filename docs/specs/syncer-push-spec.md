# Spec: Syncer incremental push planning

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Corresponding PRD | §8.3, §9.4, BR-03, BR-04, BR-12 |
| Depends on | `syncer-spec.md`, `syncer-object-spec.md`, `remote-layer-spec.md` |

## 1. Scope

This increment defines the safe boundary between a canonical local record
stream and remote shard publication:

* a local push cursor records the durable prefix already published by this
  device;
* the planner accepts only an unchanged prefix followed by new records;
* the suffix is split into validated immutable shard parts;
* one part can be encrypted and published to the device-owned remote prefix;
* a successful publication returns the cursor that the caller may persist.

Queue files, retry backoff, session metadata, adapter discovery, and CLI
orchestration remain later layers. This package does not infer a cursor from a
remote listing: eventual consistency and an interrupted last request make that
unsafe.

## 2. Push cursor

The cursor is the local source of truth for the next device-owned shard:

```go
type PushCursor struct {
    NextShard   uint64
    RecordCount uint64
    HeadDigest  [32]byte
}
```

An initial cursor has `NextShard=1`, `RecordCount=0`, and
`HeadDigest=EmptyDigest()`. After a successful shard publication, the caller
persists the cursor returned by `PutShard` atomically with its local queue
state. The cursor is never reconstructed from `Remote.List`.

The cursor binds three facts together:

```text
next object number = NextShard
number of durable records = RecordCount
digest after those records = HeadDigest
```

`Advance` accepts only a shard whose object number, base, and prefix digest
match those facts. A caller cannot accidentally advance past a gap or publish
a shard over a different local history.

## 3. Append planning

`PlanAppend` first validates every local record as canonical JSON. It computes
the digest of the local prefix at `RecordCount` and compares it with
`HeadDigest`:

| Local stream | Result |
|---|---|
| Same prefix and extra records | Plan the suffix |
| Exactly the cursor prefix | Empty plan; nothing to publish |
| Shorter than the cursor | Refuse; local history was truncated |
| Same length or longer but different prefix | Refuse; local history diverged |

The planner never rewrites or deletes a previously published shard. It copies
records into each `Shard`, so changing the caller's input after planning does
not change the plan.

The caller supplies `PlanOptions` with a maximum record count and encoded
envelope size. Both limits are positive, and the encoded limit cannot exceed
the format's 64 MiB hard cap. A single record that cannot fit is an error;
the planner never emits an oversized part and hopes the remote accepts it.

## 4. Publication and retry

`PutShard` performs these steps:

1. validate the context, remote, recipient, cursor, and shard transition;
2. derive the exact device-owned object key;
3. encode and encrypt the shard with that key as authenticated data;
4. call `Remote.Put` with the ciphertext and exact byte count;
5. return the advanced cursor only after `Put` succeeds.

Plaintext records never reach `Remote`. The remote interface intentionally
offers replace-style `Put` rather than conditional create: each device owns a
disjoint prefix, and the cursor prevents normal retries from choosing a new
sequence number or a different logical shard. Encryption is randomized, so a
retry may produce different ciphertext bytes; it is still idempotent at the
shard-content level and never creates a second object.

If `Put` fails, the old cursor remains authoritative. The caller retries the
same `ShardPart` with the same cursor. If the backend may have accepted the
request before returning an error, retrying the same logical part may replace
the bytes at the same device-owned key, but it cannot create a duplicate
sequence or modify another device's object.

The caller must persist the returned cursor only after the call returns nil.
It must not advance the cursor merely because the plan was constructed.

## 5. Failure boundaries

The push layer never:

* obtains the next sequence by listing remote objects;
* writes a shard before its local prefix has been verified;
* emits a partial, non-canonical, or oversized shard;
* sends plaintext to `Remote`;
* writes another device's prefix;
* treats a failed remote write as durable progress;
* logs record contents, local paths, credentials, or remote addresses.

`PutShard` accepts `context.Context` as its first parameter and checks
cancellation before local encryption and remote publication. `PlanAppend` is a
pure, non-blocking operation and therefore does not take a context. Both
operations return wrapped errors and leave caller-owned records and cursors
unchanged.

## 6. Test plan

Local tests cover initial and empty plans, multi-part chunking, record and byte
limits, an oversized single record, local truncation and divergence, cursor
transitions, sequence exhaustion, cancellation, nil arguments, encrypted
remote writes, exact byte counts, failed puts, and retrying the same logical
part. The integration fixture uses `remote.Dir` with synthetic records only.
