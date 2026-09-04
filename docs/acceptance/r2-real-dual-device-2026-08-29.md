# Cloudflare R2 real Windows ↔ Linux dual-device acceptance

Date: 2026-08-29
Commit: `0b613acf66df2237ccdf271738593952498be136`
Result: **passed**

This run verified the Session Hub session path on two real devices using a
dedicated, short-lived R2 credential and a unique temporary prefix. The
existing Linux CtxHop configuration and its stored R2 credential were not used
for this run. The SSH private key was used only for Linux login and file
transfer.

## Matrix

| Field | Source A | Target B |
|---|---|---|
| OS / architecture | Windows amd64 | Linux amd64 (Ubuntu host) |
| Agent | Codex CLI 0.150.1 | Codex adapter with an isolated `CODEX_HOME` |
| CtxHop configuration | isolated temporary directory | isolated temporary directory |
| Project identity | manual, shared test identity | same manual identity, different absolute path |
| Scope | session only | session only |
| Remote | Cloudflare R2 S3-compatible backend, dedicated temporary prefix | same |

## Checks and results

1. An isolated `doctor --json` backend probe passed with the new credential.
2. Windows initialized a new sync domain and created a signed device invite.
   Linux joined through that invite and reported the same domain fingerprint.
3. A real Codex CLI invocation created one temporary source session. Windows
   `ctxhop push` reported `pushed: 1, failed: 0, skipped: 0`.
4. Linux `list --json` discovered the remote session with `agent=codex` and
   the original native session identity. The read-only resume preview reported
   `localState=missing`, `remoteRecordCount=14`, and a consistent workspace.
5. Linux `resume --json --agent codex` materialized the session into its
   isolated POSIX Codex session directory. The subsequent Linux list marked it
   `local: true`; the materialized session file was present and contained the
   expected 14 records.
6. Linux pushed its local branch successfully. Windows then reported both
   devices in `device list`, and `session show` reported two complete Codex
   sources for the same logical Session Hub session—one source per device.
7. Windows refused an ambiguous resume when two complete replicas were
   available and required an explicit `--replica`. Selecting the Linux replica
   produced a preview with `localState=exact`, zero differences, and no local
   file changes.
8. Windows `status --remote --json` reported one local and one remote session,
   with no pending or blocked queue items. The observed foreign update was the
   expected second-device publication, not a conflict requiring attention.

## Boundary and cleanup

This record covers real R2 authentication, invitation-based device joining,
project-scoped listing, native Codex materialization, replica selection, and
the Windows ↔ Linux session round trip. It intentionally does not claim Git,
workspace-file, or Agent continuation coverage for this run.

After collecting the results, `remote delete-all` removed exactly 25
objects from the dedicated temporary prefix. No repository file contains R2
credentials, invitation contents, Recovery Keys, or session bodies.
