# Architecture

> Note: this document was written during the spike phase. Paths such as `spikes/03-ui-framework/`
> refer to the original research repository. In this repository, that code is at the root
> (`hub/`, `view/`, `plugins/`, `tests/`), and each plugin has its own repository.

## One sentence

Rex runs every process and draws every terminal. LazyRex is a Rex client that adds agents, plugins, MCP, messaging and a web client on top.

## Who owns what

| Concern | Owner | Notes |
|---|---|---|
| PTYs, terminal parsing (libghostty), scrollback, snapshots | **Rex** | |
| Layout: sessions, tabs, panes, native macOS app | **Rex** | |
| Agent busy/idle detection | **Rex**, plus LazyRex hooks | `program_status_changed` / `_removed` events. Rex's Codex detector misses turn end on 24-hour clocks, so LazyRex agent hooks should report status with OSC 7501. See `agent-wake.md` |
| Process supervision for agents and plugins | **LazyRex** | Spawns them as Rex panes, watches `child_exited` block events |
| Plugin runtime (Lua), plugin database, entities, events | **LazyRex** | Where it runs is open. A `rex do` Lua script can serve RPC through `client.perform` (lab), but has no sockets, JSON or SQLite built in (see the option under study "the hub in Rex's own Lua") |
| MCP server and agent-to-agent messaging | **LazyRex** | Caller identity from peer-pid ancestry |
| Terminal bytes and input to the browser | **Rex** (proposed) | The browser speaks the Rex protocol itself, either on its own admitted connection or through a paired gateway. See "Web client" |
| Web client UI for agents, plugins, messages | **LazyRex** | Served from the hub's own API |
| Workspace model | **LazyRex** | Workspace = Rex session |

```text
browser (restty) ---- rex.control.v1 / rex.data.terminal.v1 (direct, or via gateway) ----> Rex server
       |                                                                                    ^
       +--- LazyRex API (agents, plugins, messages) ---> LazyRex hub -- rex.control.v1 -----+
                                    |                                                       |
                         MCP + messaging socket                                 PTYs: agents, plugins,
                         plugin runtime + DB                                    dev servers (panes)
                                    ^                                                       |
                                    +------------------- agents call MCP -------------------+
```

## Process model

- **The Rex server is the core.** It runs as the user's normal Rex server. When it stops, all terminals stop. That is accepted.
- **The LazyRex hub** runs as a launchd/systemd service and connects to Rex as a client. It can restart without losing terminals. On reconnect it re-reads state, because Rex does not replay events.
- **Agents** run in Rex panes inside the workspace's session.
- **Plugins** run in Rex panes too, so each one is isolated and its log is visible. A plugin pane talks to the hub on the hub's own socket, not through Rex. Plugin panes can be detached so they do not fill the layout.
- Optional: a small, stable Rex block plugin that forwards palette actions to the hub. Rex finds block plugins only at server start, so individual LazyRex plugins must NOT be Rex block plugins.

## Hub restart and plugin reattach

**State.** The hub stores, for each plugin instance: an instance ID, the package identity, the intended state (running or stopped), and the Rex session and block IDs. **Spawn.** The hub saves a spawn intent with a stable instance ID **before** it calls Rex. Then it creates the block and records the block ID. If the hub stops between the two steps, the intent tells it that a block may exist. The hub also labels the block `lazyrex:<kind>:<id>`. A label is only a discovery hint, not proof of identity: any client can copy or change it.

**Restart.**

1. The hub starts a new hub epoch. Each host connection also gets its own connection generation, so a host that reconnects without a hub restart cannot have an old connection replace registrations or return stale results. (Open: exact scheme.)
2. The hub subscribes to Rex events first, then reads Rex state (`session.list`, `session.list_blocks`). Events can race the read, so the hub rechecks each affected block before it acts.
3. Each plugin host keeps its instance ID and reconnects to the hub socket with backoff.
4. The hub matches a returning host to its existing block. It checks the claim with peer-pid ancestry and does not trust a claimed block ID alone. Ancestry attributes a host to a block. It does not prove package identity and is not a security boundary.

   A block that exists is not enough to adopt. A kept-open block can hold an exited host or an unrelated process. The hub adopts a block only when the terminal process is alive and the host completed its handshake.
