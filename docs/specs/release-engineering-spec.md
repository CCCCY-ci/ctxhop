# Spec: release engineering

| | |
|---|---|
| Status | Implemented locally; external package repositories remain opt-in |
| Targets | Windows amd64/arm64, macOS amd64/arm64, Linux amd64/arm64 |

## 1. Verification

Pull requests and pushes run native `go test ./...`, `go vet ./...` and a
native build on Ubuntu, macOS and Windows. Ubuntu also runs
`go test -race ./...` and the cross-platform build matrix.

The workflow deliberately does not require Claude Code, S3 credentials or a
remote service. Those are separate acceptance fixtures and are not safe to
put into a public CI job.

## 2. Local and tagged builds

`scripts/build.sh` and `scripts/build.ps1` cross-compile with `CGO_ENABLED=0`
and inject version, commit and UTC build time. `scripts/release.sh` copies
versioned binaries into `dist/release`, writes `checksums.txt`, and renders
the Homebrew and Scoop manifests from `packaging/` templates.

A tag matching `v*` runs the release workflow. It executes tests and vet
before using the GitHub CLI to publish the six binaries, checksums and package
manifests to the GitHub Release.

The release assets are intentionally raw, versioned binaries. This keeps the
verification path transparent; consumers can verify the published SHA-256
file before installation.

## 3. Package-manager handoff

`packaging/homebrew/agentsync.rb.in` and
`packaging/scoop/agentsync.json.in` are rendered with release-specific URLs
and hashes. Publishing them still requires a maintainer-owned Homebrew tap or
Scoop bucket; the project does not silently create or modify third-party
repositories.

The rendered files are release assets so a tap/bucket maintainer can review
and copy them without recomputing hashes.

## 4. Upgrade and rollback

The binary is self-contained and does not migrate configuration on startup.
Upgrade by replacing the executable, keep the existing configuration and
remote data, and verify with `agentsync version` and `agentsync doctor`.
Rollback is the same operation with a previously verified binary. A release
must not delete or rewrite user configuration as part of installation.

## 5. Test plan

CI covers the Go test/vet/race matrix and cross-build outputs. The release
script validates version characters, requires all six binaries before
rendering manifests, and fails when a checksum entry is missing.