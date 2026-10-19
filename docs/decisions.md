# Decisions

> **Frozen (the maintainer, 2026-10-19).** Decisions now live in the maintainer's knowledge vault.
> Do not add entries here. Record a new decision as the workspace agent guide says. This file
> stays as a read-only copy until the vault migration is done. Then it becomes an index of the
> decision titles. Code and tests cite a decision by its title.

> Note: this document was written during the spike phase. Paths such as `spikes/03-ui-framework/`
> refer to the original research repository. In this repository, that code is at the root
> (`hub/`, `view/`, `plugins/`, `tests/`), and each plugin has its own repository.


Newest first. Each entry: what, why, and status.

## Spawn focus stays with the source tab (2026-10-17)

The coordinator assigned this contract to the Profiles pair for Profiles and Worktrees:

- A user spawn can pass `placement.focus: {source_window_id}`. The ID comes from the form context. No focus field means no focus.
- The exact request stores this field. A retry returns the existing spawn and does not focus again.
- After the first successful open, the hub reads the target session. It focuses the new block only if `active_window_id` still equals `source_window_id`.
- Unknown outcomes and late adoption never focus. A focus failure does not change a successful spawn into a failure.
- Agent callers cannot request focus. Detached spawns cannot request focus.
- This is server focus only. It sends no app action and does not select another session in an app. Rex has no atomic conditional focus; the read and focus call can race a later user action.

Status: implemented for `hub.spawn`. The isolated `TestSpawnFocusLab` covers first focus, replay, default behavior, agent refusal, a tab change during creation, a source in another session, splits, and late open. Templates can reuse `parseSpawnFocus`, `spawnFocusSchema`, and `focusSpawn` for `hub.spawn_window`.

## Proposed (the maintainer, 2026-10-16): the LazyRex UI is TUIs in Rex, and the hub drives the app's view

Superlogical is building iOS and web apps for Rex. LazyRex uses their apps and does not build its own web app.

- The hub has a TUI. Each plugin can have its own TUI. All are Rex panes, so every Rex app shows them.
- The hub is a state machine that decides what the user sees. It changes the layout with server methods (`detach_block`, `move_block`, `focus_block`, layers, zoom) and the app's view with app actions (`session.select`, `client.tab.goto`). Both verified on the real machine (see `rex-protocol.md`, "Driving the Rex app" (and the Rex agent guide, `rex __internal skill`)). This needs Remote Control on in the app, which the maintainer turned on.
- Each plugin gets a Rex session as its home. For messaging: the hub screen has "Messages"; it opens the messaging session; threads are views there; "back" returns to the hub. Cost: Rex has no session hide, so plugin sessions stay in the sidebar. `session.move` can group them.
- The user talks to agents through threads, not by typing in agent panes. This also removes the problem of a notice submitting the user's unfinished text.
- Proposed (Claude): plugins declare views as data (lists, threads, composers, forms), and one Go renderer draws them. Lua plugins then get a UI with no TUI code, and every plugin looks the same. A plugin that needs a custom screen ships its own program in a pane.
- Proposed library: Bubble Tea v2 (`charm.land/bubbletea/v2` v2.1.0), Lip Gloss v2 (v2.0.6), Bubbles v2 (v2.2.1), Huh v2 (v2.0.3) for forms. Versions from the Go module proxy on 2026-10-16. Alternatives: tview v0.42.0 (dated look, last release 2025-08), tcell v3 (too low level alone).
- A message from the user (`user`, shown as "User") has the user's authority. A message from a peer agent does not. `AGENTS.md` must say this before the user messages agents through threads.
- The maintainer's refinement (2026-10-16): the hub's home shows widgets that plugins declare. Entering a plugin opens its session: one tab for the plugin's home page and one tab per thread. Every terminal opens at a route. Tabs follow the declared set and snap back. All apps follow navigation. Enter sends; Shift+Enter adds a new line. Design: `architecture.md`, "UI framework".
- Open: whether `session.select` switches the app to a session it does not show now; touch on iOS (not released).

## 2026-10-16: Codex runs with `--no-daemon` for LazyRex

With the shared app-server daemon, the daemon starts Codex's MCP servers, outside Codex's pane. The hub cannot attribute that shim to a pane and refuses it (observed on the real server). A conversation started under the daemon could not be resumed with `--no-daemon`.

## 2026-10-15: The hub and the gateway are written in Go

The maintainer chose Go. Reasons:

- A remote browser without a tailnet needs a gateway with sockets (see "No tailnet requirement"). `rex do` Lua has no sockets, so some Go is needed anyway. One Go binary for the hub, the gateway and the MCP shim is simpler than two runtimes.
- Go is Rex's language. gopher-lua is Rex's Lua dialect.
- Performance was not the reason.

Gateway transport: WebRTC (pion), as Claude recommended: end-to-end encryption with no third party carrying terminal traffic. Pairing, signaling and TURN are open.

## 2026-10-15: Plugins are Rex Lua scripts that the Go hub manages

The maintainer: the first proof is a Go hub that manages Rex Lua plugins. Each plugin is a `rex do` Lua script in its own Rex pane. The hub spawns and supervises the panes. Plugins and the hub call each other through `client.perform`, so Rex is the bus and attests every caller. There is no plugin host binary. (Rejected: gopher-lua embedded in a Go plugin host.)

## 2026-10-15: Plugins are separate repos

LazyRex is the hub. Compatible plugins live in their own repos.

## 2026-10-15: The project is called LazyRex

A tribute to LazyVim: an opinionated distribution on top of Rex, as LazyVim is on top of Neovim. Superlogical intends to open-source Rex. Check its license and trademark policy when it is public.

## 2026-10-15: Workspace = Rex session

