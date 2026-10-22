#!/bin/sh
# A fake agent CLI for tests/agent_resume.lua: $1 kind, $2 conversation id, $3 the source of its
# identity report, $4 when it reports: "start" (as Claude does), "prompt" (after its first input
# line, as Codex does), "never", or "foreign" (a child process in the foreground reports the program
# status and reads the input: the status owner is not this pane's program). Optional $5: seconds
# between the prompt and the report. Every input line is appended to $PWD/fake-<conversation>.log.
# Like both CLIs (S1, 2026-10-22) it reports program status idle before any report. The report runs
# in a child process of this pane's program, as a hook does. An input line "exit" ends it.
kind=$1 conv=$2 source=$3 when=$4 delay=${5:-0}
status() { printf '\033]7501;state=%s\033\\' "$1"; }
report() {
	rex -S "$REX_SERVER" --autostart=false -C lazyrex-hub do hub.identity.report \
		--args "{\"kind\":\"$kind\",\"conversation_id\":\"$conv\",\"source\":\"$source\",\"block\":\"$REX_BLOCK\"}" >/dev/null 2>&1
}
log="$PWD/fake-$conv.log"
if [ "$when" = foreign ]; then
	set -m # job control: the child runs in its own foreground process group, so Rex names it the owner
	sh -c 'printf "\033]7501;state=idle\033\\\\"; while IFS= read -r l; do echo "input: $l" >> "$1"; done' sh "$log"
	exit 0
fi
status idle
[ "$when" = start ] && report
IFS= read -r line || exit 0
echo "prompt: $line" >> "$log"
[ "$line" = exit ] && exit 0
if [ "$when" = prompt ]; then
	sleep "$delay"
	report
fi
status done
while IFS= read -r line; do
	[ "$line" = exit ] && exit 0
	echo "input: $line" >> "$log"
done
