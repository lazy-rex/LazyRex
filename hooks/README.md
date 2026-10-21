# Conversation hook

`lazyrex-hub hook claude|codex` reports Claude Code and Codex conversations through the Rex CLI.
It needs only the `lazyrex-hub` binary and `rex` on `PATH`. `REX_BIN` can select an absolute Rex executable.
The source is `hub/cmd/hub/hook.go`.

The hook reads the agent's hook JSON from standard input. It sends `kind`, `conversation_id`, `source`, and a `block` hint to `hub.identity.report`.

The input is bounded to 1 MiB, because every agent start runs this process and it must not read without a bound (the same reason as the manifest bound).
A longer input gets no report; it is never parsed as a prefix. A SessionStart event is a few KB.
A UserPromptSubmit event contains the user's prompt, so a prompt near 1 MiB gets no `prompt` report. That report only repairs a lost one.
Rex attests the caller PID. The hub must verify the pane and the current process generation before accepting the report.
The hook does not trust `REX_SESSION` or `REX_BLOCK` as proof.

The hook does nothing outside a Rex pane. It does not start Rex or retry a missing hub.
It selects exactly one connected client labeled `lazyrex-hub`.
It ignores lost clients that Rex keeps after a hub restart.
It produces no model context. The command has a three-second deadline.

## Installation

Build the binary (`hub/` module: `go build -o ~/.lazyrex/bin/lazyrex-hub ./cmd/hub`).
Add the entries below to the existing lists. Preserve the other hooks.
JSON does not expand `~`: replace `/ABSOLUTE/HOME` with the absolute path of your home directory.
The path is quoted, so a home directory with a space works when the agent runs the command through a shell.
Claude Code documents that it does. For Codex this is untested.
These are the exact entries; `lazyrex-hub setup` and a switch of existing hooks use the same text.

Codex (`~/.codex/hooks.json`), one entry in `hooks.SessionStart`:

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "\"/ABSOLUTE/HOME/.lazyrex/bin/lazyrex-hub\" hook codex",
      "timeout": 5
    }
  ]
}
```

Review and trust the new Codex hook through `/hooks` before relying on it.
Do not disable hook trust for normal use.

Claude Code (`~/.claude/settings.json`), one entry in `hooks.SessionStart` and the same entry in
`hooks.UserPromptSubmit`:

```json
{
  "hooks": [
    {
      "type": "command",
      "command": "\"/ABSOLUTE/HOME/.lazyrex/bin/lazyrex-hub\" hook claude",
      "timeout": 5
    }
  ]
}
```

The `UserPromptSubmit` entry reports `source=prompt`.
The next prompt can repair a lost report after a hub outage.

To switch from the Python hook, replace the command `python3 /ABSOLUTE/HOME/.lazyrex/bin/session-start.py claude`
(or `codex`) with the command above, in each entry that has it. Keep `"timeout": 5`.

## Process order

The hook process starts a reporter process before anything reads the input.
The reporter reads the input. Then it replaces itself with Rex: its PID and start time stay the same, and its parent is the hook process.
The hook process waits at most three seconds, then kills the reporter. It returns success even if Rex fails.
Hook reports use reporter process launch order. A later prompt can also repair a lost resume report.
Event order equals process launch order only if the agent waits for each hook.
This ordering is unverified for both agents.

For both agents, retain Rex's own SessionStart hook. LazyRex adds a separate report.
The report is advisory. MCP tool discovery does not wait for it.
Codex can also supply its conversation ID in each MCP tool call.
