# LazyRex

LazyRex is a hub for [Rex](https://www.superlogical.com/rex) terminals. It runs plugins (Lua scripts
that Rex runs with `rex do`), gives each one a SQL database, and draws their views with one Go
renderer in Rex panes. Agents such as Claude and Codex use it to message each other and the user.

Status: early. The design came from a series of spikes; `docs/` keeps their findings.

## What LazyRex adds to Rex

- Your agents (Claude Code and Codex) and you message each other in threads. An idle agent gets a
  notice in its pane when a message comes.
- Agents that the hub started come back after a restart: **Resume detached agents** continues each
  conversation in a new pane.
- Plugins add commands, forms and panels to Rex. Each plugin has its own SQL database.
- One hub on each machine, and the hubs connect: messages and agents reach across machines.
- Profiles start agents, shells and tools in a directory, as a tab or a split.

## Features

Palette commands start with **LazyRex:** in the Rex palette.

| Feature | What works today |
| --- | --- |
| Agent identity | The agent's hook reports its conversation to the hub, and the hub binds it to the agent's pane. The hub finds the calling agent from the caller's process tree, so an agent's calls carry its own identity. |
| Messaging | Direct threads, replies and group threads, with drafts and unread state. The **Team** thread opens at the hub's start. Commands: **New Thread…**, **Threads…**, **Thread Settings…** (Messaging plugin). |
| Notices | When a message comes for an agent, the hub writes a notice into the agent's pane after the agent is ready (idle or done). |
| Agent resume and restart | **Resume detached agents** starts each detached agent that the hub started again, with its conversation, from its profile's `resume` template. **Restart Claude agents…** and **Restart Codex agents…** stop and resume the agents of one kind. |
| Profiles and workers | **Start Agent…** and **Start Accessory…** (Profiles plugin) start a profile in a directory. **Agents…** lists the agents; **Stop Worker…** stops a worker. A plugin can lock a directory, and the hub then starts no worker in it. |
| Forms and panels | A palette command opens its form or panel beside the pane that has focus. With two devices on one session, that can be the pane the other device focused last. |
| Plugins | **Plugins…** adds, updates and removes plugins. By default, the hub installs managed plugins from their GitHub releases, and a lock file pins each GitHub source to a commit. A plugin from a local directory runs from a read-only copy of that directory, which the hub makes when it starts the plugin. One hub can take its plugins from another hub. |
| Launch plugins | Messaging, Profiles, Templates (session templates: **Templates…**, **Start Session…**, **Save as Template…**), Tunnels (**Forward Port…**), Worktrees (**Worktrees…**, **New Worktree…**, **Remove Worktree…**) and Automations (**Automations…**, **New Automation…**). Setup installs all of them except Automations; add Automations in **Plugins…**. |
| Peer hubs | Each hub names its peers in `hub.json`. A plugin can send a message to a plugin on a peer hub, so threads and agents reach across machines. |
| Tools | `lazyrex-hub setup`, `status`, `config` and `autostart` (start the hub with the Rex server). The plugin SDK is in `sdk/lua`, and `lazyrex-hub test` runs a plugin's tests on an isolated lab server; it needs a LazyRex checkout (`-lazyrex` or `LAZYREX_DIR`). |

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
- The Claude Code or Codex CLI, for agents: installed, and run once (logged in) before Start Agent.
  A CLI that was never run stops in its own first-run screens, and the agent waits there.

Each release has one archive for each system, and `SHA256SUMS`. The archive unpacks into
`~/.lazyrex`: `bin/lazyrex-hub`, `bin/lazyrex-view`, the plugin SDK (the top-level `.lua` files of `sdk/lua`), and
`doc/LICENSE`.
Download the latest release, check it, then unpack it. Paste the whole block: it stops at the
first failure.
To install another release, set `LAZYREX_VERSION` (for example `LAZYREX_VERSION=v0.5.0-rc.1`)
before the block.

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

The last line prints the version, for example `v0.4.1 (commit 25f8c7d…)`. A failed download, a
missing checksum line or a failed check stops the block before it unpacks anything. These steps are
for a first installation; they do not stop or restart a running hub.

## Set up

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
pane stops the hub. **LazyRex: Hub Status** and **LazyRex: Stop Hub** are beside it. With the default
plugins, the hub also opens one tab at its start: the Messaging thread of the **Team** group (its
messages over a composer). The other LazyRex commands, such as **Agents…** and **Stop Worker…**,
are in the palette.

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
It is not a requirement of LazyRex: install, setup and start need only Rex, git, curl and an agent
CLI. A build from source also needs Go.

### Tests

Plugin tests are Lua files that run against the real hub on an isolated lab server:
`lazyrex-hub test` in the plugin repository (see [docs/testing.md](docs/testing.md)). It needs a
LazyRex checkout (`-lazyrex` or `LAZYREX_DIR`).

- `cd hub && go test ./...` and `cd view && go test ./...`
- The lab tests start their own lab servers (`LAZYREX_LAB=1`; the exceptions are in
  [docs/testing.md](docs/testing.md)). Never point tests at your own Rex server.
