# Spec: `agentsync doctor`

| | |
|---|---|
| Status | Implemented locally |
| Depends on | configuration directory, atomic file writes, adapter/backend probes |

## 1. Report

`doctor` reports the current configuration, backend, Agent, project and a
bounded local recent-error history. The history is represented in both text
and JSON output.

Each error event contains only:

* a UTC timestamp;
* the allowlisted top-level command name, or `unknown`;
* a stable error class such as `timeout`, `not-initialized`, or
  `command-failed`.

The underlying error string, command arguments, session identifiers, paths,
credentials and session content are never persisted or printed as part of the
history.

## 2. Persistence

Failed CLI commands are recorded best-effort at the top-level command error
exit. The file is `${AGENTSYNC_CONFIG_DIR}/error-history.json`, or the normal
platform configuration directory when that override is not set.

The file is versioned, atomically replaced, limited to the newest 20 events
and bounded to 64 KiB. A missing file is an empty history. A malformed or
unsupported file does not prevent the rest of `doctor` from running; the
recent-error section is marked unavailable.

The diagnostic writer itself never changes the command's exit code or masks
the original error.

## 3. Output

Text output uses:

```text
recent errors: 2 recorded
  - 2026-08-16T10:20:00Z command=resume class=command-failed
```

JSON places the same data under `recentErrors` with `status`, `count` and
optional `events` fields. The event values are already redacted before the
doctor formatter sees them.

## 4. Test plan

Tests cover missing history, bounded retention, unsafe-token redaction,
atomic persistence, stable top-level error classification, and the absence
of sensitive command arguments from the recorded representation.