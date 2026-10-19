#!/bin/sh
# Lab test of the hub's UI reconciler with tests/uitest.lua and the stub renderer.
# Usage: reconcile.sh <lab dir> <hub client ID>. The hub must run with that lab's config.
# Lab only: the hub refuses the real server.
set -u
LAB=${1:?lab dir}; HUB=${2:?hub client id}
REX_BIN=${REX_BIN:-$(/usr/bin/which rex)}
L() { env -u REX_SESSION -u REX_BLOCK "$REX_BIN" -S "unix://$LAB/rex.sock" --autostart=false "$@"; }
describe() { L -C "$HUB" do hub.ui.describe --args '{"plugin":"uitest"}'; }
q() { describe | python3 -c "import json,sys; d=json.load(sys.stdin); $1"; }
fails=0
check() { if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails+1)); fi; }
settle() { /bin/sleep "${1:-2}"; }
windows() { # tab labels of the plugin session, in order
  S=$(q 'print(d["session_id"])')
  L api call session.view "{\"session_id\":\"$S\"}" | python3 -c '
import json,sys; print(",".join(w.get("label","") for w in json.load(sys.stdin)["windows"]))'
}

check "tabs declared, no session before navigation" "$(q 'print(len(d["tabs"]), repr(d["session_id"]))')" "3 ''"

L -C "$HUB" do hub.ui.navigate --args '{"plugin":"uitest","tab":"home"}' >/dev/null; settle 3
check "navigation opens all declared tabs" "$(q 'print(sorted((v["tab"],v["status"],v["attached"]) for v in d["views"]))')" \
  "[('home', 'open', True), ('t1', 'open', True), ('t2', 'open', True)]"
check "tab order" "$(windows)" "Test home,Thread 1,Thread 2"

# Drift: move Thread 2 first. The hub snaps the order back.
S=$(q 'print(d["session_id"])')
W2=$(q 'print([v["window_id"] for v in d["views"] if v["tab"]=="t2"][0])')
W0=$(q 'print([v["window_id"] for v in d["views"] if v["tab"]=="home"][0])')
L api call session.move_window "{\"session_id\":\"$S\",\"window_id\":\"$W2\",\"before_window_id\":\"$W0\"}" >/dev/null; settle
check "order snaps back after drift" "$(windows)" "Test home,Thread 1,Thread 2"

# Native close of Thread 1: the view closes, the tab stays closed.
W1=$(q 'print([v["window_id"] for v in d["views"] if v["tab"]=="t1"][0])')
L api call session.close_window "{\"session_id\":\"$S\",\"window_id\":\"$W1\"}" >/dev/null; settle
check "user close: tab suppressed, not reopened" "$(q 'print([t["suppressed"] for t in d["tabs"] if t["tab"]=="t1"][0], sorted(v["tab"] for v in d["views"]))')" \
  "True ['home', 't2']"
check "user close: order of the rest" "$(windows)" "Test home,Thread 2"

# Navigation reopens it in its declared place.
L -C "$HUB" do hub.ui.navigate --args '{"plugin":"uitest","tab":"t1"}' >/dev/null; settle
check "navigation reopens a closed tab" "$(windows)" "Test home,Thread 1,Thread 2"

# The plugin drops Thread 2: the hub closes that tab.
PLUGIN=$(grep -o '"client_id":"client:[^"]*","event":"plugin_registered"' "$LAB/hub/events.jsonl" | tail -1 | cut -d'"' -f4)
L -C "$PLUGIN" do uitest.set_tabs --args '{"tabs":[{"key":"home","title":"Test home","route":"/"},{"key":"t1","title":"Thread 1","route":"/thread/1"}]}' >/dev/null; settle
check "undeclared tab closes" "$(windows)" "Test home,Thread 1"

# Renderer crash: kill the stub renderer of Thread 1 (a process this test's hub started).
B1=$(q 'print([v["block_id"] for v in d["views"] if v["tab"]=="t1"][0])')
P=$(L api call com.superlogical.terminal.process "{\"session_id\":\"$S\",\"block_id\":\"$B1\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["child"]["pid"])')
ps -o command= -p "$P" | grep -q stubview && kill "$P"; settle
check "renderer crash: new view, same place" "$(windows)" "Test home,Thread 1"
check "renderer crash: replacement attached" "$(q 'print([v["attached"] for v in d["views"] if v["tab"]=="t1"][0], [v["block_id"] for v in d["views"] if v["tab"]=="t1"][0] != "'"$B1"'")')" "True True"

echo "failures: $fails"
exit "$fails"
