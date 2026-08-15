# Spec: `agentsync stats`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `cli-resume-spec.md`, `device-mode-spec.md` |

## 1. Invocation

```text
agentsync stats [--json]
```

The command reads only the local aggregate state below the configured
AgentSync configuration directory. It does not require a valid `config.json`,
does not unlock secrets, and never contacts the configured backend.

An absent state file is a valid zero state:

```text
scope: local
cross-device-restores: 0
last-restored: never
```

## 2. Measurement

The statistic counts successful `resume` operations whose selected restore
version contains at least one source device other than the local device.
Restoring a version produced only by the local device does not increment the
counter. A version with both local and foreign sources is cross-device.

The count is an aggregate. It does not retain project IDs, session IDs, device
IDs, paths, titles, record content, or backend details. `lastRestoredAt` is a
local UTC timestamp for the most recent counted restore; it is advisory and is
never used by synchronization or pull decisions.

The state is stored at `state/v1/stats.json` below the local configuration
directory. Each update is written through the shared atomic-file primitive.
The command is intentionally local so its output can be pasted into an issue
without disclosing user or workspace data.

## 3. Resume integration

After `resume` has successfully written the Agent session, it records one
cross-device restore when the selected plan has a foreign source device.
The operation is not rolled back if the statistics write fails. In that case
`resume` reports that the restore completed but local statistics could not
be saved. A later successful restore may record a new event; the command does not
attempt to infer or repair a missing event.

Statistics are recorded before the advisory observed-tip state is saved. A
failure of either local state write is reported separately, and neither
failure changes the already completed Agent restore.

## 4. Output

Without `--json`, the command emits only aggregate fields:

```text
scope: local
cross-device-restores: 3
last-restored: 2026-08-15T02:05:00Z
```

With `--json`, stdout contains one JSON document:

```json
{
  "scope": "local",
  "crossDeviceRestores": 3,
  "lastRestoredAt": "2026-08-15T02:05:00Z"
}
```

When no restore has been counted, `lastRestoredAt` is omitted from JSON and
the text form prints `never`. Both forms exclude local paths, configuration,
credentials, project names, session selectors, and session content.

## 5. Failure behavior

Malformed or newer local statistics are errors. The reader rejects unknown
JSON fields, trailing JSON, invalid versions, and invalid timestamps. A
cancelled context stops before the corresponding read or atomic write.

If an update cannot be persisted after a successful Agent restore, the caller
receives an explicit local-statistics error. The Agent data remains restored;
there is no destructive rollback.

## 6. Test plan

Tests cover absent state, atomic round trips, same-device no-op behavior,
foreign-source increments, timestamp normalization, malformed and newer
state, cancellation, command registration, deterministic text and JSON
output, and the absence of configuration or remote reads.
