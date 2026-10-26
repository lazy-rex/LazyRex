# Roadmap

## Planned

- **`lazyrex-hub test` from a curl install.** A curl install can run `lazyrex-hub test` without a
  LazyRex checkout (the archive ships the test runner and the home plugin). The first planned item
  after the launch.
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
    (`rex-protocol.md`, "A lost new_window answer leaves panes that the hub cannot prove"). The template run stays pending until the user
    dismisses it.
16. An exit callback for `rex.exec.start` (an `on_exit` option), so that a plugin needs no timer
    to poll `h:running()`.
17. Move a block to another session. `move_block` and `move_window` stay inside one session.
18. A documented tab glyph for a program: an OSC 7501 key, or a documented list of the `app` and
    process-name keys that the app maps to glyphs. The renderer reports `app=lazyrex` (view/status.go),
    and which glyph that gives is not documented.

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

- **Renderer: text without key events does not reach the thread composer (lab, Rex 1203, 2026-10-17,
  cause unknown).** `rex send -b <thread view> <text>` did not change the composer; `rex send-key`
  letters did. Pasted text may be dropped. Independent reproduction remains unverified.
- **Renderer: keys right after a focus change are lost (lab, Rex 1203, 2026-10-17, cause unknown).** In a
  thread view, the first two letters sent with `send-key` right after the Tab that moves focus to
  the composer did not appear. A 500 ms wait avoided it. Independent reproduction remains unverified.

## Cleanup and test debt

One item each: what, and why it waits.

- `viewnotify.go`: remove the nil `ctx` fallback to `context.Background()`, and give each test hub a
  `ctx` (`newViewSvc()` then `h.vs.ctx`). Waits: it was outside the
  constructor change's scope; it is a test-only guard with no production effect.
- Narrow `changed(name)` for a view with no `deps.changes` and a tracked read set to the names it
  read (today every changed name marks such a view). Waits: an inventory of
  plugin views that read hub data outside covered calls.
- A plugin spawn whose outcome stays unknown reports `running` and has no timer owner. Two cases:
  the child read after `createBlock` fails or times out (the intent stays `sending`), and the block
  read after the block ID is recorded fails while the child had already exited (`checkMapped`
  logs it). The next start, reload or hub restart retries the reconciliation: it adopts a live
  child of the first case by its instance argv. It does not resolve a first-case child that
  exited: the intent does not record the block ID, and `findInstanceBlock` matches only a live
  child, so the intent stays unresolved. Waits: the fix (a `starting` state
  with a backoff timer, which must also attribute that exited child to its block) is larger than the
  change that bounded the plugin child read.
- `hub.agents` `session_id` does not follow a pane move: only an identity report writes
  `identity_panes.session_id` (identity.go), so a moved agent pane shows its old session until its
  next report. Waits: a Rex fact: does Rex send an event when a block moves to
  another session?
- Rex calls on the hub run context with no bound: `identityReport`'s `paneOfHint` and `paneName`
  (and any other found by a grep for `rex.Call`/`BlockCall` on `h.ctx`). Waits:
  one audit for all; 900e3f0c (`pluginChildRead`) bounds only the plugin child read.
- A lab fault after a `hub.sql` commit and before its reply (exit or pause), as `store-commit` was
  for `hub.store.batch` before the hub.store removal. Today no lab test ends the hub between a SQL
  commit and its reply. Waits: no test in this repository used `store-commit`,
  so the removal does not add the SQL point; a plugin test that needs it asks for it.
