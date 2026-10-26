#!/bin/sh
# release-build-test.sh: the version check of release-build.sh. A stub go records each call and
# exits 77. An invalid version must exit 2 before any build and leave the out dir unchanged; a valid
# one must pass the check (it then reaches the stub, or the clean-checkout check). CI runs this
# before the build (.github/workflows/release.yml).
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/stub" "$tmp/out"
printf '#!/bin/sh\necho "$*" >>"%s/go-calls"\nexit 77\n' "$tmp" >"$tmp/stub/go"
chmod +x "$tmp/stub/go"
echo keep >"$tmp/out/sentinel"
fails=0

run() {
	rm -f "$tmp/go-calls"
	PATH=$tmp/stub:$PATH "$here/release-build.sh" "$1" "$tmp/out" >/dev/null 2>&1
}

nl='
'
for v in v1 v1.2 v1.2.3oops v1.2.3-rc.no v1x.2y.3z v1.2.3-rc.1x v1.2.3-beta.1 V1.2.3 1.2.3 dev2 \
	"" " v1.2.3" "v1.2.3${nl}v1.2.3" "v1.2.3/x" "../v1.2.3"; do
	run "$v"
	code=$?
	if [ $code -ne 2 ] || [ -e "$tmp/go-calls" ] ||
		[ "$(ls -A "$tmp/out")" != sentinel ] || [ "$(cat "$tmp/out/sentinel")" != keep ]; then
		echo "FAIL: version '$v': exit $code, go called: $([ -e "$tmp/go-calls" ] && echo yes || echo no), out: $(ls -A "$tmp/out" | tr '\n' ' ')"
		fails=$((fails + 1))
	fi
done

for v in dev v0.1.0 v10.20.30 v1.2.3-rc.4; do
	run "$v"
	code=$?
	if [ $code -eq 2 ]; then
		echo "FAIL: valid version '$v' was refused"
		fails=$((fails + 1))
	fi
done

# check-action-pins.sh: a full SHA with its version comment passes, also beside a comment that
# names uses:. A tag, a short SHA, a SHA without the comment, a branch, a directory without
# actions, and a uses key in another form (a flow mapping beside a pinned step, a quoted key, the
# value on the next line) fail.
pin=actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
pins() {
	rm -rf "$tmp/wf"
	mkdir "$tmp/wf"
	printf 'jobs:\n%s\n' "$1" >"$tmp/wf/a.yml"
	"$here/check-action-pins.sh" "$tmp/wf" >/dev/null
}
pins "      # uses: a comment${nl}      - uses: $pin # v7.0.1" || {
	echo "FAIL: a pinned action was refused"
	fails=$((fails + 1))
}
for l in "      - uses: actions/checkout@v7" "      - uses: actions/checkout@3d3c42e5 # v7.0.1" \
	"      - uses: $pin" "    uses: org/repo/.github/workflows/x.yml@main" "name: x" \
	"      - uses: $pin # v7.0.1${nl}      - {uses: actions/upload-artifact@v7}" \
	"      - \"uses\": $pin # v7.0.1" "      - uses: $pin # v7.0.1${nl}      - \"uses\": actions/x@v1" \
	"      - uses:${nl}          $pin # v7.0.1"; do
	if pins "$l"; then
		echo "FAIL: check-action-pins.sh passed '$l'"
		fails=$((fails + 1))
	fi
done

# The SDK rule and the closure guard, in a fake checkout: a stub go writes each -o file. Every
# top-level sdk/lua/*.lua ships (not tests/); a name that the SDK loads and the archive lacks
# stops the build.
fake() {
	rm -rf "$tmp/root" "$tmp/fout"
	mkdir -p "$tmp/root/scripts" "$tmp/root/hub" "$tmp/root/view" "$tmp/root/sdk/lua/tests" "$tmp/fstub"
	cp "$here/release-build.sh" "$tmp/root/scripts/"
	echo license >"$tmp/root/LICENSE"
	echo "dofile(SDK_DIR .. '/testing.lua')" >"$tmp/root/sdk/lua/boot.lua"
	printf '%s\n' "$1" >"$tmp/root/sdk/lua/lazyrex.lua"
	echo "-- testing" >"$tmp/root/sdk/lua/testing.lua"
	echo "-- a test" >"$tmp/root/sdk/lua/tests/t.lua"
	printf '#!/bin/sh\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && : >"$2"; shift; done\n' >"$tmp/fstub/go"
	chmod +x "$tmp/fstub/go"
	TARGETS=linux/amd64 PATH=$tmp/fstub:$PATH "$tmp/root/scripts/release-build.sh" dev "$tmp/fout" >"$tmp/fake.log" 2>&1
}
fake "local x = loadfile(SDK_DIR .. \"/testing.lua\") require('boot') require \"testing\""
code=$?
got=$(tar -tzf "$tmp/fout/lazyrex-dev-linux-amd64.tar.gz" 2>/dev/null | grep -v '/$' | LC_ALL=C sort | tr '\n' ' ')
if [ $code -ne 0 ] || [ "$got" != "bin/lazyrex-hub bin/lazyrex-view doc/LICENSE sdk/lua/boot.lua sdk/lua/lazyrex.lua sdk/lua/testing.lua " ]; then
	echo "FAIL: the SDK rule: exit $code, archive: $got"
	cat "$tmp/fake.log"
	fails=$((fails + 1))
fi
for load in "loadfile(SDK_DIR .. '/missing.lua')" "require('missing')" 'require "missing"' "require  ('missing')" "require	( 'missing' )"; do
	fake "$load"
	code=$?
	if [ $code -ne 1 ] || ! grep -q 'the SDK loads sdk/lua/missing.lua, and lazyrex-dev-linux-amd64 does not hold it' "$tmp/fake.log"; then
		echo "FAIL: the closure guard, $load: exit $code"
		cat "$tmp/fake.log"
		fails=$((fails + 1))
	fi
done

if [ $fails -ne 0 ]; then
	echo "release-build-test: $fails failed"
	exit 1
fi
echo "release-build-test: ok"
