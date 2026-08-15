# Spec: `agentsync project`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `config-layer-spec.md`, `internal/project`, `device-mode-spec.md` |

## 1. Invocation

The project policy command is local and never contacts the configured backend:

```text
agentsync project bind [--path DIR] [--name NAME | --identity ID]
agentsync project unbind [--path DIR] [--identity ID]
agentsync project mode normal|push-only|excluded [--path DIR | --identity ID]
agentsync project list [--json]
```

The default path is the current directory. `--identity` selects an existing
stable project identity exactly. `--name` creates a manual identity in the
`manual:<name>` namespace and is only valid for `bind`.

## 2. Binding behavior

`bind` resolves the selected directory to an absolute project root and
persists a binding between that root and one stable identity.

Without an explicit identity, the command uses the Git remote identity found by
`project.Identify`. An unstable directory is rejected with guidance to use
`--name` for a manual project. An explicit `--identity` is retained
verbatim after non-empty and NUL validation.

The same identity/root pair is idempotent. Multiple roots may be bound to one
identity so a user can keep the same project on several local paths. One local
root may not be bound to two different identities; the command stops without
changing configuration in that case.

## 3. Unbinding and modes

`unbind` removes only explicit bindings. With both `--identity` and
`--path`, it removes that exact pair. With only `--identity`, it removes
all local roots for the identity. With only `--path`, it removes bindings for
that local root. With neither, it resolves the current directory as the
default path. An unbind that matches nothing is an error and does not save.

`mode normal` removes the identity from both policy lists. `mode push-only`
adds it to `pushOnly` and removes it from `excluded`. `mode excluded`
adds it to `excluded` and removes it from `pushOnly`. The two restrictive
modes are mutually exclusive and the identity lists are deduplicated and
sorted before saving.

A mode may be set for an identity without a binding. This supports a policy
for a project whose local path is not currently available. When targeting by
path, the directory must have a stable identity.

## 4. Listing

`project list` reads only local configuration. It reports every identity
present in bindings or policy lists, its effective mode, and its explicitly
bound local roots. `normal` is the effective mode when an identity appears
in neither restrictive list.

The text report is aggregate and human-readable. `--json` writes one JSON
document to stdout; this command has no interactive prompt. The list may show
the paths and identities explicitly requested by the user, but never includes
backend credentials, passphrases, or remote session content.

## 5. Persistence and failure behavior

All mutations use `Config.Save`, which validates the complete configuration
and atomically replaces `config.json`. A failed identity lookup, invalid
target, conflicting root, or failed save leaves the prior configuration
unchanged.

The command does not read secrets, keyfiles, remote metadata, or session
shards. It cannot trigger a push, pull, restore, or observed-tip update.

## 6. Test plan

Tests cover option parsing, remote and manual identity binding, idempotent
binding, conflicting roots, exact and broad unbinding, mutually exclusive
modes, deterministic list output, JSON output, atomic save failures, and the
absence of backend or secret access.
