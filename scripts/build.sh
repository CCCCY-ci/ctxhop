#!/usr/bin/env bash
#
# Cross-compile ctxhop for every supported target from a single machine.
#
# CGO is disabled on purpose. It is what makes cross-compilation a one-liner and
# what guarantees a single statically linked CLI binary per platform, which is
# used as an internal input for the release installer/archive packaging. Any dependency that requires cgo
# (notably some SQLite bindings) is disqualified — use a pure-Go alternative.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="github.com/CCCCY-ci/ctxhop/cmd/ctxhop"

LDFLAGS="-s -w \
  -X main.version=${VERSION} \
  -X main.commit=${COMMIT} \
  -X main.date=${DATE}"

# Platform list mirrors the MVP compatibility matrix in the PRD, plus linux so
# CI can build and test on its native runner.
TARGETS=(
  "windows/amd64"
  "windows/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "linux/amd64"
  "linux/arm64"
)

rm -rf dist
mkdir -p dist

for target in "${TARGETS[@]}"; do
  GOOS="${target%%/*}"
  GOARCH="${target##*/}"

  out="dist/ctxhop_${GOOS}_${GOARCH}"
  if [ "$GOOS" = "windows" ]; then
    out="${out}.exe"
  fi

  echo "building ${GOOS}/${GOARCH}"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$out" "$PKG"
done

# When the current host is part of the target matrix, start that binary before
# publishing the build directory. Cross-target binaries remain compile-only
# here and are covered by native acceptance jobs on their target systems.
host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
host_suffix=""
if [[ "$host_os" = "windows" ]]; then
  host_suffix=".exe"
fi
host_binary="dist/ctxhop_${host_os}_${host_arch}${host_suffix}"
if [[ -f "$host_binary" ]]; then
  echo "smoke-testing ${host_os}/${host_arch}"
  if ! version_output="$("$host_binary" version)"; then
    echo "build: host binary failed to start for version" >&2
    exit 1
  fi
  if [[ "$version_output" != ctxhop\ * ]]; then
    echo "build: host binary returned an unexpected version line" >&2
    exit 1
  fi
  if ! help_output="$("$host_binary" help)"; then
    echo "build: host binary failed to start for help" >&2
    exit 1
  fi
  if [[ "$help_output" != *"commands:"* ]]; then
    echo "build: host binary returned incomplete help" >&2
    exit 1
  fi
else
  echo "host target ${host_os}/${host_arch} is outside the build matrix; startup smoke skipped"
fi

echo
echo "built ${#TARGETS[@]} binaries into dist/"
ls -lh dist
