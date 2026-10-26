#!/bin/sh
# install.sh: install a LazyRex release into ~/.lazyrex, then run setup. Each GitHub release has a
# copy of this file (.github/workflows/release.yml), so the README install is one line:
#
#   curl -fsSL https://github.com/lazy-rex/LazyRex/releases/latest/download/install.sh | sh
#
# The script downloads the archive of this system and SHA256SUMS, checks the archive, and unpacks
# it into ~/.lazyrex. A failed download, a missing checksum line or a failed check stops it before
# it unpacks anything. It does not stop or restart a running hub.
#
# Environment:
#   LAZYREX_VERSION   the release to install, for example v0.5.0 (default: the latest release)
#   LAZYREX_SETUP=0   do not run setup at the end
#   LAZYREX_BASE      the releases URL (default: the LazyRex GitHub releases; tests use file://)
set -eu

base=${LAZYREX_BASE:-https://github.com/lazy-rex/LazyRex/releases}
dest=$HOME/.lazyrex

say() { printf 'lazyrex: %s\n' "$*"; }
die() {
	printf 'lazyrex: %s\n' "$*" >&2
	exit 1
}

command -v curl > /dev/null || die "curl is not installed"
command -v tar > /dev/null || die "tar is not installed"
if command -v shasum > /dev/null; then
	sha() { shasum -a 256 -c check; }
elif command -v sha256sum > /dev/null; then
	sha() { sha256sum -c check; }
else
	die "no shasum or sha256sum to check the download"
fi

case $(uname -s) in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "LazyRex runs on macOS and Linux, not $(uname -s)" ;;
esac
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "LazyRex runs on arm64 and amd64, not $(uname -m)" ;;
esac

# The latest release: GitHub redirects releases/latest to releases/tag/<version>.
version=${LAZYREX_VERSION:-}
if [ -z "$version" ]; then
	version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/latest") || die "no LazyRex release found"
	version=${version##*/}
fi
case $version in
*[!a-z0-9.-]* | "") die "the release $version is not a LazyRex version" ;;
v[0-9]*) ;;
*) die "the release $version is not a LazyRex version" ;;
esac

archive=lazyrex-$version-$os-$arch.tar.gz
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
say "downloading $archive"
curl -fsSLO "$base/download/$version/$archive" || die "cannot download $base/download/$version/$archive"
curl -fsSLO "$base/download/$version/SHA256SUMS" || die "cannot download SHA256SUMS of $version"
grep " $archive\$" SHA256SUMS > check || die "SHA256SUMS of $version has no line for $archive"
sha > /dev/null || die "the checksum of $archive does not match SHA256SUMS"

mkdir -p "$dest"
tar -xzf "$archive" -C "$dest"
installed=$("$dest/bin/lazyrex-hub" version) || die "the installed lazyrex-hub does not run: $dest/bin/lazyrex-hub version failed"
[ -n "$installed" ] || die "the installed lazyrex-hub printed no version"
say "installed $installed in $dest"

command -v git > /dev/null || say "warning: git is not installed; setup and the hub need it"
command -v rex > /dev/null || say "warning: rex is not on PATH; LazyRex needs Rex with its server running"

# Setup asks before it writes each file, so it needs a terminal. With "curl ... | sh", the script
# reads from the pipe, so setup reads from /dev/tty.
if [ "${LAZYREX_SETUP:-1}" = 0 ]; then
	say "next: $dest/bin/lazyrex-hub setup"
elif (: < /dev/tty) 2> /dev/null; then
	cd "$HOME"
	"$dest/bin/lazyrex-hub" setup < /dev/tty || die "LazyRex is installed, but setup did not finish; run: $dest/bin/lazyrex-hub setup"
else
	say "no terminal for setup; run: $dest/bin/lazyrex-hub setup"
fi