5. The host registers its descriptors and subscriptions again. Each instance has one active registration. A new registration replaces the old one.
6. The hub requests entity snapshots from the host (the provider) and refreshes its subscriber views. If a host must also reload hub-owned state, that is a separate operation.
7. A spawn intent with an uncertain result stays unresolved until reconciliation establishes the outcome of the earlier Rex request. The hub finds candidate blocks by label and waits for a host handshake. "Waited and saw nothing" is not permission to spawn again: the earlier request can still complete. If the Rex API cannot prove that the earlier request failed, the hub reports a conflict or asks for recovery direction. (Open: what evidence is enough.)
8. If more than one candidate matches, the hub reports a conflict. It does not adopt or stop a block at random.
9. The hub spawns a replacement for a missing mapped block only if the intended state is still "running" and the lifecycle policy permits a restart. A block that the user closed on purpose stays closed. A timeout alone never causes a second host. Open: a missing block or a `child_exited` event does not show that the user wanted a permanent stop. Explicit stops must go through the hub, or the hub must define how it classifies a native Rex close.
10. The hub leaves alone any block that it did not map.

**Hub database loss** is a separate recovery mode. Labels cannot rebuild the intended state, package configuration or authoritative versions. The hub leaves candidate blocks untouched until the user starts an explicit recovery.

**Failure rules.**

- When a connection drops, each pending RPC fails with an explicit "outcome unknown" result if it may have run.
- Do not retry a mutation (session creation, coordination publish) blindly. First read the authoritative state, then decide.
- Plugin identity (instance ID) stays the same across hub restarts. The hub epoch and the connection generation change. The hub rejects replies and registrations from an old epoch or generation.
- The hub must service plugin callbacks while it waits for a handler result. If it does not, synchronous Lua callbacks deadlock.
- The plugin host owns its database (see the proposed decision "a plugin host process that owns its plugin's storage"). So the host never waits on an RPC for a database call. This does **not** make a handler invocation exactly-once: a plugin can commit locally, then crash or disconnect before it returns the result. The caller (hub or MCP client) then gets "outcome unknown" and must reconcile durable state before it retries.

## Workspaces

- A workspace is a Rex session. Its tabs and panes are Rex windows and blocks.
- **Hide a workspace:** detach every block. Processes keep running. The sidebar still shows an empty session header (cosmetic; a Rex hide flag would fix it).
- **Unhide:** create a window with a placeholder, move the blocks back, close the placeholder. Simpler when a pane is visible anywhere in the session: `move_block` beside it, then the app's `pane.move_to_new_tab` (verified 2026-10-16).
- **What the user sees:** the hub can switch the app's view with app actions (`session.select`). See `rex-protocol.md`, "Driving the Rex app" (and the Rex agent guide, `rex __internal skill`), and the proposed decision "the LazyRex UI is TUIs in Rex, and the hub drives the app's view".
- LazyRex keeps its own map from workspace and agent IDs to Rex session and block IDs.

## Agent identity and messaging

1. Each agent calls hub and plugin actions through Rex (`rex -C lazyrex do`). Its hooks report its conversation identity (`hub.identity.report`).
2. The hub reads the peer pid of the connection and walks up the parent chain to a Rex pane's child pid. That pane is the caller.
3. The hub adds the sender's name from its own map. It does not trust what the caller says about itself.
4. Message content stays in the mailbox. The agent reads it with an MCP tool (`read_messages`).
5. The hub writes only a one-line notice into the recipient's pane, when per-agent readiness rules allow it. Rex status alone is not enough: a Codex "clear" does not mean idle, and Claude can be ready with a `done` record. Rex has no atomic "inject if idle" operation, so the check and the write can race. See `agent-wake.md`.
6. Acceptance for the messaging spike is the recipient's actual `read_messages` result, not only accepted terminal bytes.

