# Spec: device synchronization mode

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `config-layer-spec.md`, `syncflow-device-policy-spec.md`, `cli-status-spec.md` |

## 1. Configuration

The optional `Config.Device.Mode` field controls participation for one
installation. Its accepted values are:

* `normal`: push local sessions and use the explicit metadata/list/resume
  pull flows;
* `push-only`: push local sessions, but do not inspect, list, or restore
  remote sessions on this device;
* `disabled`: do not push local sessions and do not inspect, list, or restore
  remote sessions.

A missing field is treated as `normal`, so configurations written before
device modes remain readable without migration. Unknown values make the
configuration invalid rather than silently changing synchronization behavior.

New installations can select the mode with:

```text
agentsync init --device-mode normal
agentsync init --device-mode push-only
agentsync init --device-mode disabled
```

After initialization, the local setting can be inspected or changed without
editing `config.json`:

```text
agentsync device status
agentsync device mode normal|push-only|disabled
```

The management command changes only the persisted mode. It never regenerates
the opaque device ID or contacts the backend.

The mode is configuration state, not a device identifier. `Config.Device.ID`
continues to identify the local branch and is never derived from the display
name, hostname, path, or mode.

## 2. Read and write boundaries

The mode is checked before secrets, keyfiles, remote metadata, or session
shards are opened:

| Mode | Push | Metadata/list/resume pull | Automatic self-pull |
|---|---|---|---|
| `normal` | allowed | explicit flow only | none |
| `push-only` | allowed | blocked | none |
| `disabled` | skipped | blocked | none |

`agentsync status` remains local and reports the effective mode in its
redacted configuration. `agentsync status --remote` reports a device
boundary without prompting for a passphrase when the mode blocks remote
inspection.

## 3. Relationship to device branches

In `normal` and `push-only` modes, a push writes only the local device
branch. It does not list or download the local branch again. Foreign branch
changes are discovered by the metadata-only check and are restored only by
the explicit `resume` operation. The device mode is therefore an additional
local permission boundary; it does not replace branch identity or observed
foreign-tip state.

## 4. Safety

A disabled device returns a skipped push summary before project discovery
and remote setup. A push-only or disabled pull command returns a redacted
mode error before project secrets, backend credentials, keyfiles, or remote
session data are read.