Considered: workspace = tab inside one shared session. A live demo in the macOS app showed the difference: a hidden session leaves an empty header in the sidebar, and a hidden tab disappears. Sessions won because they keep a level of grouping (a workspace can have several tabs). The empty header is cosmetic.

## 2026-10-15: Plugins run in Rex panes

Each plugin process runs in a Rex pane. This isolates it and makes its log visible. The hub supervises it. Plugins talk to the hub on the hub's socket.

## 2026-10-15: The Rex server is the core, and terminals die with it

Rex has no hot handoff: its children stop when it stops. This is accepted because the Rex server is stable and the hub can restart independently.

## 2026-10-16: The hub provides services that plugins call

The maintainer: the hub exposes services that plugins call, for example SQLite storage and calls to network servers. A heavy service can run as a separate Rex process (a Rex client in its own pane) so that it does not block the hub. Rex can host any number of such clients.

- **Storage:** hub-owned SQLite, one namespace for each plugin. The hub derives the namespace from the registered caller; a caller cannot select another namespace. Operations: read, conditional atomic batch (with a revision check), and operation-result lookup. No raw SQL. Codex's proposal, agreed by Claude.
- **Call direction:** the hub answers service requests without calling a plugin inside that handler. For hub-originated work, the hub admits the request, returns, and dispatches outside the handler. So no call cycle can form (each Rex client answers one request at a time).
- **Tradeoff:** while the hub is down, plugins cannot commit durable changes. The failure must be explicit.
- **Plugins:** Rex Lua (`rex do`) is the first plugin runtime. The contract (registration, actions, caller identity, services) does not depend on Lua.
- This supersedes the plugin-host-owned storage proposal below.

## 2026-10-16: Next is a true hub with messaging

The maintainer: the first product goal is a real hub with messaging, so that agents (Claude and Codex in this repo) message each other through it officially, not by typing into each other's panes. See `roadmap.md`, spike 2.

## 2026-10-16: Agent status comes from Rex program status (OSC 7501)

