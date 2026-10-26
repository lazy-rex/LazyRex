# LazyRex

[![ci](https://github.com/lazy-rex/LazyRex/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/lazy-rex/LazyRex/actions/workflows/ci.yml)

LazyRex is an application layer for [Rex](https://www.superlogical.com/rex) terminals. It lets you
build, install and manage plugins, and it connects hubs on different machines (federation). A
plugin is a Lua script that Rex runs with `rex do`. The hub (`lazyrex-hub`) runs the plugins, and
one Go renderer draws their views in Rex panes.

Status: early.

## What LazyRex gives you

Palette commands start with **LazyRex:** in the Rex palette.

| Feature | What works today |
| --- | --- |
| Build plugins | A plugin is a Lua script with a manifest (`lazyrex.json`). The plugin SDK (`sdk/lua`, `require('lazyrex')`) gives a plugin its actions, views and database ([docs/plugin-sdk.md](docs/plugin-sdk.md)). `lazyrex-hub plugin new <name>` writes a new plugin with a test. `lazyrex-hub test` runs a plugin's tests on an isolated lab server; it needs a LazyRex checkout (`-lazyrex` or `LAZYREX_DIR`). |
| A database for each plugin | Each plugin has its own SQLite database, which the hub runs ([docs/sql.md](docs/sql.md)). |
| Views in Rex panes | Tabs, forms and panels, drawn by one renderer (`lazyrex-view`) in Rex panes. A palette command opens its form or panel beside the pane that has focus. With two devices on one session, that can be the pane the other device focused last. |
| Palette commands | A plugin declares its commands, and the hub writes them into the Rex palette. |
| Manage plugins | **Plugins…** adds, updates and removes plugins. By default, the hub installs managed plugins from their GitHub releases, and a lock file pins each GitHub source to a commit. A plugin from a local directory runs from a read-only copy of that directory, which the hub makes when it starts the plugin. |
| Supervision and recovery | Each plugin runs in its own Rex pane. When a plugin exits by itself, the hub starts it again after a delay (1 s, doubled each time, at most 60 s). After a plugin ran 60 s or more, the delay starts at 1 s again. When you close a plugin pane, the plugin stays stopped. A hub restart keeps the terminals: the hub finds its panes again by their labels. |
| Federation | Each hub names its peers in `hub.json`. A plugin can send a message to a plugin on a peer hub. A hub can take its plugins from a peer. |
| Tools | `lazyrex-hub setup`, `status`, `config` and `autostart` (start the hub with the Rex server). |

## Platform services for plugins

The hub also gives plugins services for the agent CLIs (Claude Code and Codex) that run in Rex
panes. The plugins below use them.

| Service | What works today |
| --- | --- |
| Agent identity | The agent's hook reports its conversation to the hub, and the hub binds it to the agent's pane. The hub finds the calling agent from the caller's process tree, so an agent's calls carry its own identity. This is attribution, not isolation ([docs/hub.md, "Bindings"](docs/hub.md#bindings)). |
| Notices | A plugin can wake an agent: the hub writes the plugin's notice into the agent's pane after the agent is ready (idle or done). |
| Agent resume and restart | **Resume detached agents** starts each detached agent that the hub started again, with its conversation, from its profile's `resume` template. **Restart Claude agents…** and **Restart Codex agents…** stop and resume the agents of one kind. |
| Workers and directory locks | A plugin can start agents, shells and tools (workers) in a directory, as a tab or a split, and stop them. A plugin can lock a directory, and the hub then starts no worker in it. |

## Plugins

These plugins are built on LazyRex. Each one has its own repository. Setup adds five of them to the
managed plugin file, all but Automations; add Automations in **Plugins…**.

| Plugin | What it does |
| --- | --- |
| [Messaging](https://github.com/lazy-rex/Messaging) | Threads between you and your agents: direct threads, replies and group threads, with drafts and unread state. The **Team** thread opens at the hub's start. **New Thread…**, **Threads…**, **Thread Settings…**. |
| [Profiles](https://github.com/lazy-rex/Profiles) | Starts a profile (an agent, a shell or a tool) in a directory, as a tab or a split: **Start Agent…**, **Start Accessory…**. |
| [Templates](https://github.com/lazy-rex/Templates) | Session templates: **Templates…**, **Start Session…**, **Save as Template…**. |
| [Tunnels](https://github.com/lazy-rex/Tunnels) | Port forwards: **Forward Port…**. |
| [Worktrees](https://github.com/lazy-rex/Worktrees) | Git worktrees: **Worktrees…**, **New Worktree…**, **Remove Worktree…**. |
| [Automations](https://github.com/lazy-rex/Automations) | **Automations…**, **New Automation…**. |

The home plugin of this repository (`plugins/lazyrex`) has **Plugins…**, **Agents…** (the agents,
workers and accessories) and **Stop Worker…**.

## How LazyRex uses Rex

Rex does the terminal work. LazyRex uses these Rex features:

| Rex feature | What LazyRex does with it |
| --- | --- |
| Sessions, windows (tabs) and blocks (panes), with labels | It opens plugin panes, forms, agent panes and the LazyRex session. A label finds a pane again after a hub restart. |
| The client API (`rex.control.v1`) | The hub is a Rex client. Agents and palette commands call it with client actions (`client.perform`). |
| `rex do` (Lua scripts) | Each plugin is a Lua script that Rex runs. It calls the hub through Rex. |
| Config actions (`rex.action` in the Rex config) | Setup installs the LazyRex boot entries in the Rex config (`lazyrex_boot.lua`). The hub writes the plugin palette entries (`lazyrex_palette.lua`). These actions forward to the hub, and Rex tells the hub which client ran them. |
| Program status (OSC 7501, `program_status_changed`) | The hub knows when an agent is idle, working or done, so it writes notices and resume prompts at the right time. The agent hooks give only the conversation identity. |
| Block events (`child_exited` and others) | The hub sees when a plugin or a worker exits. |
| Terminal block methods (`write`, `process`, `pwd`, `format`) | The hub writes notices and prompts into panes. It reads a pane's process and directory. Tests read what a pane shows. |
| Host labels (`rex -S <host>`) | A peer in `hub.json` names its Rex server by a host label or an address. |

Rex does these things, and LazyRex does not: the terminals (PTYs), drawing, layout, the Rex app,
and remote access to a Rex server.

## Install

Requirements:

You need Rex, with its server running. Rex is in beta, and access comes from
[Superlogical](https://www.superlogical.com/rex). LazyRex runs on macOS and Linux, arm64 and amd64.

- git. Setup reads the plugin tags, and the hub fetches the plugins at its first start.
- curl, to download the release.
- For agents only: the Claude Code or Codex CLI, installed and run once (logged in) before Start
  Agent. A CLI that was never run stops in its own first-run screens, and the agent waits there.

Install and set up with one line:

```sh
curl -fsSL https://github.com/lazy-rex/LazyRex/releases/latest/download/install.sh | sh
```

The script downloads the archive for your system, checks it against the release's `SHA256SUMS`,
and unpacks it into `~/.lazyrex`: `bin/lazyrex-hub`, `bin/lazyrex-view`, the plugin SDK (the
top-level `.lua` files of `sdk/lua`), and `doc/LICENSE`. Then it runs setup (next section). A failed
download or a failed check stops the script before it unpacks anything. It does not stop or restart
a running hub.

- Read it first: `curl -fsSLO https://github.com/lazy-rex/LazyRex/releases/latest/download/install.sh`,
  then `sh install.sh`.
- Another release: `curl ... | LAZYREX_VERSION=v0.5.0 sh`.
- No setup: `curl ... | LAZYREX_SETUP=0 sh`.

<details>
<summary>Install without the script</summary>

```sh
(
  set -eu
  base=https://github.com/lazy-rex/LazyRex/releases
  version=${LAZYREX_VERSION:-$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/latest")}
  version=${version##*/}
  case $version in v[0-9]*) ;; *) echo "no LazyRex release found" >&2; exit 1 ;; esac
  os=$(uname -s | tr A-Z a-z)
  arch=$(uname -m); case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  archive=lazyrex-$version-$os-$arch.tar.gz
  dir=$(mktemp -d); cd "$dir"
  curl -fsSLO "$base/download/$version/$archive"
  curl -fsSLO "$base/download/$version/SHA256SUMS"
  grep " $archive\$" SHA256SUMS > check
  if command -v shasum > /dev/null; then shasum -a 256 -c check; else sha256sum -c check; fi
  mkdir -p ~/.lazyrex
  tar -xzf "$archive" -C ~/.lazyrex
  ~/.lazyrex/bin/lazyrex-hub version
)
```

Then run `~/.lazyrex/bin/lazyrex-hub setup`.

</details>

## Set up

The install script runs setup. To run it again:

```sh
~/.lazyrex/bin/lazyrex-hub setup
```

Setup lists the files that it would create or change. For each file it shows the diff and asks
`[y/N]`; it writes nothing without `y`, and it backs up a file before it changes it. It writes
`~/.lazyrex/hub.json` (your default Rex server), the managed plugin file
`~/.lazyrex/state/plugins.json` (the latest released plugins), the agent profiles, the LazyRex
palette entries for Rex, and the agent hooks and skill. Details:
[docs/hub.md, "Setup"](docs/hub.md#setup).

- Setup makes a profile only for an agent CLI that is on `PATH`, and it never changes an existing
  `profiles.json`. A CLI that you install later gets no profile by itself.
- Setup installs no plugin. At its first start, the hub fetches each plugin of the managed file
  with git and starts it. An existing managed file keeps your own plugins: setup does not change
  it. You add and remove plugins later in **LazyRex: Plugins**.

Then:

1. Always: run the `rex -S <server> config reload` line that setup prints. It loads the palette
   entries.

The other steps depend on your answers to setup and on what it printed. Setup offers the skill
install; trusting a Codex hook is always yours:

2. Setup also offers `rex skill setup` for the agents that it found: Rex's own skill and hooks. If
   you answered no, run the command that setup prints.
3. Codex: if setup says so, review and trust the new hook in Codex with `/hooks`.
4. If setup printed `Install the plugins: ...`, run that command.

Go to Session and Insert Path ask the Rex app to act. The app does that only with Remote Control
on: in the Rex app, open Settings, Rex Server, and turn on Remote Control. Nothing checks the
setting ahead of time. `lazyrex-hub status` shows what the hub last saw the app do.

## Start

In the Rex palette, run **LazyRex: Start Hub**. A pane opens, and the hub runs in it. Closing the
pane stops the hub. **LazyRex: Hub Status** and **LazyRex: Stop Hub** are beside it. The commands of
the plugins, such as **Plugins…**, are in the palette. With the default plugins, the Messaging plugin
opens one tab at the hub's start: the thread of the **Team** group (its messages over a composer).

Start Hub runs this command, which you can also run in a terminal:

```sh
~/.lazyrex/bin/lazyrex-hub -config ~/.lazyrex/hub.json -real-server -restart
```

`-real-server` is for your own Rex server, the one that setup found. It must match the server in
`hub.json`: leave it out when `hub.json` names a lab server.

## Next steps

- **LazyRex: Plugins…** lists the plugins. It adds, updates and removes them.
- `lazyrex-hub plugin new <name>` writes a new plugin with a test. The SDK is in
  [docs/plugin-sdk.md](docs/plugin-sdk.md).
- With the Profiles plugin, **LazyRex: Start Agent…** opens a form that lists your agent profiles.
  Choose one and a directory; the agent starts in a new pane.

## When something is wrong

- A LazyRex pane shows "No hub: run LazyRex: Start Hub.": this pane found no connected hub on its Rex server.
- **LazyRex: Hub Status** says whether the hub runs, and lists the plugins that the hub refused.
  The Start Hub pane lists them after "ready".
- The hub's log is `~/.lazyrex/state/hub.log`.
- A plugin that "is not installed" in Hub Status, in the Start Hub pane or in the log: the hub
  installs it at its next start or reload. A plugin with a lock entry that "does not match", or a
  locked commit that "cannot be trusted": update it in **LazyRex: Plugins…**.

## Developing LazyRex

The rest of this README is for work on LazyRex itself.

### Layout

| Path | Contents |
| --- | --- |
| `hub/` | The hub (Go): plugin host, SQL databases, view service, agent identity |
| `view/` | The renderer (Go, Bubble Tea): forms, lists, thread composer, transcript pane |
| `plugins/lazyrex/` | The home plugin (plugins, agents and workers) and its tests |
| `lab/` | `server.sh` starts an isolated Rex server for tests; `test/` is the Lua test runner and API |
| `tests/` | Hub and renderer tests (most need a lab server) |
| `hooks/`, `skills/` | Agent session hook and the LazyRex skill for agents |
| `docs/` | Architecture, decisions, the Rex protocol notes and the hub design |

### Plugins

Each plugin has its own repository in [lazy-rex](https://github.com/lazy-rex):
[Messaging](https://github.com/lazy-rex/Messaging), [Profiles](https://github.com/lazy-rex/Profiles),
[Templates](https://github.com/lazy-rex/Templates), [Tunnels](https://github.com/lazy-rex/Tunnels),
[Worktrees](https://github.com/lazy-rex/Worktrees),
[Automations](https://github.com/lazy-rex/Automations).

Check them out next to this repository. Plugin tests use `../LazyRex` for the client library and
the lab harness:

```text
lazy-rex/
  LazyRex/
  Messaging/
  Profiles/
  ...
```

### Build from source

This needs Go 1.27.2 or later. `scripts/release-build.sh` makes the same archive as a release, so
the binaries and the SDK stay together. A version other than `dev` needs a clean checkout. The block
stops at the first failure, and each build writes to a new directory.

```sh
(
  set -eu
  git clone https://github.com/lazy-rex/LazyRex.git
  cd LazyRex
  os=$(uname -s | tr A-Z a-z)
  arch=$(uname -m); case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  out=$(mktemp -d)
  TARGETS=$os/$arch scripts/release-build.sh dev "$out"
  mkdir -p ~/.lazyrex
  tar -xzf "$out/lazyrex-dev-$os-$arch.tar.gz" -C ~/.lazyrex
  ~/.lazyrex/bin/lazyrex-hub version
)
```

Without `TARGETS`, the script builds all four archives, as CI does on a tag
(`.github/workflows/release.yml`).

### Run from the sources

`hub.example.json` and `profiles.example.json` are the developer examples: their plugin sources
are the sibling checkouts. (Setup writes a user's managed plugin file with released plugins
instead.)

1. Copy `hub.example.json` and `profiles.example.json`, and set the paths. Each `plugins` entry
   names a source: a plugin directory with `lazyrex.json`, absolute or relative to the config
   file (docs/plugin-sources.md).
2. Start the hub with hot reload: `./dev.sh /path/to/hub.json`. Without `DEV_REAL=1`, the hub
   refuses a real Rex server: use a lab server (`lab/server.sh /tmp/rexlab`) while you develop.

`dev.sh` builds the hub and the renderer, watches the sources, and reloads what changed.

`dev.sh` starts a hub from a source checkout (the live hubs use it). It needs a POSIX shell and Go.
It is not a requirement of LazyRex: install, setup and start need only Rex, git and curl (and an
agent CLI for agents). A build from source also needs Go.

### Tests

Plugin tests are Lua files that run against the real hub on an isolated lab server:
`lazyrex-hub test` in the plugin repository (see [docs/testing.md](docs/testing.md)). It needs a
LazyRex checkout (`-lazyrex` or `LAZYREX_DIR`).

- `cd hub && go test ./...` and `cd view && go test ./...`
- The lab tests start their own lab servers (`LAZYREX_LAB=1`; the exceptions are in
  [docs/testing.md](docs/testing.md)). Never point tests at your own Rex server.
