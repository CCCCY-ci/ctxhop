# Local gate record

| Field | Value |
|---|---|
| Date | 2026-08-16 |
| Commit under test | `0135e74` (with `4f555d4`) |
| Environment | Windows workspace, Go 1.26 toolchain |
| Scope | Synthetic/local checks only; no credentials, Agent session data, or external service |

## Results

- `go test ./...` — PASS
- `go test -race ./...` — PASS
- `go vet ./...` — PASS
- Lifecycle regression coverage — PASS; local dir Remote tests cover passphrase change/reset, partial deletion failure, cancellation, history prune boundary retention and safe error classification.
- `go run ./poc/mvp` — PASS; all six local synthetic scenarios passed
- `CGO_ENABLED=0` cross-build — PASS for windows/amd64, windows/arm64, darwin/amd64, darwin/arm64, linux/amd64, and linux/arm64

The cross-build outputs were written to a temporary directory and removed after
verification. The repository's existing `dist/` artifacts were not touched.

## Not covered by this record

Real Windows/macOS/Linux Agent behavior, real Claude Code session formats,
S3-compatible providers, third-party directory synchronizers, and strong
per-device access revocation still require the external acceptance matrix or a
separate key/credential lifecycle design.