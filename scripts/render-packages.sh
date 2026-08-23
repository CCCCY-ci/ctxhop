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
  awk -v name="$1" '{ file = $2; sub(/^\*/, "", file); if (file == name) print $1 }' "$CHECKSUMS"
}

DARWIN_AMD64_ARCHIVE_SHA256="$(hash_for "ctxhop_${VERSION}_darwin_amd64.zip")"
DARWIN_ARM64_ARCHIVE_SHA256="$(hash_for "ctxhop_${VERSION}_darwin_arm64.zip")"
LINUX_AMD64_ARCHIVE_SHA256="$(hash_for "ctxhop_${VERSION}_linux_amd64.zip")"
LINUX_ARM64_ARCHIVE_SHA256="$(hash_for "ctxhop_${VERSION}_linux_arm64.zip")"

for hash in "$DARWIN_AMD64_ARCHIVE_SHA256" "$DARWIN_ARM64_ARCHIVE_SHA256" "$LINUX_AMD64_ARCHIVE_SHA256" "$LINUX_ARM64_ARCHIVE_SHA256"; do
  if [[ -z "$hash" ]]; then
    echo "render-packages: a Unix release archive is missing from checksums.txt" >&2
    exit 1
  fi
done

mkdir -p dist/release/packages
sed \
  -e "s/@VERSION@/$VERSION/g" \
  -e "s/@DARWIN_AMD64_ARCHIVE_SHA256@/$DARWIN_AMD64_ARCHIVE_SHA256/g" \
  -e "s/@DARWIN_ARM64_ARCHIVE_SHA256@/$DARWIN_ARM64_ARCHIVE_SHA256/g" \
  -e "s/@LINUX_AMD64_ARCHIVE_SHA256@/$LINUX_AMD64_ARCHIVE_SHA256/g" \
  -e "s/@LINUX_ARM64_ARCHIVE_SHA256@/$LINUX_ARM64_ARCHIVE_SHA256/g" \
	packaging/homebrew/ctxhop.rb.in > "dist/release/packages/ctxhop.rb"
echo "rendered Homebrew manifest into dist/release/packages"
