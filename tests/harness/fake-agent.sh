#!/bin/sh
# A fake agent CLI for tests/agent_resume.lua: $1 kind, $2 conversation id, $3 the source of its
# identity report, $4 when it reports: "start" (as Claude does), "prompt" (after its first input
# line, as Codex does), "never", or "foreign" (a child process in the foreground reports the program
# status and reads the input: the status owner is not this pane's program). Optional $5: seconds
# between the prompt and the report. Optional $6, how it stops (tests/agent_restart.lua): "int" (one Ctrl-C,
# as Codex), "int2" (two Ctrl-C within a second, as Claude), "term" (it ignores Ctrl-C), "kill" (it
# ignores Ctrl-C and SIGTERM), "fgchild" (after its report a child in its own foreground process
# group logs each Ctrl-C). Every input line and each SIGTERM is appended to $PWD/fake-<conversation>.log.
# After the first prompt, the input line "again" reports once more from this program, and "exec"
# executes /bin/dash in this pid (it reports no more).
# Like both CLIs (S1, 2026-10-22) it reports program status idle before any report. The report runs
# in a child process of this pane's program, as a hook does. An input line "exit" ends it.
kind=$1 conv=$2 source=$3 when=$4 delay=${5:-0} stop=${6:-int}
status() { printf '\033]7501;state=%s\033\\' "$1"; }
report() {
	rex -S "$REX_SERVER" --autostart=false -C lazyrex-hub do hub.identity.report \
		--args "{\"kind\":\"$kind\",\"conversation_id\":\"$conv\",\"source\":\"$source\",\"block\":\"$REX_BLOCK\"}" >/dev/null 2>&1
}
log="$PWD/fake-$conv.log"
case $stop in
int2) trap 'n=$(date +%s); [ -n "$last" ] && [ $((n - last)) -le 1 ] && exit 0; last=$n' INT ;;
term) trap '' INT ;;
kill) trap '' INT TERM ;;
esac
[ "$stop" = kill ] || trap 'echo "signal: TERM" >> "$log"; exit 0' TERM
# rd reads a line; a read that a trapped signal interrupts is read again.
rd() { while :; do IFS= read -r line && return 0; [ $? -gt 128 ] || return 1; done; }
if [ "$when" = foreign ]; then
	set -m # job control: the child runs in its own foreground process group, so Rex names it the owner
	sh -c 'printf "\033]7501;state=idle\033\\\\"; while IFS= read -r l; do echo "input: $l" >> "$1"; done' sh "$log"
	exit 0
fi
status idle
[ "$when" = start ] && report
if [ "$stop" = fgchild ]; then
	set -m
	sh -c 'trap "echo \"signal: INT\" >> \"$1\"" INT; while :; do sleep 1; done' sh "$log"
	exit 0
fi
rd || exit 0
echo "prompt: $line" >> "$log"
[ "$line" = exit ] && exit 0
if [ "$when" = prompt ]; then
	sleep "$delay"
	report
fi
status done
while rd; do
	[ "$line" = exit ] && exit 0
	[ "$line" = again ] && report
	[ "$line" = exec ] && exec /bin/dash "$0" "$kind" "$conv" "$source" never 0 "$stop"
	echo "input: $line" >> "$log"
done
