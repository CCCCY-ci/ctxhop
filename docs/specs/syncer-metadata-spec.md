# Spec: syncer encrypted session metadata

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `syncer-object-spec.md`, `remote-layer-spec.md` |

## 1. Scope

Each device branch may publish one mutable `meta` object beside its immutable
shards:

```text
v1/projects/<project-id>/sessions/<session-id>/<device-id>/meta
```

The metadata object carries the durable record count, the branch head digest,
and one compact JSON payload owned by the orchestration layer. The syncer
validates the envelope but does not interpret the payload. This keeps
workspace fingerprints and future listing metadata out of the format-neutral
record and shard code.

Metadata is encrypted with the same object-level X25519/AES-GCM envelope as a
shard. The exact `meta` object key is authenticated, so moving metadata to a
different session or device branch fails decryption.

## 2. Wire format

The plaintext envelope is deterministic JSON:

```json
{"version":1,"recordCount":12,"headDigest":"...64 lowercase hex...","payload":{"...":"..."}}
```

`payload` must be one compact, valid JSON value. It is bounded to 1 MiB before
encryption. Unknown envelope fields, trailing JSON values, invalid digests,
and non-canonical payload bytes are rejected. A zero-record metadata object
must use `EmptyDigest()`.

The payload is copied on construction and parsing. Callers cannot mutate an
accepted metadata value through their original byte slice.

## 3. Publication

`PutMetadata` validates the context, store, recipient, object layout, and
metadata before calling `Remote.Put`. It writes only the local device's
`MetadataKey`, and passes the encrypted bytes with their exact length.

Metadata is written after the corresponding shard/cursor step by higher-level
orchestration. A reader may therefore observe an old metadata object or a
metadata object before all new shards are visible. Consumers must match
`recordCount` and `headDigest` to the assembled branch before using the
payload; a mismatch is stale metadata, not permission to restore it.

## 4. Reading

`FetchMetadata` lists the exact session prefix, accepts only
`<device-id>/meta`, ignores shard objects and foreign keys, and returns entries
in deterministic device-ID order. A duplicate metadata entry for one device
is an error. Missing metadata is explicit through `ErrNoRemoteMetadata`.

Objects are bounded before loading into memory, decrypted with the pinned
identity key, and parsed strictly. Missing objects after listing, authentication
failures, close/read failures, and oversized objects stop the operation. The
syncer never substitutes an empty payload or treats metadata absence as a
clean workspace.

## 5. Safety boundary

The syncer never logs or interprets the payload, and never sends its plaintext
to `remote.Remote`. A higher layer may encode a workspace fingerprint in the
payload, but must still require a matching branch digest before using it for a
restore decision.

## 6. Test plan

Local tests cover deterministic round trips, strict parsing, payload and object
size limits, zero-prefix rules, authenticated key binding, exact remote puts,
metadata listing/filtering, duplicate devices, missing objects, cancellation,
and stale metadata matching inputs. Test data remains local under the
repository's `.gitignore` policy.
