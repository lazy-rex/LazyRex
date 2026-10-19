# Conversation hook

`session-start.py` reports Claude Code and Codex conversations through the Rex CLI.
It needs Python 3 with `os.fork` support and `rex` on `PATH`. `REX_BIN` can select an absolute Rex executable.

The script reads the agent's hook JSON from standard input. It sends `kind`, `conversation_id`, `source`, and a `block` hint to `hub.identity.report`.
Rex attests the caller PID. The hub must verify the pane and the current process generation before accepting the report.
The script does not trust `REX_SESSION` or `REX_BLOCK` as proof.

The hook does nothing outside a Rex pane. It does not start Rex or retry a missing hub.
It selects exactly one connected client labeled `lazyrex-hub`.
It ignores lost clients that Rex keeps after a hub restart.
It produces no model context. The command has a three-second deadline.

## Installation

Copy `session-start.py` to `~/.lazyrex/bin/session-start.py` and make it executable.
Add one entry to the existing `SessionStart` list. Preserve the other hooks.
Use the absolute installed path in each command.

Codex (`~/.codex/hooks.json`):

```json
{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "type": "command",
        "command": "python3 /absolute/path/.lazyrex/bin/session-start.py codex",
        "timeout": 5
      }]
    }]
  }
}
```

Review and trust the new Codex hook through `/hooks` before relying on it.
Do not disable hook trust for normal use.

Claude Code (`~/.claude/settings.json`): use the same entry with `claude` as the final command argument.
Add that command to `UserPromptSubmit` as well. This hook reports `source=prompt`.
The next prompt can repair a lost report after a hub outage.
The parent starts a reporter process before the reporter reads input.
The reporter replaces itself with Rex. Its PID and start time stay the same.
The parent waits at most three seconds and returns success even if Rex fails.
Hook reports use reporter process launch order. A later prompt can also repair a lost resume report.
Event order equals process launch order only if the agent waits for each hook.
This ordering is unverified for both agents.

For both agents, retain Rex's own SessionStart hook. LazyRex adds a separate report.
The report is advisory. MCP tool discovery does not wait for it.
Codex can also supply its conversation ID in each MCP tool call.
