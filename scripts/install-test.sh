#!/bin/sh
# install-test.sh: install.sh against a fake release in a file:// directory, with a temporary HOME.
# A good release installs and prints its version. A bad checksum, a missing checksum line, a
# missing archive and an invalid version stop the script before it unpacks anything. Setup is off
# (LAZYREX_SETUP=0): it needs a terminal. CI runs this before the build (.github/workflows/release.yml).
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0
fail() {
	echo "FAIL: $*"
	fails=$((fails + 1))
}

case $(uname -s) in Darwin) os=darwin ;; *) os=linux ;; esac
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; *) arch=arm64 ;; esac
v=v9.8.7
archive=lazyrex-$v-$os-$arch.tar.gz
sum() { if command -v shasum > /dev/null; then shasum -a 256 "$@"; else sha256sum "$@"; fi; }

# The fake release: bin/lazyrex-hub prints a version, as the real one does.
mkdir -p "$tmp/pkg/bin" "$tmp/pkg/sdk/lua" "$tmp/pkg/doc" "$tmp/rel/download/$v"
printf '#!/bin/sh\necho "%s (commit fake)"\n' "$v" > "$tmp/pkg/bin/lazyrex-hub"
chmod +x "$tmp/pkg/bin/lazyrex-hub"
echo view > "$tmp/pkg/bin/lazyrex-view"
echo sdk > "$tmp/pkg/sdk/lua/lazyrex.lua"
echo license > "$tmp/pkg/doc/LICENSE"
(cd "$tmp/pkg" && tar -czf "$tmp/rel/download/$v/$archive" bin sdk doc)
(cd "$tmp/rel/download/$v" && sum "$archive" > SHA256SUMS)

# run <home> [VAR=value ...]: install.sh with that HOME; the output goes to $tmp/out.
run() {
	h=$1
	shift
	mkdir -p "$h"
	env HOME="$h" LAZYREX_BASE="file://$tmp/rel" LAZYREX_VERSION=$v LAZYREX_SETUP=0 "$@" \
		sh "$here/install.sh" > "$tmp/out" 2>&1
}

run "$tmp/h1"
code=$?
[ $code -eq 0 ] || fail "good release: exit $code: $(cat "$tmp/out")"
for f in bin/lazyrex-hub bin/lazyrex-view sdk/lua/lazyrex.lua doc/LICENSE; do
	[ -f "$tmp/h1/.lazyrex/$f" ] || fail "good release: no $f"
done
grep -q "installed $v (commit fake) in $tmp/h1/.lazyrex" "$tmp/out" || fail "good release: no version line: $(cat "$tmp/out")"
grep -q "next: $tmp/h1/.lazyrex/bin/lazyrex-hub setup" "$tmp/out" || fail "LAZYREX_SETUP=0: no next step: $(cat "$tmp/out")"

# nothing <case> <home>: the script failed and unpacked nothing.
nothing() {
	code=$?
	[ $code -ne 0 ] || fail "$1: exit 0"
	[ ! -e "$2/.lazyrex" ] || fail "$1: it unpacked into $2/.lazyrex"
}

cp "$tmp/rel/download/$v/SHA256SUMS" "$tmp/good"
# A digest that differs from the real one: all zeros, or all ones if the real one is all zeros.
bad=0000000000000000000000000000000000000000000000000000000000000000
[ "$(cut -d' ' -f1 "$tmp/good")" != "$bad" ] || bad=1111111111111111111111111111111111111111111111111111111111111111
echo "$bad  $archive" > "$tmp/rel/download/$v/SHA256SUMS"
run "$tmp/h2"
nothing "bad checksum" "$tmp/h2"
grep -q "does not match" "$tmp/out" || fail "bad checksum: wrong message: $(cat "$tmp/out")"

echo "0000  lazyrex-$v-other-arch.tar.gz" > "$tmp/rel/download/$v/SHA256SUMS"
run "$tmp/h3"
nothing "missing checksum line" "$tmp/h3"
grep -q "has no line for $archive" "$tmp/out" || fail "missing checksum line: wrong message: $(cat "$tmp/out")"
cp "$tmp/good" "$tmp/rel/download/$v/SHA256SUMS"

run "$tmp/h4" LAZYREX_VERSION=v9.9.9
nothing "missing archive" "$tmp/h4"
grep -q "cannot download" "$tmp/out" || fail "missing archive: wrong message: $(cat "$tmp/out")"

for bad in dev 1.2.3 "v1.2.3/x" "../v1.2.3" "v1 2"; do
	run "$tmp/h5" LAZYREX_VERSION="$bad"
	nothing "version '$bad'" "$tmp/h5"
	grep -q "is not a LazyRex version" "$tmp/out" || fail "version '$bad': wrong message: $(cat "$tmp/out")"
done

# A hub that does not run (it exits 23): the script fails and does not report an install.
b=v9.8.6
barchive=lazyrex-$b-$os-$arch.tar.gz
mkdir -p "$tmp/rel/download/$b"
printf '#!/bin/sh\nexit 23\n' > "$tmp/pkg/bin/lazyrex-hub"
(cd "$tmp/pkg" && tar -czf "$tmp/rel/download/$b/$barchive" bin sdk doc)
(cd "$tmp/rel/download/$b" && sum "$barchive" > SHA256SUMS)
run "$tmp/h6" LAZYREX_VERSION=$b
code=$?
[ $code -ne 0 ] || fail "broken hub: exit 0: $(cat "$tmp/out")"
grep -q "^lazyrex: installed " "$tmp/out" && fail "broken hub: reported an install: $(cat "$tmp/out")"
grep -q "does not run" "$tmp/out" || fail "broken hub: wrong message: $(cat "$tmp/out")"

if [ $fails -ne 0 ]; then
	echo "install-test: $fails failed"
	exit 1
fi
echo "install-test: ok"
