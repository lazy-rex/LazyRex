#!/bin/sh
# release-build.sh <version> <out-dir>: the release archives of LazyRex. CI runs it on a tag push
# (.github/workflows/release.yml); the labs and a source install run the same script.
#
# One archive per target: lazyrex-<version>-<os>-<arch>.tar.gz, and SHA256SUMS for all of them.
# An archive unpacks into ~/.lazyrex:
#   bin/lazyrex-hub, bin/lazyrex-view   setup needs both in one directory
#   sdk/lua/*.lua   the plugin SDK: every top-level .lua file of sdk/lua (not examples/ or tests/);
#                   the hub finds sdk/lua above its binary
#   doc/LICENSE   the MIT license (not at the root: the root is the user's ~/.lazyrex)
#
# The builds keep the Go VCS stamp (vcs.revision, vcs.modified): go version -m shows it.
# A version other than "dev" needs a clean checkout. TARGETS (os/arch, separated by
# spaces) limits the targets.
set -eu

if [ $# -ne 2 ]; then
	echo "usage: scripts/release-build.sh <vX.Y.Z | vX.Y.Z-rc.N | dev> <out-dir>" >&2
	exit 2
fi
version=$1
out=$2
# The whole value, before any build or output: dev, vX.Y.Z or vX.Y.Z-rc.N, with digits only. The
# case refuses a newline and any other character first, so the line match of grep sees one line.
bad_version() {
	echo "release-build: version $version is not vX.Y.Z, vX.Y.Z-rc.N or dev" >&2
	exit 2
}
case $version in
*[!a-z0-9.-]* | "") bad_version ;;
esac
printf '%s\n' "$version" | grep -Eqx 'dev|v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?' || bad_version
root=$(cd "$(dirname "$0")/.." && pwd)
if [ "$version" != dev ] && [ -n "$(git -C "$root" status --porcelain)" ]; then
	echo "release-build: the checkout has changes; a release builds from a clean checkout" >&2
	exit 1
fi
targets=${TARGETS:-darwin/arm64 darwin/amd64 linux/arm64 linux/amd64}

# The SDK files: a rule, not a list, so a new SDK file ships with no edit here. want is the file
# list that each archive must hold (tar -t, sorted).
sdk=
for f in "$root"/sdk/lua/*.lua; do
	sdk="$sdk ${f##*/}"
done
want=$({
	printf '%s\n' bin/lazyrex-hub bin/lazyrex-view doc/LICENSE
	for f in $sdk; do
		echo "sdk/lua/$f"
	done
} | LC_ALL=C sort | tr '\n' ' ')

# Each file that the SDK loads by name must be in the archive: a missing one fails the plugin at its
# start. The names: '/x.lua' after an SDK directory, and every literal require('x') (the SDK requires
# only its own modules; boot.lua puts sdk/lua on package.path).
sdk_loads() {
	for f in $sdk; do
		grep -oE "['\"]/[A-Za-z0-9_]+\\.lua['\"]" "$root/sdk/lua/$f" | tr -d "'\"/"
		grep -oE "require[[:space:]]*\\(?[[:space:]]*['\"][A-Za-z0-9_]+['\"]" "$root/sdk/lua/$f" |
			sed "s/^[^'\"]*['\"]\\([A-Za-z0-9_]*\\)['\"]\$/\\1.lua/"
	done | LC_ALL=C sort -u
}

# Everything is built outside the checkout first: an archive in an out-dir inside the checkout
# would be an untracked file, and the next build would be stamped vcs.modified=true.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir "$stage/archives"

for target in $targets; do
	os=${target%/*}
	arch=${target#*/}
	name=lazyrex-$version-$os-$arch
	dir=$stage/$name
	mkdir -p "$dir/bin" "$dir/sdk/lua" "$dir/doc"
	for mod in hub:./cmd/hub:lazyrex-hub view:.:lazyrex-view; do
		src=${mod%%:*}
		rest=${mod#*:}
		pkg=${rest%%:*}
		bin=${rest#*:}
		(cd "$root/$src" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-ldflags "-X main.version=$version" -o "$dir/bin/$bin" "$pkg")
	done
	for f in $sdk; do
		cp "$root/sdk/lua/$f" "$dir/sdk/lua/"
	done
	for f in $(sdk_loads); do
		if [ ! -e "$dir/sdk/lua/$f" ]; then
			echo "release-build: the SDK loads sdk/lua/$f, and $name does not hold it" >&2
			exit 1
		fi
	done
	cp "$root/LICENSE" "$dir/doc/LICENSE"
	# COPYFILE_DISABLE: macOS tar adds AppleDouble (._*) entries for extended attributes otherwise.
	COPYFILE_DISABLE=1 tar -C "$dir" -czf "$stage/archives/$name.tar.gz" bin doc sdk
	files=$(tar -tzf "$stage/archives/$name.tar.gz" | grep -v '/$' | LC_ALL=C sort | tr '\n' ' ')
	if [ "$files" != "$want" ]; then
		echo "release-build: $name.tar.gz holds $files" >&2
		exit 1
	fi
	echo "release-build: $name.tar.gz"
done

mkdir -p "$out"
rm -f "$out"/lazyrex-"$version"-*.tar.gz "$out/SHA256SUMS"
mv "$stage"/archives/* "$out/"
cd "$out"
if command -v sha256sum >/dev/null; then
	sha256sum lazyrex-"$version"-*.tar.gz >SHA256SUMS
	sha256sum -c SHA256SUMS >/dev/null
else
	shasum -a 256 lazyrex-"$version"-*.tar.gz >SHA256SUMS
	shasum -a 256 -c SHA256SUMS >/dev/null
fi
echo "release-build: SHA256SUMS"
