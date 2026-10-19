# Roadmap

> Note: this document was written during the spike phase. Paths such as `spikes/03-ui-framework/`
> refer to the original research repository. In this repository, that code is at the root
> (`hub/`, `view/`, `plugins/`, `tests/`), and each plugin has its own repository.


## Spikes

Each spike answers one question. Spikes live in `spikes/NN-name/` with a README that states the question and the result.

| # | Question | Status |
|---|---|---|
| 0 | Can restty show a Rex terminal from Rex's GHOSTSNP snapshot? | **Done: yes**, after cutting the frame at FINISH. See `spikes/00-ghostsnp-restty/` |
| 1 | Can the hub and a messaging plugin let agents message each other with attested identity and wake-up of idle agents, instead of typing into each other's panes? | **Done: yes**, with real Claude Code and Codex, through the `rex` CLI and through the MCP shim (Codex read-only). See `spikes/01-hub-lua-plugins/` |
| 1b | Can a browser page, connected to Rex directly, show a live Rex terminal with restty, with input, resize, mouse and paste? | Next. Handshake and snapshot already pass in Chrome (lab). Still to test: rendering, input, resize, reconnect, authenticated remote access |
| 2 | **Next (the maintainer, 2026-10-16):** a true hub with messaging. Can Claude and Codex in this repo message each other through a hub that survives a restart, with durable messages? | **Lab: pass** (2026-10-16), all 16 criteria. See `spikes/02-hub-messaging/`. Real-server run waits for the maintainer's approval. Scope: hub services (SQLite storage per plugin namespace), agents join through the MCP shim (no hand-written block IDs), durable mailbox (one transaction: message, idempotency key, notice intent), hub and plugin restart with reattach, readiness from Rex program status. Pass: a lab run with real agents that survives a hub restart between commit and reply, with no lost or duplicate message. Then, with the maintainer's approval, run it on the real Rex server |
| 2b | Can a remote browser reach a Rex terminal through an authenticated gateway over HTTPS and WebSockets through opentunnel? | Planned. First test WebSockets through opentunnel in a real browser. WebRTC waits for a need the tunnel cannot meet. The gateway exposes a narrow, authorized set of operations |
| 3 | Can the hub drive the Rex app as a TUI state machine? A hub TUI and a messaging TUI (Bubble Tea v2, declared views) in their own sessions. "Messages" switches the app to the messaging session and "back" returns. Mouse and resize in the Rex app | Proposed (2026-10-16). Building blocks verified on the real machine: `session.select`, `detach_block`, `move_block`, `pane.move_to_new_tab`. Run in the lab |
| 3a | Hardening of spike 1: can agents be denied the Rex socket by an OS sandbox while they still reach the hub? Does delivery hold up under races and restarts? | Planned |
| 4 | Can a real LazyRex plugin run in a plugin host in a Rex pane, and survive a hub restart? | Planned. Pass criteria: the pane stays alive across a hub restart. The host reconnects with no duplicate process or registration. The host refreshes entities. One in-flight hub-mediated mutation is reconciled. One local commit followed by a lost handler result is reconciled. An uncertain spawn does not create a duplicate. See `architecture.md` |

`session.open_on_web` apparently targets Rex's hosted web client (`rex.superlogical.com`, private beta). Exact behavior not observed. See `rex-protocol.md`.

## Requests for superlogical

Ordered by value to LazyRex.

