# LazyRex

LazyRex is a hub for [Rex](https://www.superlogical.com/rex) terminals. It runs plugins (Lua scripts
that Rex runs with `rex do`), gives each one a SQL database, and draws their views with one Go
renderer in Rex panes. Agents such as Claude and Codex use it to message each other and the user.

Status: early. The design came from a series of spikes; `docs/` keeps their findings.

## Install

Requirements:

You need Rex, with its server running. LazyRex runs on macOS and Linux, arm64 and amd64.

- git. Setup reads the plugin tags, and the plugin sync fetches the plugins.
- The Claude Code or Codex CLI, for agents: installed, and run once (logged in) before Start Agent.
  A CLI that was never run stops in its own first-run screens, and the agent waits there.
- Access to the [lazy-rex](https://github.com/lazy-rex) repositories on GitHub. The repositories are
  private for now, so the download uses the [GitHub CLI](https://cli.github.com) (`gh auth login`),
  and git must authenticate to github.com (`gh auth setup-git`).

Each release has one archive for each system, and `SHA256SUMS`. The archive unpacks into
`~/.lazyrex`: `bin/lazyrex-hub`, `bin/lazyrex-view` and the plugin SDK in `sdk/lua`. Download the
latest release, check it, then unpack it. Paste the whole block: it stops at the first failure.

```sh
(
  set -eu
  version=$(gh release view --repo lazy-rex/LazyRex --json tagName --jq .tagName)
  os=$(uname -s | tr A-Z a-z)
  arch=$(uname -m); case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  archive=lazyrex-$version-$os-$arch.tar.gz
  dir=$(mktemp -d); cd "$dir"
  gh release download "$version" --repo lazy-rex/LazyRex --pattern "$archive" --pattern SHA256SUMS
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
`~/.lazyrex/hub.json` (your default Rex server and the latest released plugins), the agent
profiles, the LazyRex palette entries for Rex, and the agent hooks and skill. Details:
[docs/hub.md, "Setup"](docs/hub.md#setup).

- Setup makes a profile only for an agent CLI that is on `PATH`, and it never changes an existing
  `profiles.json`. A CLI that you install later gets no profile by itself.
- Last, setup asks to install the plugins of `hub.json` (`lazyrex-hub plugin sync`). Answer `y`:
  git fetches each plugin, and setup says `Plugins: installed.` If you answer no, or the sync
  fails, setup prints the exact command to run later.

Then:

1. Always: run the `rex -S <server> config reload` line that setup prints. It loads the palette
   entries.

The other steps depend on your answers to setup and on what it printed. Setup offers the skill and
plugin installs; trusting a Codex hook is always yours:

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
- The hub's log is `~/.lazyrex/state/hub.log`. "is not installed: run lazyrex-hub plugin sync"
  in Hub Status, in the Start Hub pane or in the log means that the plugins are not installed: run
  `~/.lazyrex/bin/lazyrex-hub plugin sync -config ~/.lazyrex/hub.json`, then **LazyRex: Stop Hub**
  and **LazyRex: Start Hub**. (Start Hub alone is refused while the first hub runs.)

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
are the sibling checkouts. (Setup writes a user's `hub.json` with released plugins instead.)

1. Copy `hub.example.json` and `profiles.example.json`, and set the paths. Each `plugins` entry
   names a source: a plugin directory with `lazyrex.json`, absolute or relative to the config
   file (docs/plugin-sources.md).
2. Start the hub with hot reload: `./dev.sh /path/to/hub.json`. Without `DEV_REAL=1`, the hub
   refuses a real Rex server: use a lab server (`lab/server.sh /tmp/rexlab`) while you develop.

`dev.sh` builds the hub and the renderer, watches the sources, and reloads what changed.

Legacy developer tool: `dev.sh` needs python3. It is being removed. It is not a requirement of
LazyRex: install, setup and start need only Rex, git, an agent CLI and, while the repositories are
private, the GitHub CLI. A build from source also needs Go.

### Tests

Plugin tests are Lua files that run against the real hub on an isolated lab server:
`lazyrex-hub test` in the plugin repository (see [docs/testing.md](docs/testing.md)).

- `cd hub && go test ./...` and `cd view && go test ./...`
- The lab tests start their own lab servers (`LAB=1`; the exceptions are in
  [docs/testing.md](docs/testing.md)). Never point tests at your own Rex server.
