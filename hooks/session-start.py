#!/usr/bin/env python3
"""Report a conversation from an agent hook. Do nothing when no hub is available."""

import json
import os
import shutil
import signal
import subprocess
import time
import sys


def main():
    # Never let Rex choose the user's default server for a hook outside a Rex pane.
    if not all(os.environ.get(key) for key in ("REX_SERVER", "REX_SESSION", "REX_BLOCK")):
        return
    if len(sys.argv) != 2 or sys.argv[1] not in ("codex", "claude"):
        return
    try:
        event = json.load(sys.stdin)
    except (ValueError, OSError):
        return
    if not isinstance(event, dict):
        return
    hook = event.get("hook_event_name")
    if hook == "SessionStart":
        source = event.get("source")
    elif hook == "UserPromptSubmit" and sys.argv[1] == "claude":
        source = "prompt"
    else:
        return
    conversation = event.get("session_id")
    if not isinstance(conversation, str) or not conversation or "\0" in conversation:
        return
    if source not in ("startup", "resume", "clear", "compact", "fork", "prompt"):
        return
    executable = shutil.which(os.environ.get("REX_BIN", "rex"))
    if not executable:
        return
    # Rex keeps lost clients during reconnection. Select one connected hub explicitly.
    base = [executable, "-S", os.environ["REX_SERVER"], "--autostart=false", "--timeout", "1s"]
    try:
        listing = subprocess.run(base + ["client", "ls", "--json"], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                 timeout=1.5, check=True, text=True)
        clients = json.loads(listing.stdout).get("clients", [])
        matches = [c["client_id"] for c in clients if c.get("info", {}).get("label") == "lazyrex-hub"
                   and c.get("connection_state") == "connected"]
    except (OSError, ValueError, subprocess.SubprocessError):
        return
    if len(matches) != 1:
        return
    args = {"kind": sys.argv[1], "conversation_id": conversation, "source": source,
            "block": os.environ["REX_BLOCK"]}
    command = [
        executable, "-S", os.environ["REX_SERVER"], "--autostart=false",
        "--timeout", "2s", "-C", matches[0], "do", "hub.identity.report",
        "--args", json.dumps(args),
    ]
    # Exec preserves the reporter's process start time across the Rex call.
    with open(os.devnull, "r+b") as quiet:
        for fd in (0, 1, 2):
            os.dup2(quiet.fileno(), fd)
    os.execv(executable, command)


def run():
    # Start the reporter before it reads input. The hub orders this process, not its parent.
    try:
        child = os.fork()
    except OSError:
        return
    if child == 0:
        try:
            main()
        except Exception:
            pass
        os._exit(0)
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        if os.waitpid(child, os.WNOHANG)[0]:
            return
        time.sleep(0.02)
    try:
        os.kill(child, signal.SIGKILL)
    except ProcessLookupError:
        pass
    os.waitpid(child, 0)


if __name__ == "__main__":
    run()
