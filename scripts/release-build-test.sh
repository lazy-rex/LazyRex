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

# check-action-pins.sh: a full SHA with its version comment passes; a tag, a short SHA, a SHA
# without the comment, a branch, and a directory without actions fail.
pin=actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
pins() {
	rm -rf "$tmp/wf"
	mkdir "$tmp/wf"
	printf 'jobs:\n%s\n' "$1" >"$tmp/wf/a.yml"
	"$here/check-action-pins.sh" "$tmp/wf" >/dev/null
}
pins "      - uses: $pin # v7.0.1" || {
	echo "FAIL: a pinned action was refused"
	fails=$((fails + 1))
}
for l in "      - uses: actions/checkout@v7" "      - uses: actions/checkout@3d3c42e5 # v7.0.1" \
	"      - uses: $pin" "    uses: org/repo/.github/workflows/x.yml@main" "name: x"; do
	if pins "$l"; then
		echo "FAIL: check-action-pins.sh passed '$l'"
		fails=$((fails + 1))
	fi
done

if [ $fails -ne 0 ]; then
	echo "release-build-test: $fails failed"
	exit 1
fi
echo "release-build-test: ok"
