#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${VERSION:-${1:-}}"
if [[ -z "$VERSION" ]]; then
  echo "render-packages: VERSION is required" >&2
  exit 1
fi
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "render-packages: VERSION contains unsupported characters" >&2
  exit 1
fi

RELEASE_DIR="dist/release"
CHECKSUMS="$RELEASE_DIR/checksums.txt"
if [[ ! -f "$CHECKSUMS" ]]; then
  echo "render-packages: checksums.txt is missing" >&2
  exit 1
fi

hash_for() {
  awk -v name="$1" '$2 == name { print $1 }' "$CHECKSUMS"
}

WINDOWS_AMD64_SHA256="$(hash_for "agentsync_${VERSION}_windows_amd64.exe")"
WINDOWS_ARM64_SHA256="$(hash_for "agentsync_${VERSION}_windows_arm64.exe")"
DARWIN_AMD64_SHA256="$(hash_for "agentsync_${VERSION}_darwin_amd64")"
DARWIN_ARM64_SHA256="$(hash_for "agentsync_${VERSION}_darwin_arm64")"
LINUX_AMD64_SHA256="$(hash_for "agentsync_${VERSION}_linux_amd64")"
LINUX_ARM64_SHA256="$(hash_for "agentsync_${VERSION}_linux_arm64")"

for hash in "$WINDOWS_AMD64_SHA256" "$WINDOWS_ARM64_SHA256" "$DARWIN_AMD64_SHA256" "$DARWIN_ARM64_SHA256" "$LINUX_AMD64_SHA256" "$LINUX_ARM64_SHA256"; do
  if [[ -z "$hash" ]]; then
    echo "render-packages: a release binary is missing from checksums.txt" >&2
    exit 1
  fi
done

mkdir -p dist/release/packages
sed \
  -e "s/@VERSION@/$VERSION/g" \
  -e "s/@WINDOWS_AMD64_SHA256@/$WINDOWS_AMD64_SHA256/g" \
  -e "s/@WINDOWS_ARM64_SHA256@/$WINDOWS_ARM64_SHA256/g" \
  -e "s/@DARWIN_AMD64_SHA256@/$DARWIN_AMD64_SHA256/g" \
  -e "s/@DARWIN_ARM64_SHA256@/$DARWIN_ARM64_SHA256/g" \
  -e "s/@LINUX_AMD64_SHA256@/$LINUX_AMD64_SHA256/g" \
  -e "s/@LINUX_ARM64_SHA256@/$LINUX_ARM64_SHA256/g" \
  packaging/homebrew/agentsync.rb.in > "dist/release/packages/agentsync.rb"
sed \
  -e "s/@VERSION@/$VERSION/g" \
  -e "s/@WINDOWS_AMD64_SHA256@/$WINDOWS_AMD64_SHA256/g" \
  -e "s/@WINDOWS_ARM64_SHA256@/$WINDOWS_ARM64_SHA256/g" \
  packaging/scoop/agentsync.json.in > "dist/release/packages/agentsync.json"
echo "rendered package-manager manifests into dist/release/packages"
