#!/bin/sh
# check-action-pins.sh [DIR]: every "uses:" in the workflows of DIR (default: .github/workflows of
# this repository) names an action at a full commit SHA, with its version tag as a comment:
#   uses: owner/repo@<40 hex digits> # vX.Y.Z
# A tag or a branch can move; a commit cannot. Every line that names a uses key in any form (a
# flow mapping such as "- {uses: ...}", a quoted key, a value on the next line) must have exactly
# that block form: another form is refused, not parsed. Comment lines are skipped. It prints each
# bad line and exits 1. CI runs it before the build (.github/workflows/release.yml).
set -u
here=$(cd "$(dirname "$0")" && pwd)
dir=${1:-$here/../.github/workflows}
bad=0
found=0
for f in "$dir"/*.yml "$dir"/*.yaml; do
	[ -f "$f" ] || continue
	# Each line that names a uses key, in any form; a comment line is not a key.
	lines=$(grep -nE '(^|[^A-Za-z0-9_-])["'"'"']?uses["'"'"']?[[:space:]]*:' "$f" | grep -vE '^[0-9]+:[[:space:]]*#')
	[ -n "$lines" ] || continue
	while IFS= read -r l; do
		found=$((found + 1))
		if ! printf '%s\n' "$l" | grep -qE '^[0-9]+:[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]+[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+\.[0-9]+\.[0-9]+[[:space:]]*$'; then
			echo "unpinned action: $f:$l"
			bad=1
		fi
	done <<EOF
$lines
EOF
done
if [ "$found" -eq 0 ]; then
	echo "no actions found in $dir"
	exit 1
fi
exit $bad
