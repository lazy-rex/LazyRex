# LazyRex

LazyRex is a hub for [Rex](https://www.superlogical.com/rex) terminals. It runs plugins (Lua scripts
that Rex runs with `rex do`), gives them a shared store, and draws their views with one Go
renderer in Rex panes. Agents such as Claude and Codex use it to message each other and the user.

Status: early. The design came from a series of spikes; `docs/` keeps their findings.

## Install

Requirements:

- Rex, with its server running.
- Go 1.27.2 or later, to build the two binaries.
- git.
- The Claude Code or Codex CLI, for agents.
- Network access. The build downloads Go modules, and git reads the released plugins.
- Access to the [lazy-rex](https://github.com/lazy-rex) repositories on GitHub. The repositories are
  private for now. git must authenticate to github.com for the clone, for setup's tag lookup and for
  the plugin download (for example with `gh auth setup-git`).

Build the hub and the renderer into one directory. Setup needs `lazyrex-view` beside `lazyrex-hub`:

```sh
git clone https://github.com/lazy-rex/LazyRex.git
cd LazyRex
mkdir -p ~/.lazyrex/bin
(cd hub && go build -o ~/.lazyrex/bin/lazyrex-hub ./cmd/hub)
(cd view && go build -o ~/.lazyrex/bin/lazyrex-view .)
```

## Set up

```sh
~/.lazyrex/bin/lazyrex-hub setup
```

Setup lists the files that it would create or change. For each file it shows the diff and asks
`[y/N]`; it writes nothing without `y`, and it backs up a file before it changes it. It writes
`~/.lazyrex/hub.json` (your default Rex server and the latest released plugins), the agent
profiles, the LazyRex palette entries for Rex, and the agent hooks and skill. Details:
[docs/hub.md, "Setup"](docs/hub.md#setup).

- Setup makes a profile only for an agent CLI that is on `PATH`, and it never changes an existing
  `profiles.json`. A CLI that you install later gets no profile by itself.

Then, in this order:

1. Run the `rex -S <server> config reload` line that setup prints. It loads the palette entries.
2. Run `rex skill setup`, as setup prints. It installs Rex's own skill and hooks.
3. Codex: if setup says so, review and trust the new hook in Codex with `/hooks`.
4. Install the plugins of `hub.json`. Setup does not do this:

   ```sh
   ~/.lazyrex/bin/lazyrex-hub plugin sync -config ~/.lazyrex/hub.json
   ```

## Start

In the Rex palette, run **LazyRex: Start Hub**. A pane opens, and the hub runs in it. Closing the
pane stops the hub. **LazyRex: Hub Status** and **LazyRex: Stop Hub** are beside it.

Start Hub runs this command, which you can also run in a terminal:

```sh
~/.lazyrex/bin/lazyrex-hub -config ~/.lazyrex/hub.json -real-server -restart
```

`-real-server` is for your own Rex server, the one that setup found. It must match the server in
`hub.json`: leave it out when `hub.json` names a lab server.

## First agent

- **LazyRex: Start Agent…** opens a form that lists your agent profiles. Choose one and a
  directory; the agent starts in a new pane.
- **LazyRex: New Thread…** and **LazyRex: Threads…** open the messages between you and your agents.

## When something is wrong

- A LazyRex pane shows "No hub: run LazyRex: Start Hub.": this pane found no connected hub on its Rex server.
- **LazyRex: Hub Status** says whether the hub runs.
- The hub's log is `~/.lazyrex/state/hub.log`. "is not installed: run lazyrex-hub plugin sync"
  there means step 4 of "Set up" did not run.

## Developing LazyRex

The rest of this README is for work on LazyRex itself.

### Layout

| Path | Contents |
| --- | --- |
| `hub/` | The hub (Go): plugin host, store, view service, agent identity, MCP shim (`cmd/mcp`) |
| `view/` | The renderer (Go, Bubble Tea): forms, lists, thread composer, transcript pane |
| `plugins/lazyrex/` | The home plugin (plugins, agents and workers) and its tests |
| `lib/rex-client.mjs` | A dependency-free Rex control and data client for Node tests |
| `lab/` | `server.sh` starts an isolated Rex server for tests; `lab.mjs` checks the lab socket; `test/` is the Lua test runner and API |
| `tests/` | Hub and renderer tests (most need a lab server) |
| `hooks/`, `skills/` | Agent session hook and the LazyRex skill for agents |
| `docs/` | Architecture, decisions, the Rex protocol notes and the hub design |

### Plugins

Each plugin has its own repository in [lazy-rex](https://github.com/lazy-rex):
[Messaging](https://github.com/lazy-rex/Messaging), [Profiles](https://github.com/lazy-rex/Profiles),
[Templates](https://github.com/lazy-rex/Templates), [Tunnels](https://github.com/lazy-rex/Tunnels),
[Worktrees](https://github.com/lazy-rex/Worktrees).

Check them out next to this repository. Plugin tests use `../LazyRex` for the client library and
the lab harness:

```text
lazy-rex/
  LazyRex/
  Messaging/
  Profiles/
  ...
```

### Run from the sources

`hub.example.json` and `profiles.example.json` are the developer examples: their plugin sources
are the sibling checkouts. (Setup writes a user's `hub.json` with released plugins instead.)

1. Copy `hub.example.json` and `profiles.example.json`, and set the paths. Each `plugins` entry
   names a source: a plugin directory with `lazyrex.json`, absolute or relative to the config
   file (docs/plugin-sources.md).
2. Start the hub with hot reload: `./dev.sh /path/to/hub.json`. Without `DEV_REAL=1`, the hub
   refuses a real Rex server: use a lab server (`lab/server.sh /tmp/rexlab`) while you develop.

`dev.sh` builds the hub and the renderer, watches the sources, and reloads what changed.

Legacy developer tools: `dev.sh` needs python3, and the Node tests (`tests/*.mjs`, `lab/lab.mjs`,
`lib/rex-client.mjs`) need Node 22 or later. They are being removed. They are not requirements of
LazyRex: install, setup, start and the plugin tests need only Rex, Go, git and an agent CLI.

### Tests

Plugin tests are Lua files that run against the real hub on an isolated lab server:
`lazyrex-hub test` in the plugin repository (see [docs/testing.md](docs/testing.md)).

- `cd hub && go test ./...` and `cd view && go test ./...`
- Node tests (legacy) need an isolated lab server:
  `lab/server.sh /tmp/rexlab`, then `REX_SERVER=unix:///tmp/rexlab/rex.sock node tests/<name>.mjs`.
  Never point tests at your own Rex server.
