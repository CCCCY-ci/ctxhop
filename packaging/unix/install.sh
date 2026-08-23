#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
source_path="$script_dir/ctxhop"

if [ ! -f "$source_path" ]; then
	printf '%s\n' "ctxhop installer: ctxhop was not found beside install.sh" >&2
	exit 1
fi

install_dir=${CTXHOP_INSTALL_DIR-}
if [ -z "$install_dir" ]; then
	install_dir=${XDG_BIN_HOME-}
fi
if [ -z "$install_dir" ]; then
	if [ -z "${HOME-}" ]; then
		printf '%s\n' "ctxhop installer: HOME is not set; use CTXHOP_INSTALL_DIR" >&2
		exit 1
	fi
	install_dir="$HOME/.local/bin"
fi

case "$install_dir" in
	/*) ;;
	*) install_dir="$PWD/$install_dir" ;;
esac

mkdir -p "$install_dir"
temporary_path=$(mktemp "$install_dir/.ctxhop-install.XXXXXX")
cleanup() {
	rm -f "$temporary_path"
}
trap cleanup 0 1 2 3 15

cp "$source_path" "$temporary_path"
chmod 755 "$temporary_path"
mv -f "$temporary_path" "$install_dir/ctxhop"
trap - 0 1 2 3 15

case ":${PATH-}:" in
	*":$install_dir:"*)
		printf 'CtxHop was installed at %s/ctxhop\n' "$install_dir"
		printf '%s\n' "Open a new shell, then run: ctxhop version"
		;;
	*)
		printf 'CtxHop was installed at %s/ctxhop\n' "$install_dir"
		printf 'Add this directory to PATH before using the command:\n  export PATH=%s:\$PATH\n' "$install_dir"
		;;
esac
