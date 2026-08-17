# Acceptance and failure-reporting guide

This directory is for reproducible acceptance records that require an actual
Agent installation, a second device or a configured S3/third-party directory
sync tool. It must never contain session JSONL, source files, credentials or
Recovery Keys.

## Local gate

Run these commands from the repository root and attach the commit hash:

```bash
go test ./...
go test -race ./...
go vet ./...
go run ./poc/mvp
```

The CI `go-test-report` artifact is the machine-readable test report for the
Ubuntu test job. A failure report should include the failing package/test
name, OS, Go version and commit, but only the redacted error class or a
minimal synthetic reproduction.

## External matrix record

For each run, record:

| Field | Value |
|---|---|
| Date / commit | |
| Source OS / Agent version | |
| Target OS / Agent version | |
| Remote type and consistency model | |
| Source and target device modes | |
| Project path shape | same / different / non-ASCII / long |
| Result | pass / fail / blocked |
| Failure class and next action | |

The minimum scenarios are: same-device local `dir`, two devices with
different project paths, Windows to POSIX, POSIX to Windows, a foreign-device
metadata-only check, a fork requiring explicit version selection, interrupted
remote publication, incomplete remote listing, and recovery after a failed
write.

## Version compatibility record

When an Agent format or Remote format changes, record the observed version,
the adapter compatibility classification, whether push remains allowed, and
whether restore requires explicit consent. Unknown versions must remain
limited or stopped according to the adapter policy; never mark them full merely
to make an acceptance run pass.

The R2-specific headless-server procedure is in
[r2-headless-device-2026-08-17.md](r2-headless-device-2026-08-17.md). It covers a second
AgentSync device without Claude Code and deliberately stops at metadata
inspection rather than claiming native session restore.