This is attribution, not security: all agents run as the same uid. To stop agents from driving Rex directly, run them under an OS sandbox that denies the Rex socket (see `rex-protocol.md`).

## Web client

See "Browser access" in `rex-protocol.md`.

Two access options. Neither is chosen.

1. **Direct network access.** The browser opens its own admitted Rex connection and gets its own tickets. Verified only on anonymous loopback TCP. Remote use needs a reachable, authenticated transport, such as a tailnet. Authenticated remote access is untested.
2. **Paired gateway.** A LazyRex gateway authenticates the browser (pairing with a key exchange), then carries separate control and data connections to the Rex Unix socket. Rex sees the gateway's Unix-peer principal, not a browser identity. Rex has no per-method authorization, so a paired browser gets the complete Rex API. A byte-forwarding gateway cannot give the browser a separate identity or method limits inside Rex without more work. If WebRTC carries the traffic, the logical channels and the close and error mapping must be defined. Untested.

In both options:

- The browser speaks the Rex protocol itself. Input, resize and paste go on its own control channel.
- The browser renders with restty. The first frame needs the split described in `rex-protocol.md`.
- The hub does not carry terminal bytes. It serves only LazyRex data: agents, plugins, messages, workspace map.
- `session.open_on_web` apparently targets Rex's hosted client (`rex.superlogical.com`). Its exact behavior and transport are not observed (the site is behind SSO).
- Fallback: a protocol-aware relay, as first proposed. A ticket works only for the principal that requested it, so a relay on a different principal must open its own data channel.

## UI framework (design)

The maintainer's direction, as designed. The built hub differs: per-plugin sessions and the home tab are removed (`docs/hub.md`, "Removed: app mode" and "No LazyRex tab"). See the proposed decision "the LazyRex UI is TUIs in Rex, and the hub drives the app's view" and `rex-protocol.md`, "Driving the Rex app". Check the Rex agent guide (`rex __internal skill`) for documented layout and app features.

### Model: Rex is the navigation, TUIs are the interaction

- **The hub's home** is a route (`hub:/`). It shows widgets that plugins declare, for example an unread count. Selecting a widget navigates to a plugin route.
- **A plugin's home is a Rex session.** The first tab is the plugin's home page. The plugin declares the other tabs, for example one tab per open thread. Tabs open and close often; Rex handles this cheaply.
- **Every pane runs at a route** (plugin + path, for example `messaging:/thread/42`). The pane runs the shared renderer (`lazyrex-view`) or the plugin's own TUI program, and shows what the route shows.
- **Inside a tab, layout uses named slots.** A plugin never computes splits or ratios. The hub compiles slots into Rex operations: a recursive layout in `session.new_window`, `move_block` for existing panes, and `move_window` for tab order.

### Identity and state

- A **route** identifies content. A **view ID** identifies one pane. One route can be open in more than one pane.
- The hub's database is the source of truth. For each view it stores the plugin, the canonical route, the slot, the desired state and the Rex IDs. Rex labels are discovery hints only.
- A renderer counts as live only after it checks in with the hub from the pane the hub expects (caller-pane attribution, as for plugins). A copied label or a pane whose renderer has exited is not adopted.

### Reconciliation

- The hub reconciles only resources that it owns: LazyRex sessions, their tabs and their panes. It never changes agent sessions or the user's sessions.
- Reconciliation runs one plugin session at a time. The hub commits each open or close intent before it changes Rex. It merges repeated opens of the same route. A spawn whose outcome is unknown blocks a second spawn for the same view. Concurrent spawns and crash retries are bounded.
- Background changes use `focus=false`. Only an explicit navigation changes what the user sees.
- Arrangement snaps back to the declared layout (the maintainer). Overrides are a later topic. The hub ignores events from its own changes, and stops with a visible conflict if drift does not settle.
- To repair a ratio, the hub moves or resizes. It never respawns.

### Closes

