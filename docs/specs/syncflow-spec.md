# Spec: Adapter-to-syncer session flow

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-14 |
| Depends on | `claude-code-adapter-spec.md`, `syncer-execution-spec.md` |

## 1. Scope

`internal/syncflow` is the composition boundary between an Agent adapter and
the format-independent syncer. It consumes a complete adapter snapshot,
canonicalizes its records, applies the adapter's compatibility decision, and
hands the resulting stream to `syncer.AppendExecutor`.

It does not discover files, read the remote, persist cursors, or understand any
Agent record type. File discovery and safe reads stay in the adapter; remote
publication and cursor durability stay in the syncer.

## 2. Canonicalization

`CanonicalizeSession` accepts the records returned by a strict adapter read:

* complete records are canonicalized in their original order;
* a dropped, unterminated tail is reported but is not pushed;
* a non-zero skipped-record count is rejected because a lenient read is not a
  safe source for an immutable remote shard;
* unknown path-bearing fields downgrade the session to `CompatStopped` and
  stop before any remote operation;
* an unverified or unknown Agent version remains pushable, as required by the
  adapter compatibility policy.

The path space must contain both the project root and Agent home. The
canonicalizer replaces known local prefixes with tokens before the records
reach the syncer, so local paths cannot be stored as canonical content.

## 3. Execution

`CanonicalStream.Push` calls `AppendExecutor.Execute` with the caller-supplied
last durable cursor. A missing initial cursor must be explicitly established
by the caller; this layer never guesses it.

The stream is safe to retry when the executor returns an error. The executor's
cursor semantics determine which records are durable; this layer does not add
another progress marker.

## 4. Failure boundaries

The flow never:

* pushes a lenient or partially parsed record stream;
* pushes a session with an unknown path-bearing field;
* logs record contents or local paths;
* rewrites records a second time after canonicalization;
* chooses a cursor by inspecting remote objects.

## 5. Test plan

Tests cover deterministic canonical streams, path-token replacement, dropped
tails, skipped-record rejection, unknown-field refusal, limited-version push,
remote ciphertext publication through the executor, cursor persistence, and
context cancellation before remote I/O.
