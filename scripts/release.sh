#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-${GITHUB_REF_NAME:-$(git describe --tags --always 2>/dev/null || echo dev)}}"
VERSION="${VERSION#v}"
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "release: VERSION contains unsupported characters" >&2
  exit 1
fi

VERSION="$VERSION" bash ./scripts/build.sh

RELEASE_DIR="dist/release"
rm -rf "$RELEASE_DIR"
mkdir -p "$RELEASE_DIR"

build_windows_installer() {
  local arch="$1"
	local cli="dist/ctxhop_windows_${arch}.exe"
	local stub="dist/ctxhop-installer_windows_${arch}.exe"
	local output="$RELEASE_DIR/CtxHop-Setup_${VERSION}_windows_${arch}.exe"

  echo "building Windows installer for windows/${arch}"
  CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -H=windowsgui" \
    -o "$stub" ./cmd/ctxhop-installer
  CGO_ENABLED=0 go run ./cmd/ctxhop-installer \
    --pack --stub "$stub" --payload "$cli" --output "$output"
}

build_unix_zip() {
  local os_name="$1"
  local arch="$2"
	local binary="dist/ctxhop_${os_name}_${arch}"
	local archive="$RELEASE_DIR/ctxhop_${VERSION}_${os_name}_${arch}.zip"
  local staging

  staging="$(mktemp -d "${TMPDIR:-/tmp}/ctxhop-release.XXXXXX")"
	cp "$binary" "$staging/ctxhop"
  cp packaging/unix/install.sh LICENSE NOTICE "$staging/"
	chmod 755 "$staging/ctxhop" "$staging/install.sh"
	zip -q -9 -j "$archive" "$staging/ctxhop" "$staging/install.sh" "$staging/LICENSE" "$staging/NOTICE"
  rm -rf "$staging"
}

if ! command -v zip > /dev/null 2>&1; then
  echo "release: zip is required to create Unix release packages" >&2
  exit 1
fi

build_windows_installer amd64
build_windows_installer arm64
build_unix_zip darwin amd64
build_unix_zip darwin arm64
build_unix_zip linux amd64
build_unix_zip linux arm64

expected_assets=(
	"CtxHop-Setup_${VERSION}_windows_amd64.exe"
	"CtxHop-Setup_${VERSION}_windows_arm64.exe"
	"ctxhop_${VERSION}_darwin_amd64.zip"
	"ctxhop_${VERSION}_darwin_arm64.zip"
	"ctxhop_${VERSION}_linux_amd64.zip"
	"ctxhop_${VERSION}_linux_arm64.zip"
)
for expected in "${expected_assets[@]}"; do
  if [[ ! -f "$RELEASE_DIR/$expected" ]]; then
    echo "release: missing expected asset $expected" >&2
    exit 1
  fi
done
if ! compgen -G "$RELEASE_DIR/CtxHop-Setup_*" > /dev/null || \
	! compgen -G "$RELEASE_DIR/ctxhop_*.zip" > /dev/null; then
  echo "release: no release assets were produced" >&2
  exit 1
fi

(cd "$RELEASE_DIR" && sha256sum CtxHop-Setup_* ctxhop_*.zip > checksums.txt)
if ! (cd "$RELEASE_DIR" && sha256sum --check checksums.txt > /dev/null); then
  echo "release: checksum self-check failed" >&2
  exit 1
fi
VERSION="$VERSION" bash ./scripts/render-packages.sh
echo "release assets written to $RELEASE_DIR"
cat "$RELEASE_DIR/checksums.txt"
