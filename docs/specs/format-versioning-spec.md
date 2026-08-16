# Spec: format versioning and migration policy

| | |
|---|---|
| Status | Implemented policy; migration hooks remain maintenance work |
| Date | 2026-08-16 |
| Depends on | syncer wire-format specs, config-layer-spec.md, crypto-spec.md |

## 1. Scope

AgentSync has several independently persisted formats: configuration, the
keyfile, encrypted remote metadata and shards, device records, pull tips,
pending queue state, and restore statistics. Each format owns its version field
and decoder. The remote adapter remains content-agnostic and never interprets a
format version.

This policy applies to local state and remote objects. It does not promise that
an older build can read every future format, and it does not permit silently
rewriting user data while opening it.

## 2. Read behavior

Readers must reject unknown fields, trailing values, malformed values, and
unsupported versions. A version higher than the current build is an explicit
upgrade error. A lower or otherwise different version is an invalid-format
error unless that format's decoder documents a compatible read path.

The caller must fail closed: it must not treat a rejected object as empty,
truncate a local branch, publish a replacement, or continue to an Agent body
read. Error classification may be surfaced to doctor, but raw payloads,
credentials, and local paths must not be persisted in diagnostic history.

## 3. Write and migration behavior

Writers emit exactly the current version and publish through the existing
atomic/conditional path for that state. A format change requires all of the
following before implementation:

1. a new version number and a format-specific compatibility table;
2. a decoder test for current, future, old, malformed, and trailing input;
3. an explicit migration or rebuild command when old data can be converted;
4. a rollback rule that leaves the old object/state recoverable until the new
   representation has been validated;
5. a cross-version acceptance record covering at least one old and one new
   build.

Migration must be opt-in or an explicitly documented startup migration. It must
not be hidden inside a normal metadata-only pull check. Remote migration is
performed by AgentSync using authenticated, fully validated objects; the
storage provider is never asked to understand the format.

## 4. Current compatibility baseline

The current production-intended baseline is version 1 for the formats listed
above. The repository already rejects future versions in the relevant decoders
and has regression tests for representative version failures. No version 2
wire format or in-place migration is currently defined, so no automatic
migration code is warranted yet.

Before the first version bump, update this document and the affected format
specification together, then record the result under docs/acceptance/.