1. An `env` option on terminal spawn.
2. A documented data-channel HELLO and in-band `rex1` message format, with stable protocol versions.
3. A session hide or archive flag (or free-form session metadata).
4. Caller identity on `block.method`.
5. Restart of crashed block plugins, and discovery of new block plugins at runtime.
6. A working `rex block stream`.
7. Codex detector: match a 24-hour clock in the `turn_end` rule (it requires AM/PM on 0.0.1792088193-g7a93ec), so that Codex reports `done` with no hook. (Less urgent: Codex CLI 0.163.0 reports OSC 7501 itself.)
8. User input activity for a block: a `last_input_at` on the block, or an input event, for keys and pastes that reach the terminal from any client. Better: an OSC 7501 field with which a program reports unsent text in its input (a draft). LazyRex writes notices into agent panes; without this, a notice's Enter can submit text the user is typing. Rex 0.0.1792158932-g88fd41 has neither (API reference): keys reach scripts only when a keymap binds them, and data connections only receive.
9. Fix Lua multiple assignment: `a, b = b, a` gives `(b, b)` on 0.0.1792158932-g88fd41 (`spikes/lab/lua-swap-probe.lua`). Plugins must swap through a temporary local until then.
10. A visibility filter on `rex.action` (for example `when = { block_label = "lazyrex-view:*" }`), so that a palette entry shows only while a matching pane has focus. Today the entry list cannot follow focus; LazyRex keeps tab-specific commands in the tab (the maintainer, 2026-10-17).
11. A block plugin that wraps a terminal block: it renders as a terminal in the app and adds its own block actions. Then a LazyRex thread could be its own block type with contextual palette actions. Today the app shows "Unsupported block" for any non-terminal type.

## Open questions

- Do `rex1` APC messages appear later in the data stream, and what types exist?
- How does restty render a large Rex scrollback, and how big is the snapshot?
- How should the hub block Rex autostart from inside the agent sandbox?
- Which event reports a `program_status` change? Answered: `program_status_changed` and `program_status_removed` (see `rex-protocol.md` and `agent-wake.md`).
- Can a `rex do` script answer `client.perform`? Answered: yes, with `rex.call("client.reply", ...)` (see `rex-protocol.md`). A Node control client can too.
- Is a Lua hub fast enough with one-at-a-time handlers, and can it walk the requester pid to a pane without a helper binary?
- What evidence lets the hub decide that an uncertain spawn failed, so a retry is safe?
- How does the hub tell a deliberate user close from a crash, so a deliberate close stays closed?
- Is there an atomic "inject if idle" operation? None was found, so a user or agent can change state between the check and the write.

- **Custom events (requested 2026-10-16).** LazyRex wants to publish its own events (spawn opened, agent joined, worktree created) on Rex's event stream, so consumers use `rex events`, `rex.on` and control clients and no second bus exists. Today `block.event` refuses custom types from a client. A block plugin **can** emit a free-form `block_event` with an undocumented frame (found 2026-10-16, see `rex-protocol.md`, Plugin event emission), so LazyRex has a working path through a plugin block. The ask stays: document that frame (and the `api` action, which did nothing in the lab), or add `event.publish {name, payload}` for a client. Ask: a documented way to publish a namespaced event (for example a block plugin emitter, or `event.publish {name, payload}` for a client), delivered like other block events.
- **Peer PID to block lookup (proposed 2026-10-16).** The hub currently lists sessions and terminal blocks, then checks process ancestry. Ask for a Rex method that resolves an attested peer PID to its terminal block and session. Preserve process identity across PID reuse. Labels and client block ownership do not provide this identity.
- **Existing blocks in a new window (proposed 2026-10-16).** `session.new_window` creates new block templates. `session.move_block` requires a visible anchor. Ask for a new window that accepts existing detached blocks, or an atomic layout operation. This would remove temporary anchor processes and their recovery records.
- **Idempotent create operations (proposed 2026-10-16).** Ask for caller-supplied operation keys on session, window, and block creation, with a queryable result. After a lost reply, an absent label does not prove that a delayed request cannot create the resource. The hub and worktree plugin must retain unresolved intents today. A Rex-owned result would let callers retry without duplicate resources. Case (2026-10-17, lab): after a hub crash during a session-template start, a whole tab of shell panes exists, and the hub cannot adopt them: labels are hints, and macOS hides the environment of platform binaries such as `/bin/zsh`, so no process proof exists (`rex-protocol.md`, "A lost new_window answer"). The template run stays pending until the user dismisses it.
- **Exit callback for `rex.exec.start` (requested 2026-10-16).** Plugins poll `h:running()` today. Ask: an `on_exit` option, so a plugin needs no timer.
- **Caller identity on block methods (proposed 2026-10-16).** `block.method` requests to a block plugin carry only `{block_id, method, args}`, from every path (API, palette, key, `rex do`, Lua). No event shows the caller either. Ask: add the calling client (`requester {client_id, principal}` as in `client.perform`), `origin` and the Rex `session_id`, or a method descriptor flag that asks for them. Today LazyRex routes identity-carrying calls through `client.perform` (see `rex-protocol.md`, Block method caller identity).
- **Move a block to another session (requested 2026-10-16).** `move_block` / `move_window` stay inside one session.

