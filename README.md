# AgentSync

Continue the same AI coding session on another machine.

> **Status: pre-alpha, not usable yet.** The feasibility tests have passed and
> the lower layers are being built, but there is no command that syncs anything
> yet. See [Project status](#project-status).

---

## The problem

Claude Code and similar agents keep their sessions on the machine that created
them. A session is hours of accumulated context: what you asked for, what the
agent read, what it changed, what it decided and why.

Switch to another machine — desktop to laptop, work to home, Windows to macOS —
and none of it comes with you. Same account, same Git repository, same project:
the session still stays behind. So you re-explain everything, or you copy-paste
transcripts, or you leave the first machine running so you can SSH back into it.

AgentSync moves the session instead, so the agent on the second machine picks up
where the first one stopped.

## How it works

A single binary runs locally. It finds sessions your agent has written, encrypts
them on your machine, and stores them in a backend **you own** — an S3-compatible
bucket, or a directory you already sync with something else.

There is no AgentSync server. No account to create. Nothing to trust us with.

```
agentsync resume      # pick a session, restore it onto this machine
claude --resume       # it is now in your agent's own session list
```

The source machine does not need to be online.

## Why not just sync `~/.claude` with Syncthing or iCloud Drive?

Because that corrupts sessions rather than moving them:

- **Sessions are bound to absolute paths.** The directory holding them can
  encode the project's full path, and the session records its working directory
  internally. Copied as-is to a machine where the project lives somewhere else,
  the agent does not recognise it. Restoring correctly is a rewrite, not a copy.
- **Conflicts resolve the wrong way.** A session is an append-only log. File
  sync tools apply last-write-wins (silently discarding one side's entire
  conversation) or drop a `conflict-copy` file the agent cannot read. Two
  machines continuing one session need to fork, not fight.
- **Some agents keep live databases.** Copying SQLite files while the agent
  holds them open produces corruption, not a backup.
- **The granularity is wrong.** An agent's data directory also holds
  credentials, tokens, caches and logs. Syncing all of it copies your API keys
  to every machine you own.

AgentSync can *use* a synced folder as its transport — the storage layout
guarantees two devices never write the same object — but it never delegates the
semantics.

It also does something no file sync tool can: before restoring, it checks
whether the files this session actually touched still match what the session
believes. Otherwise the agent resumes with a confidently wrong picture of your
code, which is worse than not resuming at all.

## Privacy

- Everything is encrypted on your machine before it is written anywhere,
  including metadata and session titles.
- The key is derived from your passphrase. It is never uploaded and cannot be
  recovered by anyone, including you, if you lose both it and your recovery key.
- **This tool collects no data of any kind.** No telemetry, no crash reporting,
  no opt-in analytics, no phone-home. It contacts nothing except the storage
  backend you configured.
- You choose which projects sync. Work machines can be set to push only, or to
  exclude specific projects entirely.

## Project status

| Stage | State |
|---|---|
| Design (PRD) | Done — see [`docs/`](docs/) |
| PoC-1: cross-device, cross-path restore | Passed |
| PoC-2: workspace consistency fingerprint | Passed |
| `internal/adapter` — Claude Code | Done |
| `internal/remote` — directory and S3 | Done |
| `internal/crypto` — encryption and keys | Done, awaiting merge |
| `internal/project` — git identity, mapping, consistency | Done |
| `internal/syncer`, `internal/config`, CLI | Not started |
| MVP | Not started |

PoC-1 asked whether a Claude Code session can be moved between machines with
different project paths and still resume natively. It can, and the conclusions
are written up in [`docs/specs/`](docs/specs/).

The remaining cross-device behaviour — a genuinely different operating system,
a different username, reading a session while the agent writes it — is still
verified only through a simulated second device.

## Building

Requires Go 1.26 or newer. Builds with `CGO_ENABLED=0` into a single static
binary; nothing here needs a C toolchain.

```bash
go build ./cmd/agentsync      # current platform
./scripts/build.sh            # all supported platforms into dist/
```

## Design documents

- [`docs/AgentSync PRD v2.0（无服务端开源版）.md`](docs/) — current design
- [`docs/archive/`](docs/archive/) — earlier revisions, kept for context

Two interfaces define the extension points:

- [`internal/adapter`](internal/adapter/adapter.go) — support a new agent
- [`internal/remote`](internal/remote/remote.go) — support a new storage backend

Contributions are welcome once PoC-1 has settled the core design.

## License

[Apache-2.0](LICENSE). If you redistribute a modified version, keep the
[`NOTICE`](NOTICE) file and mark the files you changed.
