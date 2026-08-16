#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-${GITHUB_REF_NAME:-$(git describe --tags --always 2>/dev/null || echo dev)}}"
VERSION="${VERSION#v}"
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "release: VERSION contains unsupported characters" >&2
  exit 1
fi

VERSION="$VERSION" ./scripts/build.sh

RELEASE_DIR="dist/release"
rm -rf "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR"

for binary in dist/agentsync_*; do
  [[ -f "$binary" ]] || continue
  filename="$(basename "$binary")"
  target="${filename#agentsync_}"
  cp "$binary" "$RELEASE_DIR/agentsync_${VERSION}_${target}"
done

if ! compgen -G "$RELEASE_DIR/agentsync_*" > /dev/null; then
  echo "release: no binaries were produced" >&2
  exit 1
fi

(cd "$RELEASE_DIR" && sha256sum agentsync_* > checksums.txt)
VERSION="$VERSION" ./scripts/render-packages.sh
echo "release assets written to $RELEASE_DIR"
cat "$RELEASE_DIR/checksums.txt"