## Planned (agreed with the maintainer, 2026-10-17; not built)

- **One LazyRex tab.** Built (see decisions.md, "One LazyRex session; app mode retired"). Still to do: plugins add widgets to the LazyRex tab, and Worktrees becomes palette commands ("New Worktree…", "Remove Worktree…", "Worktrees…") plus a widget.
- **Side panels, then Tunnels.** See decisions.md, "Side panels and the Tunnels plugin".
- **Live feed.** The LazyRex tab shows recent and running plugin operations (from `view_ops` and the hub's records) with their state and a Retry for failures. Running work may also report through Rex's Program Status Protocol, so the app shows it in tab headers and the Activity sidebar (to check in the app).
- **Start commands.** "Start Agent…" and "Start Accessory…": a side-pane form with the profile, the directory (default: the source pane's `terminal.pwd`; Browse… with `terminal.list_dir`; a worktree choice for a git repo; maybe "New worktree…"), and the placement (new tab or split). "Start Session…": a saved template (a named group of tabs, each with a profile, a directory relative to a root, and its place in the layout); the form asks for the root, so a template runs in any repo or worktree. "Save as Template…" stores a session's tabs relative to its common root (the git repo root when there is one). A template is a Durable Session that never ran.
- **Events first (the maintainer).** The hub reacts to Rex events wherever Rex has one (for example `session_label_changed`, `terminal.title_changed`), and polls only where Rex has no event.

## Known defects

- **Messaging: a group send fails when its mailboxes are large (lab, Rex 1203, 2026-10-18).** The send writes every member's mailbox and the thread record in one `hub.store.batch`. The batch must fit in one 64 KiB control message (rex-protocol.md, "Rex Lua runtime"). In the lab, with no reader acknowledging, Team sends failed after 287 messages (`control message too large: encoded request is 65541 bytes`). Live mailboxes are small while agents read their mail, but a large group or an agent that never reads can reach the limit. Fix proposal: split the batch, or let the hub append to mailboxes.

- **Lab: `rex send-key` sends a capital letter as lowercase (Rex 0.0.1792158932-g88fd41, 2026-10-17, inferred from the form screen).** `send-key M` typed `m` in a form text field. Tests type lowercase text.

- **Renderer: text without key events does not reach the thread composer (lab, Rex 1203, 2026-10-17, cause unknown).** `rex send -b <thread view> <text>` did not change the composer; `rex send-key` letters did. Pasted text may be dropped. Not reproduced by Codex yet.
- **Renderer: keys right after a focus change are lost (lab, Rex 1203, 2026-10-17, cause unknown).** In a thread view, the first two letters sent with `send-key` right after the Tab that moves focus to the composer did not appear; a 500 ms wait avoided it (`tests/thread-home-lab.mjs`). Not reproduced by Codex yet.

- **Migration fixture: draft-only thread closes; navigation times out (observed in the lab, Rex 0.0.1792158932-g88fd41, 2026-10-18, cause unknown).** The current hub closed the migrated `thread/claude` view because the messaging plugin did not declare this draft-only thread. An explicit `hub.ui.navigate` request then timed out after 10 seconds. The nested `hub.agents` request answered only at that timeout. Phase 4 of `tests/one-tab-migration-lab.mjs` stopped before the selector check. This fixture tests a one-time migration that is already live. It does not gate the select change. Investigate the fixture and the nested request before reusing this migration check. Probable cause (2026-10-18, not verified for this fixture): a hub handler waited for the plugin while the plugin's own `hub.agents` call waited in the hub's request queue (rex-protocol.md, "The hub has one request queue"). `hub.view.history` hit the same stall in the lab before it became asynchronous.
