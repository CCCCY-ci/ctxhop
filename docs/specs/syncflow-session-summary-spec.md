# Spec: encrypted session listing summary

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `syncflow-metadata-push-spec.md`, `syncer-metadata-spec.md`, `claude-code-adapter-spec.md` |

## 1. Scope

The syncflow session summary is the higher-layer payload placed inside an
encrypted syncer metadata envelope. It gives `list` and `resume` enough
information to identify a session without downloading its shard bodies.

The payload contains:

* the agent-native session identifier;
* a locally derived display title;
* creation and update timestamps;
* optional source workspace evidence for safe restore.

It deliberately does not contain the local project path or file size. The path
is machine-local state and must not cross the encryption boundary.

## 2. Wire format

The payload is compact JSON with a separate payload version:

```json
{"version":1,"nativeId":"...","title":"...","createdAt":"...","updatedAt":"...","fingerprint":{"head":"...","branch":"...","dirty":[],"files":{}}}
```

Timestamps are UTC RFC3339Nano strings. Unknown fields, trailing JSON,
non-compact JSON, missing timestamps, unsafe native IDs, and unsupported
versions are rejected. Titles are bounded and are sanitized again at the
terminal output boundary.

The optional fingerprint contains Git state, relative paths, and content
digests only. Its path and digest shapes are bounded before a remote payload is
accepted. Resume requires a fingerprint whose metadata tip matches the
selected resolved version; it never treats missing evidence as a clean
workspace.

The payload is not a substitute for the syncer metadata envelope: its durable
record count and head digest remain outside this payload and are checked by the
syncer layer.

## 3. Privacy and listing behavior

Titles can be derived from user prompts, so they are encrypted before remote
publication. A project-level list reads and decrypts only `meta` objects. It
does not read shard bodies or write Agent files. An unrecognised higher-layer
payload remains a valid syncer metadata object; list shows an encrypted
metadata placeholder rather than treating it as a session body.

## 4. Test plan

Tests cover compact deterministic encoding, UTC normalization, path exclusion,
fingerprint validation, unknown-field rejection, unsafe identifier rejection,
unsupported versions, and merging a local summary with foreign device
metadata.