- `block_closed` and `window_closed` have no actor (see `rex-protocol.md`). The hub rules out its own closes by matching them to its operations, and runs renderers in keep-open panes, so a renderer crash is a `child_exited`, not a close.
- A tab closed by the user closes the **view only**. The thread and its data stay. The plugin decides whether the thread gets a tab again.
- A tab that is missing after a hub restart is ambiguous. The hub does not archive anything because of it.

### Navigation across apps

- The hub sends one navigation intent to every connected app (the maintainer: all devices for now). Each app has its own delivery status. A disconnected or refusing app does not block the others. An app that reconnects gets only the latest intent.
- Before calling an app action, the hub checks the app's action list, because actions differ by app and version. It uses full IDs, not tab numbers.
- Two devices that show one pane share one terminal grid. The pane's owner sets its size (`com.superlogical.terminal.resize`).

### View contract

- A plugin returns a view tree with a schema version and stable node IDs. First node types: list, message history, composer, form, widget, and explicit loading and error states. History comes in bounded pages.
- The plugin owns domain state and actions. The renderer owns input editing, focus, hit testing, scrolling and terminal adaptation.
- The renderer subscribes, then fetches. It fetches again after a reconnect or a revision gap. A new data revision does not reset local UI state (text, cursor, focus, scroll).
- A custom TUI program is allowed for any route. It follows the same route, lifecycle and action contract, and it does not change Rex layout.
- Keys: Enter sends; Shift+Enter adds a new line (kitty keyboard protocol). Mouse works (Ghostty passthrough). Provide visible Back and Send controls for touch. No hover-only or right-click-only controls.

### Drafts and sends

- The composer saves its text to the plugin store as the user types, keyed by user and thread, with revisions. A native close kills the renderer at once, so a save only at close is not enough.
- The composer shows whether the text is saved. If the hub is down, it keeps showing the unsaved text.
- A send uses a durable operation key. The message and the removal of the matching draft revision commit in one transaction. A lost reply cannot duplicate a send or erase newer edits.

### Authority

- A registered renderer acts in the user's context (`user`). The hub binds it to that context and checks which actions it may call. Plugin-generated actions and agent calls keep their own origin.
- This is attribution, not security: all processes run as the same uid, and another Rex client can inject terminal input.

## Dependencies

LazyRex is a new Rex platform, designed from first principles.

- **Used as a library:** restty (`~/Projects/restty`) for the browser terminal. Spike 0 shows that it imports Rex snapshots.

## Rex ownership

Rex's [server API guide](https://www.superlogical.com/rex/docs/automate/server-api.md) explains how to inspect the current server contract.
The exact schemas are in `docs/reference/rex-0.0.1792158932-g88fd41/llms.txt`.

The event log is not a replay log for the hub's application state. SQLite stores operation and recovery state.
Lab tooling reads `events.jsonl`. Production behavior emits events but does not recover state by replaying that file.

The status source is `com.superlogical.terminal.program_status`, as described by Rex's
[program status protocol](https://www.superlogical.com/rex/docs/build/program-status.md).
The hub subscribes to changes and reads the current record when an agent joins or rebinds.
The hub treats a removed record as unknown readiness. This cache preserves the application's wake policy.

The `session.own_block` contract (Rex `0.0.1792158932-g88fd41` reference) says that ownership moves between connected client candidates.
An ownership change can refresh a client view. It cannot prove which agent owns a terminal process.
The hub's identity scan therefore cannot be replaced with `block_owner_changed` alone.

In that reference, the `session.new_window` schema creates block templates. It does not accept an existing block ID as a layout leaf.
`session.move_block` requires an anchor block.
No atomic layout restoration method appears in that reference.

Rex documents action and keyboard ownership in its
[custom action guide](https://www.superlogical.com/rex/docs/customize/actions.md) and
[keyboard shortcut guide](https://www.superlogical.com/rex/docs/customize/keyboard-shortcuts.md).
The renderer's local editor state is outside Rex's workspace keymap.
`client.set_script` affects the caller's connection; it does not install keys in another application client.
