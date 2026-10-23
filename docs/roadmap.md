# Roadmap

## Planned

- **The live hub runs under the restart wrapper.** The live hub runs `lazyrex-hub -restart` from
  the user's install layout, as users run it, with the rotated log. The new interface is
  `hub.admin.restart` and `lazyrex-hub restart` (child exit 4, then the wrapper runs its own
  binary again with the lock held). Target: before the
  public candidate.
- **Live feed.** A LazyRex surface (its place is not chosen yet) shows recent and running plugin operations (from `view_ops` and
  the hub's records) with their state, and a Retry for failures. Running work may also report
  through Rex's Program Status Protocol, so that the app shows it in tab headers and in the
  Activity sidebar (untested in the app).
- **Renders that depend on watch state.** Track the watch state that a view render reads, as the
  hub tracks SQL reads.
- **Events first.** The hub reacts to a Rex event wherever Rex has one (for example
  `session_label_changed`, `terminal.title_changed`). It polls only where Rex has no event.

## Requests for superlogical

Ordered by value to LazyRex.

1. An `env` option on terminal spawn.
2. A documented data-channel HELLO and in-band `rex1` message format, with stable protocol versions.
3. A session hide or archive flag (or free-form session metadata).
4. Caller identity on `block.method`. Requests to a block plugin carry only
   `{block_id, method, args}`, from every path (API, palette, key, `rex do`, Lua), and no event
   shows the caller. Ask: the calling client (`requester {client_id, principal}`, as in
   `client.perform`), `origin` and the Rex `session_id`, or a method descriptor flag that asks
   for them. Today LazyRex routes calls that need identity through `client.perform`
   (`rex-protocol.md`, "Block method caller identity").
5. Restart of crashed block plugins, and discovery of new block plugins at runtime.
6. A working `rex block stream`.
7. Codex detector: match a 24-hour clock in the `turn_end` rule (it requires AM/PM on
   0.0.1792088193-g7a93ec), so that Codex reports `done` with no hook. Less urgent: Codex CLI
   0.163.0 reports OSC 7501 itself.
8. User input activity for a block: a `last_input_at` on the block, or an input event, for keys
   and pastes that reach the terminal from any client. Better: an OSC 7501 field with which a
   program reports unsent text in its input (a draft). LazyRex writes notices into agent panes;
   without this, a notice's Enter can submit text that the user is typing. Rex
   0.0.1792158932-g88fd41 has neither (API reference): keys reach scripts only when a keymap
   binds them, and data connections only receive.
9. Fix Lua multiple assignment (observed on Rex 1203 and on 0.0.1792158932-g88fd41, lab). In a
   plain multiple assignment to existing variables, a later value that reads an earlier target
   sees the new value. Minimal repro: `local x, y = 1, 2; x, y = y, x + 10; print(x, y)` prints
   `2 12` (Lua: `2 11`); `a, b = b, a` gives `(b, b)`. `local a, b = b, a` and table fields are
   correct. Plugins must use a temporary local until then; `lazyrex-hub test` warns about the
   pattern.
10. A visibility filter on `rex.action` (for example `when = { block_label = "lazyrex-view:*" }`),
    so that a palette entry shows only while a matching pane has focus. Today the entry list
    cannot follow focus, so LazyRex keeps tab-specific commands in the tab.
11. A block plugin that wraps a terminal block: it renders as a terminal in the app and adds its
    own block actions. Then a LazyRex thread could be its own block type with contextual palette
    actions. Today the app shows "Unsupported block" for any non-terminal type.
12. Custom events. LazyRex wants to publish its own events (spawn opened, agent joined, worktree
    created) on Rex's event stream, so that consumers use `rex events`, `rex.on` and control
    clients, and no second bus exists. `block.event` refuses custom types from a client. A block
    plugin can emit a free-form `block_event` with an undocumented frame (`rex-protocol.md`,
    "Plugin event emission"). Ask: document that frame, or add `event.publish {name, payload}`
    for a client, delivered like other block events.
13. Peer PID to block lookup: a Rex method that resolves an attested peer PID to its terminal
    block and session, and keeps process identity across PID reuse. Today the hub lists sessions
    and terminal blocks, then checks process ancestry. Labels and client block ownership do not
    give this identity.
14. Existing blocks in a new window. `session.new_window` creates new block templates, and
    `session.move_block` needs a visible anchor. Ask: a new window that accepts existing detached
    blocks, or an atomic layout operation. Then the hub needs no temporary anchor processes and
    no recovery records for them.
15. Idempotent create operations: caller-supplied operation keys on session, window and block
    creation, with a result that a caller can query. After a lost reply, an absent label does not
    prove that a delayed request cannot create the resource, so the hub and the Worktrees plugin
    keep unresolved intents. Case (lab): after a hub crash during a session-template start, a
    whole tab of shell panes exists, and the hub cannot adopt them. Labels are hints, and macOS
    hides the environment of platform binaries such as `/bin/zsh`, so no process proof exists
    (`rex-protocol.md`, "A lost new_window answer"). The template run stays pending until the user
    dismisses it.
16. An exit callback for `rex.exec.start` (an `on_exit` option), so that a plugin needs no timer
    to poll `h:running()`.
17. Move a block to another session. `move_block` and `move_window` stay inside one session.

## Open questions

- Do `rex1` APC messages appear later in the data stream, and what types exist?
- What does `session.open_on_web` do? It appears to target Rex's hosted web client
  (`rex.superlogical.com`, private beta). Not observed. See `rex-protocol.md`.
- How should the hub block Rex autostart from inside an agent sandbox?
- What evidence lets the hub decide that an uncertain spawn failed, so that a retry is safe?
- How does the hub tell a deliberate user close from a crash, so that a deliberate close stays
  closed?
- Is there an atomic "inject if idle" operation? None was found, so a user or an agent can change
  state between the check and the write.

## Known defects

- **Lab: `rex send-key` sends a capital letter as lowercase (Rex 0.0.1792158932-g88fd41, 2026-10-17,
  inferred from the form screen).** `send-key M` typed `m` in a form text field. Tests type lowercase text.
- **Renderer: text without key events does not reach the thread composer (lab, Rex 1203, 2026-10-17,
  cause unknown).** `rex send -b <thread view> <text>` did not change the composer; `rex send-key`
  letters did. Pasted text may be dropped. Independent reproduction remains unverified.
- **Renderer: keys right after a focus change are lost (lab, Rex 1203, 2026-10-17, cause unknown).** In a
  thread view, the first two letters sent with `send-key` right after the Tab that moves focus to
  the composer did not appear. A 500 ms wait avoided it. Independent reproduction remains unverified.
- **Hub: a late `view.close_request` can close the composer after a navigation cancelled the close
  (source reading, 2026-10-22, not reproduced; timeout audit row 15).** The case: the user closes the
  transcript, so the hub sets `close_pending` and sends `view.close_request` to the composer
  attachment. The renderer does not take its queue at once. The user navigates to the tab, and
  `resetTranscripts` clears `close_pending`; the composer attachment stays the same. Rex then delivers
  the request. The renderer checks only the view and the attachment, so it accepts, sets
  `closePending` and closes through `closeView`, the Ctrl+Q path, which does not check
  `close_pending`. The composer closes although the close was cancelled. No probe has run, so the
  runtime case is not verified. It ends with the transcript sweep (claude-43), which removes
  `requestClose` and `close_pending` with the transcript and composer pair. If the sweep keeps
  `view.close_request`, the fix comes back to the hub lead.

## Cleanup and test debt

One line per item: what, its owner, and why it waits.

- `viewnotify.go`: remove the nil `ctx` fallback to `context.Background()`, and give each test hub a
  `ctx` (`newViewSvc()` then `h.vs.ctx`). Owner: Hub Features. Waits: it was outside the
  constructor change's scope (lead-c37-item6g1); it is a test-only guard with no production effect.
