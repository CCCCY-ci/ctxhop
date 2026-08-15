# Spec: `agentsync device`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `config-layer-spec.md`, `device-mode-spec.md` |

## 1. Invocation

The command manages settings for the current local installation. It never
contacts the configured backend and never reads `secrets`.

```text
agentsync device status [--json]
agentsync device mode normal|push-only|disabled
```

`device status` reports whether an opaque local device identity is configured
and the effective synchronization mode. It does not print the opaque device
identifier, display name, backend settings, local paths, or credentials.

The text form is:

```text
device:
  identity: configured
  mode: normal
```

The `--json` form contains the same redacted information:

```json
{
  "device": {
    "configured": true,
    "mode": "normal"
  }
}
```

## 2. Changing the mode

`device mode` accepts the same three values as `init --device-mode`. Input is
trimmed and matched case-insensitively, then persisted in normalized lowercase
form. A missing mode or an unknown value is an error.

The command loads the complete existing configuration, changes only
`Config.Device.Mode`, validates the result, and saves it through
`Config.Save`. The atomic save preserves the previous valid configuration if
the write fails. Legacy configurations with an omitted mode become explicitly
`normal` when the user selects `normal`.

Changing the mode does not change the opaque device identifier, display name,
remote branch, project bindings, secrets, or observed pull state.

## 3. Failure behavior

The command stops if the local configuration is absent, malformed, from a
newer version, or otherwise invalid. It never creates a default configuration
as a side effect. Invalid mode input is rejected before any configuration is
written.
