# Cloudflare R2 single-device acceptance record

This record captures the single-device R2 verification completed on 2026-08-17.
It intentionally contains no credential values, session bodies, invitation
contents, or Recovery Keys.

| Field | Value |
|---|---|
| Commit under test | `df2ab21` (`feat(sync): finish single-device sync hardening`) |
| Host | Windows amd64 |
| Scope | One configured AgentSync device with an isolated test prefix |
| Remote | Cloudflare R2 through its S3-compatible API |
| Credential | Short-lived test credential; revoke/delete it after the run |
| Result | PASS |

## Results

- The opt-in `TestS3Integration` passed against the R2 bucket, including the
  bounded listing/read/write/cleanup probe.
- The 1001-object pagination scenario passed and removed only the objects it
  created under the dedicated test prefix.
- The real CLI flow passed: `init`, project binding, session upload, remote
  `list`/`status`, metadata-only `pull --check`, `resume`, and exact-prefix
  cleanup.
- The local single-device CLI matrix also passed upload, repeated upload,
  metadata inspection, pull checks, history, watch, resume, passphrase change
  and reset, device key rotation, device modes, project policies, and remote
  deletion.

## Boundaries

This is a single-device/provider acceptance record. It does not claim that the
following are complete:

- a real second device joining the same domain and restoring a session;
- Windows-to-POSIX path localization or native Agent resume on another OS;
- third-party S3-compatible providers, directory synchronizers, or a full
  remote failure matrix;
- a live, still-open Agent session being synchronized while it is being
  written.

The R2 procedure for the later headless second-device run remains in
[`r2-headless-device-2026-08-17.md`](r2-headless-device-2026-08-17.md).