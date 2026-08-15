# Spec: project-scoped encrypted metadata listing

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `syncer-metadata-spec.md`, `syncer-object-spec.md`, `remote-layer-spec.md` |

## 1. Scope

`FetchProjectMetadata` enumerates the authenticated metadata objects for every
session below one keyed project identifier. It is the only syncer entry point
for a project-level session listing.

The method reads object listings and encrypted `meta` objects only. It never
reads immutable shard bodies, assembles a branch, infers a session from a
shard-only prefix, or writes local Agent data.

## 2. Object selection

The remote listing is restricted to:

```text
v1/projects/<project-id>/sessions/<session-id>/<device-id>/meta
```

Shard objects and keys with extra path segments are ignored. Session and
device identifiers are validated as opaque lowercase identifiers before a key
is accepted. A duplicate metadata object for the same session/device pair is
an error.

The result is sorted by session identifier and then device identifier. The
metadata payload remains opaque to the syncer; higher layers decide whether it
contains a listing summary, workspace fingerprint, or a future payload.

## 3. Read and failure behavior

Every accepted object is bounded, read, decrypted with the caller's identity
key, and parsed through the same strict metadata path as a single-session
read. A missing project metadata set is returned as `ErrNoRemoteMetadata`.
Transport failures, authentication failures, malformed envelopes, oversized
objects, and cancellation are errors; none is converted into an empty list.

The operation never logs or returns local paths, credentials, backend
addresses, or plaintext shard records.

## 4. Test plan

Tests cover deterministic session/device ordering, shard filtering, duplicate
metadata, missing metadata, encrypted payload recovery, malformed objects,
and cancellation before any object body is read.
