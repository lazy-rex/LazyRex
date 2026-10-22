#!/bin/sh
# Probe (rex-protocol.md, "A create can complete after its client dies"): does Rex complete a
# session.new_block after the client that sent it is gone? The B8 attempt proof depends on the
# answer: an absent block never proves that a create did not happen.
#
# Lab only. Run: lab/probes/late-create.sh unix:///tmp/rexlab-<team>-<you>/rex.sock
# It makes its own session, kills only the rex CLI processes that it started, and destroys the
# session at the end.
#
# For each delay D (ms): a rex CLI sends session.new_block with a unique label, and the probe
# kills that CLI D ms later (SIGKILL), before or after the reply. A second client then lists the
# session's blocks every 20 ms for 3 s. The probe prints, per trial: the delay, whether the CLI had
# printed the reply, and at which poll (each about 20 ms plus one session.list_blocks) after the kill the
# labeled block was first seen, or "never".
set -eu
sock=${1:?usage: late-create.sh <lab socket URL>}
r() { rex -S "$sock" --autostart=false "$@"; }

sess=$(r api call session.create '{"label":"lateprobe","initial_windows":[{"layout":{"block":{"flavor":"com.superlogical.terminal.shell","label":"base","options":{"command":["sleep","600"],"login":false}}}}]}' |
	sed -n 's/.*"session_id":"\([^"]*\)".*/\1/p')
[ -n "$sess" ] || { echo "no session"; exit 1; }
trap 'r api call session.destroy "{\"session_id\":\"$sess\"}" >/dev/null 2>&1 || true' EXIT
echo "$(rex version 2>/dev/null | head -1)"
echo "session $sess"
out=$(mktemp -d)
for d in 0 1 2 5 10 20 50 200; do
	for n in 1 2 3; do
		label="late-$d-$n"
		# rex directly, not the function r: $! must be the rex CLI itself, so the kill reaches it.
		rex -S "$sock" --autostart=false api call session.new_block "{\"session_id\":\"$sess\",\"flavor\":\"com.superlogical.terminal.shell\",\"label\":\"$label\",\"options\":{\"command\":[\"sleep\",\"600\"],\"login\":false}}" >"$out/$label" 2>&1 &
		pid=$!
		sleep "$(printf "0.%03d" "$d")"
		kill -9 "$pid" 2>/dev/null || true
		status=0
		wait "$pid" 2>/dev/null || status=$?
		killed=no
		[ "$status" -eq 137 ] && killed=yes
		replied=no
		grep -q block_id "$out/$label" && replied=yes
		seen=never
		i=0
		while [ $i -lt 150 ]; do
			if r api call session.list_blocks "{\"session_id\":\"$sess\"}" 2>/dev/null | grep -q "\"label\":\"$label\""; then
				seen="poll $i"
				break
			fi
			sleep 0.02
			i=$((i + 1))
		done
		echo "delay=${d}ms trial=$n killed=$killed replied=$replied seen_after_kill=${seen}"
	done
done
rm -rf "$out"
