# Spec: Syncer remote branch reads

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Corresponding PRD | §8.3, §9.5, §9.6, §13 |
| Depends on | `syncer-object-spec.md`, `remote-layer-spec.md` |

## 1. Scope

This increment reads all visible encrypted shard objects for one project and
session, groups them by writing device, decrypts them, and assembles complete
device branches. It does not install a session, change local Agent files, or
write remote objects.

## 2. Listing semantics

The syncer lists the exact session prefix:

```text
v1/projects/<project-id>/sessions/<session-id>
```

Only keys with the exact shape `<device-id>/<six-digit-shard-number>` are shard
objects. `meta` and objects that fail the identifier/key-shape checks are
ignored as metadata or foreign objects. A duplicate shard sequence for one
device is an error rather than an arbitrary choice.

The absence of a key from a list is never treated as proof that the key does
not exist. If the visible set starts at a later sequence or has a gap, branch
assembly returns `ErrIncompleteBranch`; the caller must retry or report the
missing range. It must not restore the visible prefix.

If the list contains no shard objects, the session has no remotely readable
branch and the caller receives `ErrNoRemoteBranches`.

## 3. Object handling

Each object is read with the caller's context, bounded before being loaded into
memory, decrypted using the pinned identity private key, and parsed by the
versioned shard decoder. A missing object after listing, an authentication
failure, a malformed envelope, or a close/read failure stops the operation.
The syncer never substitutes an empty shard or another key.

Branches are returned in deterministic device-ID order. Their records are
already validated and can be passed to `ResolveBranches` for consistent,
fast-forward, or fork handling.

## 4. Test plan

Synthetic remote tests cover out-of-order listing, metadata/foreign-key
filtering, duplicate sequences, missing first and middle shards, list/get
disagreement, corrupt ciphertext, wrong object keys, context cancellation, and
successful multi-device assembly.
