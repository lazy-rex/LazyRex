# Agent prompt tests

Lua tests of agent prompts (docs/hub.md, "Agent prompts and acting as the user") on the real hub,
with real panes that report OSC 7501 status. Run:

```sh
lazyrex-hub test tests/prompts
```

The fixture plugin `promptfix` (`fixture.lua`) has the `prompts` grant (manifest `test.prompts`).
It applies `hub.prompt` pushes and `hub.prompts` snapshots with the plugin's apply rule of
docs/hub.md ("Prompt pushes"), so it is also a reference for a real plugin. Test agents report
status with `a:program_status(state, {kind, app, msg})`, and `a:input()` returns the bytes that an
answer wrote to the pane.

## Coverage

| Case | Test |
| --- | --- |
| A blocked report opens a prompt; a push and `hub.prompts` give it; `hub.agents` gives `block_id` | prompts.lua |
| The same report keeps the prompt; another msg replaces it (a new id, generation + 1) | prompts.lua |
| A state change closes it; `hub.prompts` is empty | prompts.lua |
| A blocked report without kind and msg (Codex) opens a prompt | prompts.lua |
| keys: the bytes, no Enter; text: a bracketed paste (newline to space), then Enter | prompts.lua |
| A repeated key writes nothing; other input with the key is `operation_conflict` | prompts.lua |
| An answer to a closed prompt: `refused`, zero bytes | prompts.lua |
| A close before the write: zero bytes; a close before Enter: `partial`, the paste only | prompts.lua |
| A write refused with `not_found`: `failed`; a lost Enter reply: `unknown_after_text` | prompts.lua |
| A lost push reply, then a close: the plugin ends with no prompt | prompts.lua |
| A hub restart: a new generation; a repeated answer key returns its record and writes nothing (H1d); a new prompt id | prompts.lua |
| The apply rule: a newer open push before an older snapshot; a newer close before an older snapshot; the snapshot floor; a stale generation | prompts.lua |

Unit tests (`hub/cmd/hub/prompts_test.go`, `promptpush_test.go`, `promptanswer_test.go`) cover:
the state machine with rebind and process exit; the 8 KiB cut on a UTF-8 boundary and a change
after the cut; one outstanding push call with a plugin that never answers; a push of an old
registration; every write error class; a crash during a write (`unknown`, `unknown_after_text`);
retention while open and 7 full days after the close; a repeat while the first answer writes
(`in_progress`); a close during admission.

Not covered by a lab test:

- A hub crash in the middle of a write. The unit test `TestPromptAnswerRestart` sets the rows in
  `writing` and `text_written` and runs the start.
- The hourly sweep's timer. The unit tests call the sweep with a test clock.
- Real Claude Code and Codex panes. The real-agent facts are from the Telegram reviewer's lab
  (rex-protocol.md, "Blocked records of real agents").
