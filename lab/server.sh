#!/bin/sh
# Start an ISOLATED Rex server for experiments. Never experiment on the user's real Rex server.
# Usage: spikes/lab/server.sh [lab_dir]        (default lab_dir: /tmp/rexlab)
# Then:  export REX_SERVER=unix://$LAB_DIR/rex.sock
#        rex -S "$REX_SERVER" --autostart=false ls
# Optional: put executables in $LAB_DIR/blocks to load them as block plugins (discovered only at start).
# Optional: LAB_CONFIG=<path to init.lua> loads a config file (default: none).
# Optional: LAB_EXTRA_ARGS adds server options, for example a TCP listener for the app:
#   LAB_EXTRA_ARGS='--extra-listen http://127.0.0.1:47900 --allow-anonymous'   (lab only)
set -eu
LAB_DIR="${1:-${LAB_DIR:-/tmp/rexlab}}"
mkdir -p "$LAB_DIR/state" "$LAB_DIR/blocks"
rm -f "$LAB_DIR/rex.sock"
echo "lab server: unix://$LAB_DIR/rex.sock" >&2
# Rex does not set REX_SERVER for lab panes: they inherit it from this process. A server started
# from inside a Rex pane would pass the REAL server's address, and `rex` in a lab pane would reach
# the real server. Without REX_SERVER, `rex` falls back to the default (real) server too.
# So set it to the lab socket. (Observed 2026-10-15, Rex 0.0.1792088193-g7a93ec.)
exec env -u REX_SESSION -u REX_BLOCK -u REX_CLIENT REX_SERVER="unix://$LAB_DIR/rex.sock" rex -v server run \
  --listen "unix://$LAB_DIR/rex.sock" \
  --state-dir "$LAB_DIR/state" \
  --block-dir "$LAB_DIR/blocks" \
  --block-plugin-reload \
  --config "${LAB_CONFIG:-/dev/null}" \
  ${LAB_EXTRA_ARGS:-}
