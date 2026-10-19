# LazyRex

LazyRex is a hub for [Rex](https://www.superlogical.com/rex) terminals. It runs plugins (Lua scripts
that Rex runs with `rex do`), gives them a shared store, and draws their views with one Go
renderer in Rex panes. Agents such as Claude and Codex use it to message each other and the user.

Status: early. The design came from a series of spikes; `docs/` keeps their findings.

## Layout

| Path | Contents |
| --- | --- |
| `hub/` | The hub (Go): plugin host, store, view service, agent identity, MCP shim (`cmd/mcp`) |
| `view/` | The renderer (Go, Bubble Tea): forms, lists, thread composer, transcript pane |
| `plugins/lazyrex.lua` | The home plugin: plugins, agents and workers |
| `lib/rex-client.mjs` | A dependency-free Rex control and data client for Node tests |
| `lab/` | `server.sh` starts an isolated Rex server for tests; `lab.mjs` checks the lab socket; `test/` is the Lua test runner and API |
| `tests/` | Hub and renderer tests (most need a lab server) |
| `hooks/`, `skills/` | Agent session hook and the LazyRex skill for agents |
| `docs/` | Architecture, decisions, the Rex protocol notes and the hub design |

## Plugins

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

## Run

Requirements: Rex, Go, and Node 22 or later for the tests.

1. Copy `hub.example.json` and `profiles.example.json`, and set the paths.
2. Start the hub with hot reload: `./dev.sh /path/to/hub.json`. Without `DEV_REAL=1`, the hub
   refuses a real Rex server: use a lab server (`lab/server.sh /tmp/rexlab`) while you develop.

`dev.sh` builds the hub and the renderer, watches the sources, and reloads what changed.

## Tests

Plugin tests are Lua files that run against the real hub on an isolated lab server:
`lazyrex-hub test` in the plugin repository (see [docs/testing.md](docs/testing.md)).

- `cd hub && go test ./...` and `cd view && go test ./...`
- Node tests need an isolated lab server:
  `lab/server.sh /tmp/rexlab`, then `REX_SERVER=unix:///tmp/rexlab/rex.sock node tests/<name>.mjs`.
  Never point tests at your own Rex server.
