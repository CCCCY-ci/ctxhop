# Spec: `agentsync history <session>`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `cli-list-spec.md`, `cli-resume-spec.md`, `syncer-remote-read-spec.md`, `syncflow-restore-spec.md` |

## 1. Invocation

`history` requires one session selector:

```text
agentsync history [--json] SESSION
```

The selector may be the native Agent session ID, the keyed remote session ID,
or the native ID used when the session was pushed. The command never prompts
for session selection. A passphrase prompt, when needed, is written to the
interactive prompt stream; `--json` therefore leaves stdout as one JSON
document.

## 2. Version model

The command first reads authenticated project metadata to resolve the
selector, then reads and validates the complete immutable device branches for
that session. It applies the same non-destructive branch resolution used by
`resume`.

The report contains the currently recoverable maximal versions. The zero-based
version index is the same index accepted by `resume --version`. A consistent
session has one version; a fast-forward session has one maximal version and a
non-zero common prefix; a fork has one entry per incomparable maximal version.
Earlier immutable prefixes remain in the backend, but are not presented as
separate restore choices by this command.

Version order is derived from canonical session content and source-device
identifiers, never from backend modification times. The optional
`updatedAt` value is taken from an authenticated session summary only when
that summary's tip matches the version; it is advisory and may be absent when
metadata is stale or unsupported.

## 3. Boundaries

The command follows the same local device and project policy as `list` and
`resume`:

* a disabled or push-only device cannot inspect remote session bodies;
* excluded and push-only projects stop before keyfile unlock;
* the local device ID is used only to label a matching source as `local`;
* opaque session and device identifiers are not interpreted as paths.

History is read-only. It does not write Agent files, publish or delete remote
objects, advance the push cursor, save observed pull tips, or change
configuration.

## 4. Output

The text report is aggregate and selection-oriented:

```text
scope: session
session: native-session
title: continue the migration
resolution: fork
common-prefix: 12
versions: 2
- version=0 records=20 updated=2026-08-15T02:00:00Z sources=device-a
- version=1 records=21 updated=2026-08-15T02:05:00Z sources=local
```

The JSON report contains `scope`, the selected session label, the sanitized
title when metadata supports it, the resolution kind, the common-prefix
record count, and version entries with their zero-based index, record count,
source labels, and optional advisory update time. It never contains
passphrases, backend configuration, local paths, or session record content.

## 5. Failure behavior

Invalid configuration, unavailable storage, failed keyfile authentication,
unsupported session metadata, incomplete branches, malformed shards, and
branch divergence that cannot be resolved are errors. A partial branch is
never shown as a valid history version.

Cancellation stops before the next remote read and is returned to the caller.
Remote read failures are presented as a safe aggregate error rather than
leaking backend paths or credentials.

## 6. Test plan

Tests cover option parsing and command registration, selector matching,
device/project boundaries, deterministic text and JSON output, consistent
and forked version reports, complete-branch validation, metadata-only
timestamp fallback, and the absence of Agent or pull-state writes.
