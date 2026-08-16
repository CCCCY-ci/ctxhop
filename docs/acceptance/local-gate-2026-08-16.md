# Local gate record

| Field | Value |
|---|---|
| Date | 2026-08-16 |
| Commit under test | `1a0ca09` (`feat(sync): add signed device pairing`, on top of the earlier lifecycle commits) |
| Environment | Windows workspace, Go 1.26 toolchain |
| Scope | Synthetic/local checks only; no credentials, Agent session data, or external service |

## Results

- `go test ./...` — PASS
- Signed device pairing integration: PASS; device A invitation and device B `init --invite` produce distinct device IDs with the same fingerprint and pinned identity, while tampering fails before local configuration is saved.
- `go test -race ./...` — PASS
- `go vet ./...` — PASS
- Lifecycle regression coverage — PASS; local dir Remote tests cover passphrase change/reset and rejected changes, scoped delete-all/project/session boundaries, partial deletion failure, cancellation, history prune boundary retention and safe error classification.
- Remote cancellation contract — PASS; dir and S3 implementations observe cancellation while consuming a Put body, and cancellation does not publish a partial object.
- Command registry and clean-tree checks — PASS; every registered CLI command has a handler, and `git archive HEAD` followed by `go test ./...` passes without relying on uncommitted or ignored source files.
- `go run ./poc/mvp` — PASS; all six local synthetic scenarios passed
- `CGO_ENABLED=0` cross-build — PASS for windows/amd64, windows/arm64, darwin/amd64, darwin/arm64, linux/amd64, and linux/arm64

The cross-build outputs were written to a temporary directory and removed after
verification. The repository's existing `dist/` artifacts were not touched.

## Not covered by this record

Real Windows/macOS/Linux Agent behavior, real Claude Code session formats,
S3-compatible providers, third-party directory synchronizers, and strong
per-device access revocation still require the external acceptance matrix or a
separate key/credential lifecycle design.
