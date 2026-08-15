# Spec: `agentsync resume`

| | |
|---|---|
| Status | Draft |
| Date | 2026-08-15 |
| Depends on | `cli-list-spec.md`, `syncflow-restore-spec.md`, `syncflow-restore-apply-spec.md`, `syncflow-session-summary-spec.md` |

## 1. Selection and read order

`agentsync resume [session]` unlocks the configured remote keyfile, reads the
project's encrypted metadata, and selects one native session. Without a
session argument it presents a numbered selection. `--json` requires an
explicit session so machine-readable output is never mixed with a prompt.

Only after selection does the command read encrypted shard bodies for that one
session. Project-level listing never downloads shard contents.

## 2. Safety gates

The selected session must have authenticated metadata whose durable record
count and head digest match the resolved remote version, and whose encrypted
summary contains a source workspace fingerprint. Missing or stale fingerprint
evidence stops the operation. The command then delegates to
`syncflow.FetchRestorePlan` and `syncflow.ApplyRestore`.

The default behavior refuses:

* unverified Agent compatibility (`--allow-limited` is explicit);
* divergent workspaces (`--allow-divergent` is explicit);
* replacing an existing local session (`--replace-existing` is explicit);
* selecting an ambiguous fork without `--version`.

Every write goes through the adapter's atomic session writer. A failed
precondition never creates or replaces an Agent file.

## 3. Device behavior and privacy

Resume is an explicit body-read and restore operation. It is separate from the
automatic pull check, which only reads changed foreign metadata and excludes
the local device branch. The remote source device list is opaque scheduling
metadata; paths, credentials, endpoints, and plaintext records are not printed
by the command.

After a successful restore, the command persists only the opaque tips for
devices included in the selected restore version. The local device and other
fork versions are not marked as observed. If this local marker cannot be
saved, the command reports the error after the Agent write; it never attempts
to roll back the restored session.

The source fingerprint contains only Git state, relative file names, and
content digests. It is encrypted inside the session summary and is used only
for the target workspace comparison.

## 4. Test plan

Tests cover native and interactive selection, fork selection, missing and stale
fingerprints, limited compatibility, workspace divergence, existing-session
replacement, and an end-to-end encrypted restore into a separate Agent data
directory.
