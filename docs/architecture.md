# Architecture

Rex runs every process and draws every terminal. LazyRex is a Rex client: the hub. It adds agent
identity, messaging, plugins with their own databases, views in Rex panes, and links between hubs on
other machines. The details of each part are in `docs/hub.md`. The Rex behavior that LazyRex
depends on, with the Rex build of each observation, is in `docs/rex-protocol.md`.

## Who owns what

| Concern | Owner | Notes |
|---|---|---|
| Terminals (PTYs), drawing, scrollback | Rex | |
| Sessions, windows (tabs), blocks (panes), the Rex app | Rex | LazyRex opens and closes its own panes through the Rex API. |
| Remote access to a Rex server | Rex | Rex host labels (`rex -S <host>`). |
| Agent state (idle, working, done, blocked) | Rex | Program status (`program_status_changed`). The hub reads it; the agent hooks report only the conversation identity. |
| Agent identity | LazyRex | The hook reports a conversation; the hub binds it to the reporter's pane by process ancestry. |
| Plugins: start, stop, supervision, versions | LazyRex | Each plugin runs in its own Rex pane. |
| Plugin data | LazyRex | One SQLite database for each plugin, run by the hub (`docs/sql.md`). |
| Views: forms, panels, threads | LazyRex | The renderer `lazyrex-view` runs in a Rex pane and draws the view tree of a plugin. |
| Messaging, profiles, templates, tunnels, worktrees, automations | Plugins | Each one has its own repository. |
| Links between hubs | LazyRex | Peer hubs (`docs/hub.md`, "Peer hubs (federation v1, 2026-10-23)"). |

## Processes

- **The Rex server.** The user's normal Rex server. When it stops, every terminal stops, the hub
  with them.
- **The hub** (`lazyrex-hub`). It runs in a Rex pane (**LazyRex: Start Hub**, or the autostart
  that setup can install), usually under the restart wrapper (`-restart`). The wrapper starts the
  hub again after a failure (`docs/hub.md`, "Restart wrapper"). The hub connects to the Rex server
  as a client with the label `lazyrex-hub`. A second client of the same hub, labeled `lazyrex`,
  carries only the actions that agents may call. The hub keeps its state in `hub.db` in its state
  directory. A hub restart keeps the terminals: the hub reads Rex state again and finds its panes by label.
- **Plugins.** Each plugin is a Lua script. The hub starts it with `rex do` in a pane labeled
  `lazyrex-plugin:<name>`. The plugin registers with the hub and calls hub actions through Rex
  (`client.perform`). The SDK in `sdk/lua` wraps those calls (`docs/plugin-sdk.md`).
- **Renderers.** Each view runs `lazyrex-view` in a pane (`lazyrex-view:<id>`, or
  `lazyrex-form:<id>` for a form or panel). The renderer gets the view tree from the hub and sends
  the user's actions back (`docs/hub.md`, "Renderer contract").
- **Agents.** Claude Code and Codex run in Rex panes. The hub starts them from profiles, or the
  user starts them by hand. Each agent's hook reports its conversation to the hub.

```text
            Rex server (PTYs, sessions, panes, app)
     ^            ^               ^                ^
     | client     | rex do        | pane           | pane
  lazyrex-hub   plugin.lua    lazyrex-view       agent CLI
  (hub.db,      (one pane      (one pane          (hook -> hub.identity.report;
   plugin dbs)   each)          each view)          calls through rex -C lazyrex)
     |
     +-- rex CLI --> a peer's Rex server --> the peer's lazyrex-hub
```

## Calls and identity

- Every call to the hub goes through Rex as a client action (`client.perform`). Rex tells the hub
  which client called. For a call from a process in a pane (an agent, a plugin), the hub walks the
  caller's process ancestry to the pane's process (`docs/hub.md`, "Bindings").
- Palette commands are config actions in the Rex config: setup installs the boot file
  `lazyrex_boot.lua`, and the hub writes the palette file `lazyrex_palette.lua`. An action forwards
  to the hub with `client.perform`, and Rex attests the client that ran it.
- This is attribution, not isolation. All agents run as the same OS user, and a process of that
  user can open its own Rex connection.

## Messaging and notices

The Messaging plugin keeps threads in its database. Agents read and send with `messaging.*`
actions. When a message comes for an agent, the plugin sets a notice. The hub writes the notice
text into the agent's pane when the agent's program status is ready (idle or done)
(`docs/hub.md`, "Notices").

## Restart and recovery

- The hub records an intent before each Rex call that creates a pane, and it labels the pane. After
  a restart it finds its panes again by label and checks the process before it adopts one. A
  label is a hint, not proof (`docs/hub.md`, "Reconciler and view behavior").
- An agent whose process ended is detached. **Resume detached agents** starts an agent that the hub
  started again, from its profile's `resume` template. An agent that the user started by hand
  needs a working directory to resume (`docs/hub.md`, "Agent resume").

## Peer hubs

Each hub can name peers in `hub.json`. A hub pulls envelopes (messages that plugins address to a
plugin on another hub) from its peers with the rex CLI
(`rex -S <peer server> -C <peer hub> do hub.peer.pull`); it never pushes data. A hub can also take
its plugins from one peer. See `docs/hub.md`, "Peer hubs (federation v1, 2026-10-23)".

## Limits

- Rex has no API to restore a whole layout at once, no operation keys on create calls, and no
  caller identity on block methods. The hub works around each one (`docs/roadmap.md`, "Requests
  for superlogical"; `docs/REX-BUGS.md`).
- The hub's event log is a record, not a replay log. The hub recovers from `hub.db` and from Rex
  state, not from the log.
