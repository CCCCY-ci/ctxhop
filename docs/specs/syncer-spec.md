# Spec: Syncer record streams and version planning

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Corresponding PRD | §8.3, §9.4, §9.5, §9.6, BR-03, BR-04, BR-05, BR-12 |
| Depends on | `adapter` canonical records, `crypto` object encryption, `remote` opaque storage |

## 1. Scope

This increment defines the content model the syncer uses before it performs any
remote I/O or writes an Agent directory:

* a deterministic digest for a sequence of canonical records;
* an immutable shard envelope containing a contiguous record range;
* validation and assembly of one device's shard stream;
* comparison of record sequences and resolution of multiple device branches.

The envelope is plaintext only inside the syncer. The caller must encrypt its
encoded bytes with `crypto.Encrypt` before passing them to `remote.Remote`
(PRD §10.2, P6). The remote layer never sees this structure.

Push queue persistence, object-key derivation, remote reads/writes, adapter
localisation, and atomic Agent installation remain separate increments. Keeping
this core pure makes the dangerous version rules testable without touching a
user's Agent data directory.

## 2. Canonical record stream

The stream consists of complete, single-line JSON records produced by an
Adapter's canonicalizer. A record must be valid JSON, contain no CR/LF, and be
compactly encoded. The syncer does not interpret Agent fields and therefore
does not perform a second JSON re-encoding; field-aware path handling remains
the Adapter's responsibility.

The digest is a chained SHA-256 value. The empty prefix is:

```text
SHA256("agentsync/records/v1\\x00")
```

For each record, the next digest is:

```text
SHA256("agentsync/records/v1\\x00" || previousDigest ||
       uint64_be(len(record)) || record)
```

Including the record length prevents concatenation ambiguity. The digest after
`N` records is the prefix digest for a shard beginning at record `N`.

## 3. Shard envelope

The encoded plaintext is deterministic JSON:

```json
{"version":1,"base":0,"count":2,"prefixDigest":"...64 lowercase hex...","records":[{"...":...},{"...":...}]}
```

`base` is the zero-based record offset, `count` equals the number of records,
and `prefixDigest` is the digest immediately before the first record. A shard
must contain at least one record. Shards are immutable once published.

The object name supplies the device-local sequence number. It is not part of
the encrypted payload: names are storage concerns, while the header is what
allows the syncer to verify that a stream is contiguous and that no prefix was
silently skipped.

The current implementation accepts at most 64 MiB of encoded shard data. This
is below the remote driver's 256 MiB object limit and bounds memory used while
parsing untrusted remote bytes. A future format with larger shards requires a
new version or an explicit compatible limit change.

## 4. Device stream validation

For one device, shard sequence numbers must start at `1` and increase without a
gap. After sorting by the storage sequence number, every shard must satisfy:

```text
shard.base == recordsAlreadyAssembled
shard.prefixDigest == digest(recordsAlreadyAssembled)
```

Any missing sequence, duplicate sequence, invalid header, base mismatch, or
digest mismatch stops assembly. The caller must not treat the available prefix
as a complete session; this is the final-consistency rule from
`remote-layer-spec.md §5.2`.

## 5. Version relation

Two complete record sequences are compared byte-for-byte:

* equal sequences are consistent;
* if the left sequence is a strict prefix of the right, the left is behind;
* if the right sequence is a strict prefix of the left, the left is ahead;
* otherwise they diverged at the first unequal record.

When several devices have written a session:

* identical branches are grouped by their device IDs;
* a branch that is a strict prefix of another is removed from the set of
  maximal versions, producing a fast-forward result;
* two or more incomparable maximal branches produce a fork result, and every
  maximal branch is preserved;
* no branch is deleted or automatically chosen over an incomparable branch
  (PRD §9.6, BR-05).

The result is a plan only. Installing a branch into an Agent directory requires
the later adapter/sync orchestration layer to perform path rewriting, user
confirmation where compatibility is limited, workspace checks, and an atomic
write.

## 6. Failure boundaries

The syncer core never:

* accepts a partial or non-canonical record;
* turns a missing shard into an empty session;
* selects one fork and discards another;
* writes local Agent data;
* logs record contents, local paths, credentials, or remote addresses.

All exported operations that parse external bytes return an error with `%w`
wrapping and leave their input untouched.

## 7. Test plan

The local test suite covers digest chaining, deterministic round trips, invalid
and oversized envelopes, gaps and out-of-order shards, prefix/digest mismatch,
equal/fast-forward/fork relations, duplicate branches, and fuzz-style arbitrary
input parsing. The tests remain local per the repository's `.gitignore` policy.
