#!/bin/sh
# Builds release archives of selfaction for every supported platform, with a
# checksums file:
#
#   scripts/release.sh v0.3.0          # into dist/
#   scripts/release.sh v0.3.0 out/
#
# Linux and macOS get selfaction_<version>_<os>_<arch>.tar.gz, Windows a .zip.
# Every build is static (CGO_ENABLED=0), so it runs without Go installed.
set -eu

version=${1:?usage: scripts/release.sh <version> [output dir]}
out=${2:-dist}
targets=${TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"}

cd "$(dirname "$0")/.."
rm -rf "$out"
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for target in $targets; do
	os=${target%/*}
	arch=${target#*/}
	name="selfaction_${version#v}_${os}_${arch}"
	exe=selfaction
	[ "$os" = windows ] && exe=selfaction.exe
	mkdir -p "$work/$name"
	echo "building $name"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$work/$name/$exe" .
	cp README.md "$work/$name/"
	if [ "$os" = windows ]; then
		(cd "$work" && zip -qr "$out/$name.zip" "$name")
	else
		tar -czf "$out/$name.tar.gz" -C "$work" "$name"
	fi
done

cd "$out"
if command -v sha256sum >/dev/null; then
	sha256sum selfaction_* >checksums.txt
else
	shasum -a 256 selfaction_* >checksums.txt
fi
set -- selfaction_*
echo "wrote $# archives and checksums.txt to $out"
