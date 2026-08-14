# Spec: Syncer object keys and encrypted shards

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Corresponding PRD | §8.3, §10.2, BR-03, BR-04, BR-08 |
| Depends on | `syncer-spec.md`, `crypto-spec.md`, `remote-layer-spec.md` |

## 1. Object key layout

The syncer builds keys only from the keyed identifiers returned by the crypto
layer. Native Git remotes, local paths, native session IDs, and device names
never appear in a remote key.

```text
v1/projects/<project-id>/sessions/<session-id>/<device-id>/meta
v1/projects/<project-id>/sessions/<session-id>/<device-id>/000001
v1/projects/<project-id>/sessions/<session-id>/<device-id>/000002
```

The device directory is the only prefix this device may write. Shard sequence
numbers start at one, are six decimal digits, and are monotonically increasing;
the six-digit limit is explicit so lexicographic listing remains numeric. A
future larger stream needs a format decision rather than silently changing key
ordering.

Every generated key passes `remote.ValidateKey` before it is returned. IDs are
restricted to lowercase ASCII alphanumeric segments, matching the current
lowercase Crockford output of `crypto.ProjectID`, `SessionID`, and `DeviceID`.

## 2. Encryption boundary

`SealShard` first encodes the validated shard and then calls
`crypto.Encrypt(recipient, objectKey, plaintext)`. The exact object key is bound
into the ciphertext's authenticated data and key derivation. `OpenShard` uses
the same key before parsing the decrypted envelope.

Consequences:

* no plaintext shard bytes are handed to `remote.Remote`;
* moving a ciphertext to a different object key fails authentication;
* malformed or tampered ciphertext never produces a partial shard;
* `remote` remains content-agnostic and cannot make version decisions.

The object key is an input to both operations, so callers must derive it once
and retain it unchanged through `Put` and `Get`.

## 3. Failure behavior

Invalid identifiers, sequence numbers, and object keys fail before encryption
or remote I/O. A decryption failure and a malformed plaintext envelope are
returned as errors; no fallback key, alternate path, or partial record set is
attempted.

## 4. Test plan

Local tests cover key layout and validation, sequence boundaries, encrypted
round trips, wrong-key and wrong-path failures, and the absence of recognizable
plaintext in the ciphertext. Test data is synthetic and remains ignored by the
repository's test-file policy.