The maintainer: use Rex program status (https://www.superlogical.com/rex/docs/build/program-status) for agent readiness. Facts on Rex 0.0.1792088193-g7a93ec:

- Claude Code 2.1.301 reports its own status with OSC 7501: its binary contains `terminal_program_status` and the feature probe `OSC 7501 ; ?`. Lab: `idle`, `working` and `done` with no setup.
- Codex CLI 0.162.1 (the newest Homebrew version on 2026-10-16) has no OSC 7501 strings in its binary. The maintainer expects native support soon. Until then, Rex's Codex detector (screen rules) produces its records.
- **Update (2026-10-16):** Codex CLI 0.163.0 reports OSC 7501 natively (see `rex-protocol.md`). The hub now trusts Codex's root record, like Claude's. The `lazyrex-codex` hook (`spikes/01-hub-lua-plugins/hooks/`) is removed. If the hub writes a notice while Codex is busy (for example a short `idle` between queued turns), Codex takes the paste and Enter as steering input for the active turn. The maintainer wants this (2026-10-16): messages steer. So the hub also writes notices when the agent's record is `working`, for Claude and Codex. It does not write when the record is `blocked` (Enter could answer the waiting prompt), `error`, or absent.
- Codex: Rex's detector reports `working`, but its `turn_end` rule requires an AM/PM clock (checked in the installed binary). On a 24-hour clock, Rex clears the record instead of reporting `done`. The maintainer's Codex shows a 24-hour clock. So LazyRex launches Codex with a lifecycle hook that reports status itself with OSC 7501 (`spikes/01-hub-lua-plugins/hooks/`). Remove the hook when Rex matches 24-hour clocks.

## Proposed: a plugin host process that owns its plugin's storage

**Superseded (2026-10-16)** by "The hub provides services that plugins call".

Each plugin runs in its own Rex pane, in a **plugin host** process. Proposed by Claude and Codex, 2026-10-15. Not tested.

**Partly superseded (same day):** The Rust/mlua 5.4 host is no longer proposed. The points about storage ownership, one writer, the RPC boundary and the gate still apply to a plugin host in any language.

- The plugin host owns its plugin's database (SQLite). Each plugin's data is scoped to that plugin. So database calls stay local and atomic batches stay local: the host never waits on a hub RPC for a database call. This does not make a handler invocation exactly-once. A plugin can commit locally, then crash or disconnect before it returns the result, so the caller gets "outcome unknown" and must reconcile durable state before it retries (see `architecture.md`).
- Calls that need the hub (events, `entity_publish`, coordination, session spawn, config) go over one full-duplex RPC socket. Neither side may block its read loop while it waits for a reply.
- Each plugin store has exactly one writer: its plugin host. The host keeps atomic batches and revisions.
- (Superseded) This decouples the hub language from the Lua runtime. A Go hub plus a Rust plugin host is a sound process boundary.
- Gate: one real LazyRex plugin must pass through the new socket protocol, and survive a hub restart during a mutation or spawn without duplicate effects, before this is selected. The hub language stays provisional until then.
- Still open: what a host may do while the hub is disconnected (Codex's question). Claude proposes that the host keeps writing its own store, because it is the only writer.

## Option under study: the hub in Rex's own Lua (`rex do`)

The maintainer asked (2026-10-15): can the whole hub be Lua that Rex runs, so nothing needs to compile? Findings (lab, see `rex-protocol.md`, "Rex Lua runtime"):

- For: the most Rex-native option. No build step. Same Lua (gopher-lua 5.1) and API as `init.lua`. `io`, `os.execute` and `io.popen` are available. Each plugin could be a `rex do` script in its own pane, with no plugin host binary.
- **Rex is the RPC bus (verified in a lab).** A `rex do` script declares actions with `client.keymap`, receives `client_perform_requested`, and answers with `rex.call("client.reply", {call_id = ev.call_id, result = ...})`. The caller gets the result. The script keeps state across calls. Agents can call it from a shell: `rex -C lazyrex do lazyrex.send to=codex body=hello`. Each request carries the server-attested requester pid.
  - Correction: an earlier entry said the reply timed out. That was a bug in Claude's probe (the handler read `call_id` from `ctx`, not from the event). Codex's positive control with a Node client, and then a corrected Lua probe, both passed.
- Against, in this build:
  - It has no sockets. Helper processes (`io.popen`, `os.execute`) and file-based designs remain possible, but they add their own costs and are not designed or tested.
  - Handlers run one at a time, and `rex.sleep` / `rex.wait` block the whole script.
  - No JSON, SQLite or `utf8` built in.
  - The browser gateway (pairing, WebRTC) has no direct transport in it. A helper process would have to carry it.
- Not directly a block plugin: a Rex block plugin must serve HTTP on its own socket. A `rex do` script cannot do that without a helper process.
- Network: no native sockets or WebSockets, and no C modules. Outbound HTTP works through `curl` (`io.popen`). Every other client, including a browser, can reach the Lua hub **through Rex** with `client.perform` (lab, anonymous TCP client). So the hub does not need a listener of its own: Rex is the network edge.
- Status: **viable for RPC and messaging**, which is the scope of spike 1. Still open for a Lua hub: storage (files, or the `sqlite3` CLI through `io.popen`), JSON (a pure-Lua library through `require`), caller-pane attribution (walk the requester pid with `ps` through `io.popen`, or match Rex `process` results), the throughput of one-at-a-time handlers, and the browser gateway (it needs a separate process in any design).

## Superseded: hub language (decided above: Go)

Rex is Go. LazyRex does not need a systems language, because Rex does the PTY and terminal work. Candidates:

| Language | For | Against |
|---|---|---|
| **Go** (Claude's recommendation) | Same language as Rex (read and contribute upstream when it is open source). pion/webrtc is a mature WebRTC stack. Official MCP Go SDK. One static binary for hub, plugin host and MCP shim. gopher-lua is the same Lua that Rex uses, so plugins use Rex's Lua dialect | Needs a compile step |
| TypeScript (Bun) | Fastest to write. Official MCP TS SDK. Shares code with the web client. Built-in SQLite. `bun build --compile` makes one executable | WebRTC outside the browser: library choice not evaluated. Lua embedding: not evaluated (wasmoon is one candidate; a separate plugin process could also use another runtime) |
| Rust | Team experience (mlua, rmcp, WebRTC) | The slow, tedious language that we want to leave. No code reuse is planned |
| Lua as the host | Same language as plugins | Needs a host runtime anyway (for example luvit). WebRTC and MCP libraries not evaluated |
| Lua in `rex do` | No compile step. Most Rex native. Rex carries RPC with attested callers (verified in a lab) | No sockets, JSON or SQLite built in. Handlers run one at a time. See "Option under study" above |
| Ruby with Spinel | The maintainer's language | Spinel (Matz's AOT Ruby-to-C compiler, RubyKaigi 2026) is described by its author as experimental with many limitations, and supports a subset of Ruby. Its MCP, WebRTC and socket libraries were not evaluated |

Performance is not a criterion: the hub moves messages between sockets, JSON and SQLite. The criteria are Rex-nativeness, one executable, and libraries (MCP, WebRTC, SQLite, Lua).

Plugins stay in Lua whatever the host language is.

## 2026-10-15: The first spike is hub messaging through a plugin

The maintainer: build the hub and a messaging plugin first, so that agents talk through a real messaging service instead of typing into each other's panes.

## Proposed (2026-10-16): agents reach LazyRex through the MCP shim

Spike 1 passed a full round trip through `lazyrex-mcp`, a stdio MCP server that each agent starts as its child. A Codex in a read-only sandbox could not use the `rex` CLI, because its sandbox blocked the Rex socket. Through the shim it could, because Codex runs MCP servers outside its tool sandbox. So the MCP shim is the agent interface, and agents need no `rex` access of their own. The shim is attribution, not a security boundary: it holds the full Rex API. See `spikes/01-hub-lua-plugins/README.md`.

## 2026-10-16: Reading does not acknowledge; a reply does

Reading a message changes nothing, so a lost read reply or a crashed agent never loses a message. The agent acknowledges with `messaging.ack`. The maintainer: a reply acknowledges the message it answers. `messaging.send {reply_to}` removes that message from the sender's mailbox in the same transaction as the new message. mcp_agent_mail_rust (reviewed 2026-10-16) also separates fetch from acknowledge.

## 2026-10-16: Use Rex's own features where they exist

See `rex-reuse-audit.md`. Plugin schemas live in Rex (`client.keymap`), plugins are detached blocks, clients find the hub with `target.resolve` (only a `connected` candidate counts), and admin actions run through `rex -C lazyrex-hub do`. A newer Rex preview exists (build 1203, Rex 0.0.1792158932-g88fd41, 2026-10-16) than the tested one (build 1173). Not installed: an app update can restart the real server.

## 2026-10-15: No tailnet requirement

The maintainer does not want LazyRex to require a tailnet. Rex has no other authenticated network transport (see `rex-protocol.md`, "Network admission without a tailnet"). So remote browsers go through a LazyRex gateway. The gateway needs sockets, so it cannot be a `rex do` Lua script. It is the one component that needs a real runtime (Go, or Node/Bun). The hub can still be Lua. The gateway's transport is open (see below).

## Proposed (the maintainer, 2026-10-15): LazyRex hosts the web app locally; a plugin exposes it

- LazyRex serves its web app locally (the Go gateway). Nobody hosts a central service.
- For access outside the home, a LazyRex plugin wraps opentunnel (`github.com/anomalyco/opentunnel`) behind an easy interface.
- Stable URL (Codex's source review, `opentunnel.md`, not run): yes, if LazyRex keeps the tunnel profile. A fixed route gives `https://lazyrex.<tunnel-id>.opentunnel.xyz`, reused across restarts. `delete` loses it, so "disable remote access" must use `down`, never `delete`. A custom domain or a bare `lazyrex.opentunnel.xyz` is not supported.
- Visitor TLS terminates in the local opentunnel client; the relay forwards ciphertext. The hosted operator still controls DNS and certificate issuance.
- The tunnel has no visitor auth: anyone with the URL reaches the route. The gateway must do all auth, check Host and Origin, and never expose the raw Rex API.
- WebSockets: inferred from byte forwarding, not tested. Source limits: 256 connections per client, 15-minute idle reset.
- Next check before relying on it: URL stability across restarts, a WebSocket through the tunnel, idle recovery.
- Auth stays in the gateway (Dexter-style login or device pairing). The tunnel only provides reachability.

## Proposed: a paired gateway for remote browsers

The maintainer asked for pairing with a key exchange in front of Rex. Rex has no auth hook. A gateway can do the pairing instead:

- Rex listens only on its Unix socket. No TCP or tailnet listener.
- A LazyRex gateway pairs with the browser (key exchange, for example over a WebRTC data channel). After pairing, it carries separate control and data connections from the browser to the Rex socket, without parsing them.
- The browser still speaks the Rex protocol and requests its own tickets. A ticket binds to the principal, not the process (lab, see `rex-protocol.md`). The gateway's connections are all one Unix-peer principal, so tickets work across them.
- **Access boundary:** Rex sees the gateway's principal, not the browser. Rex has no per-method authorization, so a paired browser gets the complete Rex API. Separate browser identity or method limits would need more work in the gateway.
- Costs: a browser adapter that makes a WebRTC channel act like a WebSocket. Its channel mapping, close and error behavior must be defined; its size is unknown. If the gateway restarts, browser views drop and resync from the snapshot. Run the gateway as its own small process.
- Untested beyond the ticket checks.

## Proposed: the browser connects to Rex directly for terminals

A lab test (Chrome, Rex 0.0.1792088193-g7a93ec) showed that a page on an allowed origin (`--websocket-origin-pattern`) is a full Rex client. It gets its own data tickets and the GHOSTSNP snapshot, with no relay. Rex's hosted web client (`rex.superlogical.com`) is allowed browser WebSockets by the app, which suggests the same model; its behavior is not observed.

- For: the hub leaves the byte path, spike 1b needs no relay, and hub restarts do not interrupt terminals in the browser.
- Against: remote use needs a network path from the browser to Rex. The tailnet (`--tailscale`) is one option. It is not a protocol requirement. The paired gateway above is another.
- Verified only on anonymous loopback TCP in a lab. Authenticated tailnet access is untested.
- Still open: how the browser reaches the hub's own API. Needs the maintainer's approval.

## Decided: Home is an app launcher; each plugin is an app (the maintainer, 2026-10-16)

**Replaced on 2026-10-17** by "One LazyRex session; app mode retired" below.

- Home lives in the session "LazyRex". Each plugin has its own session, "LazyRex: <name>". The plugin may set the name; the hub always adds the prefix. All of an app's tabs stay mounted. This replaces the single session that swapped plugin tabs.
- Opening an app hides every session that LazyRex does not own (app mode). "Exit LazyRex" on Home restores them exactly and selects the first recorded session (Rex does not report which session an app shows).
- Scope: every session LazyRex does not own. Spawn points can narrow it later.
- The hub hides only layouts it can rebuild exactly; otherwise it refuses and hides nothing (Codex's review). The capture is durable before the first detach.
- Details: `spikes/03-ui-framework/README.md`, "App sessions and app mode".

## Proposed (the maintainer, 2026-10-16): agent identity is the conversation; Durable Sessions

The maintainer approved the direction; this design waits for his review. Facts: `agent-identity.md`.

**Identity.**
- An agent is `<kind>:<conversation id>`, for example `claude:7a186ca5-…` or `codex:01a14848-…`. The conversation ID is the one that resume takes. Counter names (`codex-2`) go away.
- `user` stays a fixed identity.
- A new conversation in a pane (`/clear`, fork, new thread) is a new identity. The old identity keeps its mailbox for a later resume. Resume continues the same identity, also in another pane.
- Short unique prefixes resolve (`claude:7a18`); an ambiguous prefix is refused.

**Names.**
- The display name is the Rex tab name, or the pane label when several agents share a tab. Rex reports renames (`window_label_changed`), so names stay current. The user renames in Rex.
- Messages and listings show the ID and the name. A send may use a name; the hub resolves it to one ID at send time and refuses a name that two agents share. A stored message always records IDs.

**Binding a pane to an identity.**
- Tool discovery (`initialize`, `tools/list`) works before identity is known. Startup never waits for a hook.
- Claude: the hub assigns the ID at spawn (`--session-id`). Changes arrive from a SessionStart hook that runs in the pane's process tree. It reports `{session_id, source}` to the hub with the same pid proof that join uses. The MCP shim cannot see `/clear`.
- Codex: the shim reads `_meta.threadId` on each `tools/call` and binds or updates the pane's identity then, after the pid proof. `threadId` is the identity; `sessionId` is not, even where a probe showed equal values. Before the first turn, a Codex pane has a pending identity (SessionStart runs on the first turn, not at thread creation).
- A pane whose conversation is not known yet has a pending identity, bound by pane proof as today.
- Agent panes must not inherit conversation or nesting variables from another agent (for example `CLAUDE_CODE_SESSION_ID`, `CLAUDE_CODE_CHILD_SESSION`). Deliberate launch configuration stays (for example `CODEX_HOME`).
- A hook report or a tool call changes a binding only with process-generation proof: the reporting process descends from the pane's current child (pid and start time). A late report from an exited conversation cannot replace the current binding.

**Durable Sessions.**
- A Durable Session record holds: the Rex session, the threads, and the members (identity, profile, pane, working directory, resume command), plus accessories.
- Each pane in a Durable Session stores its launch spec, so the hub can create it again (the maintainer): the command and its arguments (or the profile and its arguments), the working directory (any path; it need not be a Git worktree), the pane label, and its place in the layout (tab, split position). For an agent, the resume command replaces the original command on resurrect.
- Panes that LazyRex did not spawn (for example a user's shell) can join a Durable Session from what Rex reports: `terminal.process` gives the child's path, argv0 and cwd, and the layout comes from the session view. Untested for argument capture: Rex reports `argv0`, not the full argument list.
- Panes created outside LazyRex join automatically (the maintainer). The hub is attached to the session and gets `block_created`. It records the new pane with what Rex reports, and updates the directory from `terminal.pwd_changed`. A plain shell is restored as a shell in its last directory.
- An agent started by hand in a session pane (for example the user types `claude`) is discovered by its hook. LazyRex installs a SessionStart hook for Claude and Codex, as Rex does for its own hook. The hook runs in the pane, where Rex sets `REX_SESSION` and `REX_BLOCK`. It reports `{kind, conversation id, source}` to the hub. The hub checks the pid ancestry (the environment can be spoofed) and adds the agent to that session's Durable Session. Outside a Rex pane, or with no hub running, the hook exits at once and does nothing.
- A Durable Session is not bound to one repository or worktree (the maintainer). It may span several repositories, or several worktrees of one repository. Each member and accessory records its own working directory. The session lists the directories it uses only as a derived view (for display, and for the worktree lock check).
- Its lifecycle log is append-only: created, member started, resumed, cleared (old -> new identity), renamed, pane moved, exited, suspended, resurrected. This is the event history that `events.jsonl` was proposed for, per Durable Session.
- Closing the Rex session suspends the Durable Session; it does not delete it. Resurrect creates a Rex session again and starts each member in its own recorded directory with its resume command. Delete is an explicit action. Worktree removal stays a separate, guarded step.

**Migration.** Existing mailboxes and thread members (`claude`, `codex`, `codex-2`) map to a conversation identity on that agent's first join after the change. Until then they keep their current names.

**Stage 1 (built 2026-10-16, lab-tested on Rex 1203; contract agreed with Codex).** The hub records identities; messaging still routes by agent ID.
- `hub.identity.report {kind, conversation_id, source, block?}`. `source` is the SessionStart source (`startup`, `resume`, `clear`, `compact`, `fork`), `prompt` (Claude UserPromptSubmit), or `mcp` (a Codex tools/call `_meta.threadId`). `block` is a hint; the pid ancestry of the requester decides the pane.
- Result: `{identity, state: current|unchanged, predecessor, pane: {session_id, block_id}, name, agent_id}`. `agent_id` is null when no hub agent is bound to the pane. Errors: `invalid`, `not_in_pane`, `stale`, `kind_mismatch`. (`generation_mismatch` is reserved. The pane row decides the process generation; an agent's join binding may name an older process until the shim rejoins.)
- Order: hook reports are ordered by the start time of the reporting process. An older reporter than the one that set the current identity is stale. A newer hook report wins, also for a superseded identity, so a later prompt repairs a lost `/clear` or resume. This is hook launch order. It equals event order only if the CLI awaits each hook before the next event: unverified for Claude 2.1.302 and Codex 0.163.0.
- An `mcp` report has no order proof (the shim lives long). It never revives a superseded identity.
- Accepted limits: a report lost during a hub outage stays lost until the agent's next prompt or tool call. For Claude, a message sent at the moment of `/clear` can be attributed to the new identity. A hook that forks and exits orphans its reporter, and the pid proof then fails.
- A profile with `"conversation": "claude"` gets `--session-id <uuid>` at admission. Every agent pane starts through `env -u` for the variables in `agentEnvUnset` (identity.go). Observed in the lab on Rex 1203: a pane inherits the environment of the process that started the Rex server, including another agent's `CLAUDE_CODE_*` variables.
- States: `current`, `superseded` (another conversation replaced it in its pane), `detached` (its process exited, or its pane closed). A detached or superseded identity keeps its record for a later resume.
- Lifecycle log events: `assigned`, `started`, `cleared`, `forked`, `resumed`, `compacted`, `renamed` (tab label, from `window_label_changed`), `exited` (Rex `child_exited`, checked against the recorded process; or the pane runs a new process), `pane_closed`, `kind_mismatch` (an early report of another kind than the spawn expects; it does not stay current), `stale_report`. `hub.identity.list {log?: n}` returns the identities and the last n log entries.
- Lab test: `spikes/03-ui-framework/tests/identity-lab.mjs`.

**Revision (the maintainer, 2026-10-17): the hook gives the kind.** No profile field names the agent CLI, and the hub does not add `--session-id`. Claude's SessionStart(startup) reports the conversation ID within about a second of launch, which also gives the resume ID. Another kind in the same pane is an ordinary transition (the user quits Claude and starts Codex there). `kind_mismatch` is gone. The env cleanup at spawn stays.

**Stage 2, hub side (built 2026-10-17, lab-tested on Rex 1203; shapes agreed with Codex).**
- `hub.identify {pid}` prefers the pane's current identity: `{found, agent: <identity>, identity, legacy: [aliases], block_id}`. A pane needs no `hub.join` once a hook has reported. `hub.spawn` accepts the same callers.
- `hub.agents {all?}` lists "user", the current and detached identities (`all`: also the superseded ones), and the legacy agents that no identity took over. Identity entries carry `kind`, `state` and `aliases`; aliased legacy entries (only with `all`) carry `alias_of`. With `all`, the list is the complete alias map.
- `hub.agents.resolve {to}`: an alias, an exact ID, a unique identity prefix (at least 4 characters after the colon), or a unique tab name among current identities. Errors: `unknown`, `ambiguous` (with candidates).
- Aliases: the first identity report from a pane bound to a legacy agent records `legacy -> identity`, permanently. It follows the conversation, not the pane. "user" is never aliased. The hub never rewrites plugin data; each plugin migrates its own keys (messaging: one batch per alias, with `hub.store.scan`).
- Notices: identities are notice targets with the same readiness rules. A pending notice for an aliased legacy ID goes to its identity while that identity is current and live.
- `hub.store.scan {prefix?, after?, limit?}`: a plugin's own records in key order, paged.
- Observed in the lab: a pane that never joined sends with `rex -C <messaging client> do messaging.send …` from its shell. The plugin attributes the sender to the pane's identity, and the recipient reads it with `messaging.read`. No MCP is involved. Test: `spikes/03-ui-framework/tests/identity-stage2-lab.mjs`.

**Open.** Subagent hooks in Claude (do they carry the parent ID?). Where the Claude hook command lives (Rex already installs agent hooks; LazyRex adds its own). Template format.

## Decided: palette commands open forms; tab commands stay in the tab (the maintainer, 2026-10-17)

- A plugin declares global commands at `hub.register`: `commands = [{id, title, description?, keywords?, route}]`. `id` matches `^[a-z0-9_]+$`. `route` starts with `/forms/`.
- The hub writes one `rex.action` per command (`lazyrex_<plugin>_<id>`, title "LazyRex: <title>", category LazyRex) to the file named by `palette_lua` in the hub config, beside the Rex config. `init.lua` has one line: `pcall(require, "lazyrex_palette")`. The hub reloads the Rex config only when the file content changes. Commands persist in `ui_commands`, so plugin restarts do not rewrite the file. The maintainer approved test changes to their config. Undo: delete `init.lua` and `lazyrex_palette.lua`, then `rex config reload`.
- Reason: only config actions appear in the Rex palette (the maintainer checked the app: hub client actions do not). A new plugin's commands appear with no manual step.
- The palette is not contextual: `rex.action` has no visibility filter, and a custom block type cannot render in the app. So the palette has global commands only (for example "New Thread…"). Thread commands (rename, members, archive) stay in the thread view: a menu button, right-click and F2 (Codex's renderer menu node). Requests 10 and 11 in `roadmap.md` would change this.
- A palette run calls `hub.palette.run {command, ctx}` on the connected `lazyrex-hub`. The hub takes the plugin and route from its own command table, never from ctx. It checks `ctx.session_id` and `ctx.block_id` against Rex, opens the form, and returns `select`. The Lua action queues `session.select` on the app that ran it.
- A form is a renderer in a focused split on the right of the source pane (`session.new_split`, before ratio 0.55), in the window of the source pane. Not a floating layer: the app draws floating layers translucent, and Rex has no option for layer opacity (the maintainer's screenshots, 2026-10-17; `rex-protocol.md`, "Floating layers in the app"). A split keeps the source pane and its data, but not its size: the source pane gets 55% of its width while the form is open. A zoomed source pane loses its zoom when the form opens (Rex clears zoom on a focus change), and the hub does not restore it (lab, Rex 1203). The hub focuses the form again during about 1.75 s if focus goes back to the source pane. This is a workaround: inferred, the app gives focus back to the palette's pane when the palette closes. A focus change to any other pane stops it.
- The form is a view row with tab `@form` and a stored context `{session_id, window_id, source_view_id, source_route}`. The plugin gets it as `args.context` on every render and action. Nothing is recovered from focus later. Forms are not tabs: the tab reconcile, tab rename and crash counts skip them.
- View action results: `{form: "/forms/…"}` opens a form (from a form: with the same context), `{close_form: true}` closes the form that ran the action, and `{navigate}` from a form closes it after the navigation. The hub records these effects with the operation result in one write (`view_ops.effect_state`), runs them in order (navigate, open, close), and keeps progress, so a retry or a restart repeats no unsafe step. A failed effect leaves the form open; `hub.view.operation` reports `effects: failed` with the error, and `hub.view.effect.retry` runs only the effects again. Cancel (Escape, Ctrl+Q) exits the form renderer; its pane closes itself.
- Lab test: `spikes/03-ui-framework/tests/palette-form-lab.mjs` with `tests/formprobe.lua`.


## Thread membership and paginated history (2026-10-17)

The maintainer chose full history access for new group members. Agents must request history
in pages, so a thread does not fill their conversation context. The proposed API
is `messaging.history {thread, before?, limit?}` with a default of 20 and a maximum
of 50 messages per page. It returns newest messages first and an older-page cursor.

Membership changes affect future delivery. New members receive no unread backfill.
Removed members retain already delivered mail, but cannot page group history.
Adding members to a direct thread creates a separate group and preserves the
direct thread. The built-in team group keeps dynamic membership.

The palette contains global commands only. Thread-specific actions belong in the
thread menu. The menu has a visible button, a right-click action, and an F2 shortcut.

## Thread home sessions (2026-10-17)

The maintainer chose one home session per thread. A thread created from a form uses the
form's source session. Creating an existing direct thread preserves its first
home session. Agent-created threads and older threads without a home keep the
Messages placement. Membership changes do not move an existing thread.

The plugin stores the home with the canonical thread identity. The creation
transaction writes both the home and the saved operation result. Old operation
replays preserve their original result and placement.

The plugin declares `session_id` and `revision` on each thread tab. The revision
is the newest message ID, or an empty string before the first message. The hub
records this revision when the user closes a tab. A different revision reopens
the tab without focus. Read, acknowledgment, rename, and restart do not change
this revision. Explicit navigation opens and focuses the thread.

`Threads…` lists threads for the source session. The plugin uses
`hub.ui.describe.default_session_id` to include threads without a home in the
Messages session. Archive remains separate from closing a tab.

Status: the plugin implementation passed controlled Lua tests. Combined hub tests
are pending.

The hub keeps the user's tab order in a home session. Its lab test is
`spikes/03-ui-framework/tests/hometabs-lab.mjs`.

The maintainer chose to recreate a missing home session with its original name when a
thread must appear. The hub records the replacement and returns `home_redirects`
from `hub.ui.describe`. The plugin retains the stored home and uses the redirect
to list the thread in the replacement session.

The hub can use an existing session with that name. If the hub never learned the
name, it uses the default session. A closed tab does not recreate its session.
Only a definite `session_not_found` counts as absence; uncertain responses move
no tabs.

A row from before revision tracking has `rev_known = 0`. Its first declaration
sets the baseline without reopening a closed tab. A new empty thread has a known
empty revision. Its first message must reopen the closed tab.

## Decided: Session templates (Templates pair and coordinator, 2026-10-17)

Goal (the maintainer): "Start Session…" starts a saved template; "Save as Template…" stores a session's tabs relative to its common root; a template does not depend on a directory.

- **Owner.** The plugin `templates` (`plugins/templates.lua`): capture, forms, the Templates panel, and the adapter to the hub. The start engine (`plugins/templates_run.lua`, Codex) owns durable runs. The plugin loads it with `dofile` and its own absolute path (`debug.getinfo(1, 'S').source`, observed on Rex 1203).
- **Storage.** The plugin's hub store: `template/<hex of name>`. Names are unique. A save is one revision-checked batch with its operation result.
- **Format v1.** `{v = 1, name, tabs = {{title, layout}}}`. A leaf is `{launch = {profile, args?} | {shell = true}, dir, label?}`. A split is `{direction = horizontal|vertical, ratio, before, after}`, with ratio = the before share (Rex's own shape). `dir` is `.` or a relative path without `.`, `..` or empty segments. No absolute directories: a pane outside the root must be left out, or the user picks a wider root.
- **Root.** The save form proposes the git root when every pane is in one repository, else the deepest common parent. The start form proposes the git root of the source pane's directory. The user can change both.
- **Capture.** LazyRex's own panes (labels `lazyrex-*`, except `lazyrex-spawn:`) are left out. Only the tiled layer is captured; floating panes are left out and counted in the form. A pane that the hub spawned keeps its profile and its effective args (`hub.spawn.spec`; args after defaults). A pane whose child is a shell in the foreground is guessed "Shell". Any other pane (a program run directly, or a shell running a program) has no guess: the user chooses a profile, Shell, or Leave out. Rex gives `argv0`, not the arguments, so the hub never guesses a command line.
- **Plain shell.** The built-in profile `shell` (Profiles pair).
- **Start.** One `hub.spawn_window` per tab: one atomic `session.new_window` with the whole tree. Tabs start in order; only an open tab advances. Default target: a new session named after the template (`hub.session.ensure`, one per run).
- **App switch (the maintainer and coordinator, 2026-10-19).** A start into a new session switches the user's Rex app to that session and its first tab, once. The start form stays open ("Starting…", `{status = "running"}`) until the first tab opens, the start stops, or 15 s pass. Then the plugin calls `hub.app.select_session {view_id, session_id, window_id}` from the still-open form. The hub runs the app action `session.select` only when the form's pane (from the view row) is the focused pane of its session's active window and exactly one connected app shows that session. Else it refuses (`not_focused`, `no_app`, `ambiguous_app`), the start still succeeds, and the panel says how to switch by hand ("Go to Session"). The plugin keeps the decision under the start's operation key in `switch/<hex key>`: `attempted` commits before the call, so a crash or a lost answer after it reads as `unknown` and never switches again. A replay, recovery or late first tab never switches. "Add to existing session" never switches (the first new tab gets the focus there). Untested: what `session.select` does in the real app; that check needs the maintainer's approval.
- **Not in v1.** A run whose create answer was lost stays pending with its candidate panes shown, and the user can dismiss it; the hub never adopts accessory panes by label (see roadmap, "Idempotent create operations").
- Lab: `spikes/03-ui-framework/tests/templates-lab.mjs` (end to end), `tests/templates-run.lua` (engine), `hub/cmd/hub/spawnwindow_test.go` (hub).

## Decided: One LazyRex session; app mode retired (the maintainer, 2026-10-17)

- Every plugin shares the LazyRex session. A plugin gets no session of its own. A tab with a home session (`session_id`) still opens in that session.
- Only the home plugin orders the tabs of the LazyRex session; its tabs come first. Other plugins' tabs keep their place, and the hub syncs only their titles.
- Navigation never enters app mode. At start, the hub sets an app-mode cycle that an older build left active to `restoring`, and the existing restore brings back the user's sessions. Legacy views do not retire while a cycle exists, so a failed restore changes nothing else (Codex's review).
- A view in an old `LazyRex: <name>` session retires: the renderer exits only when its draft is saved (`view.retire`), and its tab opens again in the LazyRex session. The hub keeps the old session, because it can hold the user's own panes.
- Tab status: a `hub.ui.tabs` entry may have `status {state, kind?, msg?}`. A view's render may return `tree.status`, which overrides the declared status and leaves the tree. The renderer reads only the snapshot's top-level `status` and writes it with OSC 7501, `app=rex` (the maintainer: "use the rex icon for now").
- Ctrl+Q in a tab view is a user close (`hub.view.close`): the tab stays closed until a new revision or a navigation. Before this, the hub counted the exit as a crash and reopened the tab (the maintainer, 2026-10-17).
- Plugin menus open as forms in the side pane (the maintainer: "menus should open in a slideover panel like the command pallet").
- A new thread asks for its session: the default is the source session; the choices are the open sessions and "New session…". "Move to session…" in the thread menu changes the home. Rex cannot move a pane to another session (`session.move_block` takes one session), so the hub retires the view and opens it in the new session. The draft is kept.
- Hub actions for this: `hub.session.list` (exact IDs and labels); `hub.session.ensure {key, name}`, one session per plugin and key, with the error codes `conflict`, `outcome_unknown` (a create was sent and no success answer came; the hub never adopts a session by its label and never sends a second create) and `session_gone`; `hub.identify` gives the caller's `session_id`.
- Lab: `spikes/03-ui-framework/tests/one-tab-migration-lab.mjs`, phases 1 to 5, passed on Rex 0.0.1792158932-g88fd41.

## Decided: Side panels and the Tunnels plugin (the maintainer, 2026-10-17; not built)

- A plugin declares each entry as surface `panel` (the shared side pane) or `tab` (a dedicated tab). For example, the Threads list, Worktrees and Tunnels are panels; a thread is a tab. Clicking a plugin on Home, or its palette command, opens its landing panel in the side pane, as menus and forms open now.
- Each tab has at most one panel. Opening another panel replaces its content; the hub asks first when the panel has unsaved input. A panel belongs to the tab where it opened.
- Tunnels is its own plugin. "Forward Port…" lists the listening TCP ports of processes in the panes (block method `com.superlogical.terminal.process` gives the child; `ps` gives its descendants; `lsof` gives their listening ports; checked on macOS without root, 2026-10-17), with the current session first and an "Other port…" field. A process outside a pane's process tree (for example a Docker container) is not found (inferred).
- Each forward runs in a detached block in the session of the forwarded process. "Show Logs" moves the block beside the source pane (`session.move_block` works only inside one session); "Hide Logs" detaches it; "Stop" closes it. Closing the session ends the tunnel. Rex does not show a detached block's status on a tab, so the Tunnels panel and the live feed show it.


## Decided: Agents may act on anything (the maintainer, 2026-10-19)

- An agent may create, change, stop and delete anything that a user can, including what another agent or the user created. There is no "agent-created only" rule.
- This covers `hub.stop`, Templates delete and dismiss, `worktree.remove`, `tunnels.stop` and `tunnels.forward`.
- A plugin keeps its other checks: validation, operation key replay and conflicts.
- Reason (the maintainer): agents are trusted to act with care; the restriction costs more than it protects.

## Decided: Agents use the Tunnels plugin fully (the maintainer, 2026-10-19)

- `tunnels.forward` is an exported agent action. Forward Port… names it; its temporary `user_only` reason is removed.
- A forward makes a local port public. The tool description says so: anyone with the URL can reach the port, and the tunnel does not ask visitors to sign in.

## Decided: Replacing a side panel whose renderer is not attached (harness team and coordinator, 2026-10-19)

- Each view records `attach_state`: `never`, `attached`, `claimed` or `unknown`. Rows from an older database get `unknown`.
- A replacement claims a `never` view in one transaction with its move to `opening`. A renderer that attaches after the claim follows the replacement: it freezes, then reads that replacement's state once (`hub.view.replacement`) and resumes if the replacement ended. A read error starts recovery.
- An `attached` or `unknown` view that has no attachment now (for example after a hub restart) gives the conflict "the side panel's renderer is reconnecting; try again in a moment". It can hold unsaved input.
- A cancelled or superseded replacement undoes the claim only when no replacement of that view is still active. Hub start runs the same cleanup.
- Limit: the claim belongs to the view, not to one replacement. While any replacement of the view stays active, the claim stays, and a new replacement gets a conflict. It never gets a second claim.
- Reason: the old rule cancelled every replacement of a new panel whose renderer was still starting ("renderer is not attached"). The hub must not wait for a renderer in its request queue. Reviewed by the harness reviewer through 41c639b.
- Test renderers (`lazyrex-hub testview`) attach again after a hub restart, retry a failed subscribe, and answer `view.release` before their subscribe returns.

## Decided: Templates run grants and size bounds (coordinator, 2026-10-19)

Full text: `Templates/tests/templates-run.md` (Templates ab6bcb5).
- **Grant provenance.** A resumed agent run finds its grant by the plugin-scoped key `run/<id>`. Only `start_core` creates that key, in the initial call or a replay of the run's operation key. The lookup covers the identity set (identity plus legacy IDs) of the run's agent, compares the exact entries, and needs an active grant. It does not compare `grant.agent` with `request.by`: after an identity migration, `request.by` can keep the legacy ID.
- **Size.** Admission refuses a run whose final record would exceed 62 KiB (`invalid_input`, before any write); input IDs are checked by their encoded size. Stored error texts are cut to 512 bytes. Reason: a plugin's own request or reply is limited to 64 KiB (observed; see `rex-protocol.md`).
- **Agent IDs.** An identity or legacy ID over 128 bytes, or with control characters, refuses the agent as a whole. No ID is dropped, because a dropped legacy ID would change the ownership check silently.
