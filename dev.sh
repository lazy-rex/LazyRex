#!/bin/sh
# Hot reload for spike 3. Runs the hub, watches the sources, and reloads what changed:
#   plugins/<dir>/*.lua -> hub.admin.stop + hub.admin.start <manifest name> (views render again)
#   view/*.go           -> build, install the renderer atomically, hub.ui.reload for each plugin
#   hub/**/*.go         -> build, restart the hub (renderers and plugins reattach)
# Usage: dev.sh <hub config>. The config names the server, state dir, view path and plugins.
# Lab by default: without DEV_REAL=1 the hub refuses the real server.
# DEV_REAL=1 passes -real-server (approved by the maintainer, 2026-10-16, for the official hub only).
# The hub then checks that the config names the real server and refuses lab faults.
# It polls once a second (no dependency on fswatch). It stops only the hub process it started.
set -u
CFG=${1:?hub config}
DIR=$(cd "$(dirname "$0")" && pwd)
REX_BIN=${REX_BIN:-$(/usr/bin/which rex)}
# json_str PATH [FILE] prints each string value at PATH in the JSON of FILE (else stdin), one a line.
# A PATH is dot-separated object keys, with * for any array item: "server", "plugins.*.plugin".
# Plain awk (no python3, no jq): it reads strings, escapes and nesting, not numbers or literals.
json_str() {
  awk -v want="$1" '
    { s = s $0 "\n" }
    END {
      n = length(s); d = 0; i = 1
      while (i <= n) {
        c = substr(s, i, 1)
        if (c == "{" || c == "[") { d++; kind[d] = c; key[d] = ""; i++; continue }
        if (c == "}" || c == "]") { d--; i++; continue }
        if (c != "\"") { i++; continue }
        v = ""; i++
        while (i <= n && (c = substr(s, i, 1)) != "\"") {
          if (c == "\\") {
            i++; c = substr(s, i, 1)
            if (c == "n") c = "\n"; else if (c == "t") c = "\t"
          }
          v = v c; i++
        }
        i++
        j = i; while (j <= n && index(" \t\r\n", substr(s, j, 1))) j++
        if (substr(s, j, 1) == ":") { key[d] = v; i = j + 1; continue }
        path = ""
        for (k = 1; k <= d; k++) path = path (k > 1 ? "." : "") (kind[k] == "[" ? "*" : key[k])
        if (path == want) print v
      }
    }' ${2:+"$2"}
}
field() { json_str "$1" "$CFG"; }
SERVER=$(field server); STATE=$(field state); VIEW=$(field view)
DEV=$STATE/dev; mkdir -p "$DEV"
L() { env -u REX_SESSION -u REX_BLOCK "$REX_BIN" -S "$SERVER" --autostart=false "$@"; }
log() { echo "dev: $(date +%H:%M:%S) $*"; }

hub_id() { grep -o '"client_id":"client:[^"]*","event":"hub_started"' "$STATE/events.jsonl" | tail -1 | cut -d'"' -f4; }
hub_do() { L -C "$(hub_id)" do "$@"; }
# The manifests own the plugin names (docs/plugin-sources.md). plugin_names asks the running hub;
# manifest_name reads the manifest of a plugin directory under plugins/.
plugin_names() { hub_do hub.plugins --json 2>/dev/null | json_str plugins.*.plugin; }
manifest_name() { json_str name "$1/lazyrex.json"; }

HUB_PID=
start_hub() {
  (cd "$DIR/hub" && go build -o "$DEV/hub.new" ./cmd/hub) || { log "hub build failed; old hub keeps running"; return 1; }
  if [ -n "$HUB_PID" ] && kill -0 "$HUB_PID" 2>/dev/null; then kill "$HUB_PID"; wait "$HUB_PID" 2>/dev/null; fi
  mv "$DEV/hub.new" "$DEV/hub"
  REAL=; [ "${DEV_REAL:-}" = 1 ] && REAL=-real-server
  env -u REX_SESSION -u REX_BLOCK REX_SERVER="$SERVER" "$DEV/hub" -config "$CFG" -sdk "$DIR/sdk/lua" $REAL >> "$DEV/hub.log" 2>&1 &
  HUB_PID=$!
  # Wait until the new hub reports itself, so later commands in this pass reach it.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    /bin/sleep 1
    grep '"event":"hub_started"' "$STATE/events.jsonl" 2>/dev/null | tail -1 | grep -q "\"pid\":$HUB_PID[,}]" && break
  done
  log "hub started, pid $HUB_PID, $(hub_id) (log $DEV/hub.log)"
}
build_view() {
  (cd "$DIR/view" && go build -o "$VIEW.new" .) || { log "renderer build failed"; return 1; }
  mv "$VIEW.new" "$VIEW" # rename is atomic: running renderers keep the old file until they exec
}
install_view() {
  build_view || return 1
  for p in $(plugin_names); do log "reload $p: $(hub_do hub.ui.reload --args "{\"plugin\":\"$p\"}" 2>&1)"; done
}
restart_plugin() {
  log "restart plugin $1"
  hub_do hub.admin.stop --args "{\"plugin\":\"$1\"}" >/dev/null && hub_do hub.admin.start --args "{\"plugin\":\"$1\"}" >/dev/null
}
trap 'if [ -n "$HUB_PID" ]; then kill "$HUB_PID" 2>/dev/null; fi; exit 0' INT TERM HUP

MARK=$DEV/mark; touch "$MARK"
build_view && start_hub || exit 1
while :; do
  /bin/sleep 1
  NEXT=$DEV/mark.next; touch "$NEXT"
  changed=$(find "$DIR/plugins" "$DIR/view" "$DIR/hub" -newer "$MARK" -type f \( -name '*.lua' -o -name '*.go' -o -name go.mod \) 2>/dev/null)
  mv "$NEXT" "$MARK"
  [ -z "$changed" ] && continue
  case $changed in *"$DIR/hub/"*) start_hub ;; esac
  case $changed in *"$DIR/view/"*) install_view ;; esac
  for d in "$DIR"/plugins/*/; do
    case $changed in *"$d"*) restart_plugin "$(manifest_name "$d")" ;; esac
  done
done
