#!/bin/sh
# Installs selfaction from its GitHub releases on Linux and macOS:
#
#   curl -fsSL https://raw.githubusercontent.com/broisnees/selfaction/main/install.sh | sh
#
# SELFACTION_VERSION      a release tag such as v0.3.0 (default: the latest release)
# SELFACTION_INSTALL_DIR  where to put the binary (default: /usr/local/bin if
#                         writable, else ~/.local/bin)
#
# The archive's SHA-256 is checked against the release's checksums.txt.
set -eu

repo=broisnees/selfaction
base=${SELFACTION_DOWNLOAD_URL:-https://github.com/$repo/releases}
version=${SELFACTION_VERSION:-latest}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

case $(uname -s) in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported system $(uname -s); on Windows use install.ps1" ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

if command -v curl >/dev/null; then
	fetch() { curl -fsSL -o "$2" "$1"; }
	latest() { curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/latest"; }
elif command -v wget >/dev/null; then
	fetch() { wget -qO "$2" "$1"; }
	latest() { wget -S --spider "$base/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -1 | tr -d '\r'; }
else
	fail "curl or wget is required"
fi

if [ "$version" = latest ]; then
	url=$(latest) || fail "could not find the latest release at $base"
	version=${url##*/}
	case $version in v*) ;; *) fail "no release found at $base" ;; esac
fi

name="selfaction_${version#v}_${os}_${arch}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading selfaction $version for $os/$arch"
fetch "$base/download/$version/$name.tar.gz" "$tmp/$name.tar.gz" || fail "download failed: $base/download/$version/$name.tar.gz"
fetch "$base/download/$version/checksums.txt" "$tmp/checksums.txt" || fail "download failed: checksums.txt"

want=$(awk -v f="$name.tar.gz" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$name.tar.gz is not in checksums.txt"
if command -v sha256sum >/dev/null; then
	got=$(sha256sum "$tmp/$name.tar.gz" | awk '{print $1}')
else
	got=$(shasum -a 256 "$tmp/$name.tar.gz" | awk '{print $1}')
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
dir=${SELFACTION_INSTALL_DIR:-}
if [ -z "$dir" ]; then
	if [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="$HOME/.local/bin"
	fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/$name/selfaction" "$dir/selfaction" 2>/dev/null || {
	cp "$tmp/$name/selfaction" "$dir/selfaction"
	chmod 0755 "$dir/selfaction"
}
echo "installed $("$dir/selfaction" --version) to $dir/selfaction"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "add $dir to your PATH to run it as selfaction" ;;
esac
