# Testing plugins

A plugin repository contains only Lua: the plugin and its tests. `lazyrex-hub test` runs the
tests against the **real hub**, on an isolated lab Rex server that the command starts and stops.
No test touches your own Rex server.

Requirements: Rex and Go 1.27.2 or later (to build `lazyrex-hub`). The plugin tests need no Node and
no Python.

Status: observed on Rex 0.0.1792158932-g88fd41 (lab), 2026-10-19.

## Run

Build the hub binary in a LazyRex checkout, then run it in the plugin repository:

```sh
cd LazyRex/hub && go build -o ~/bin/lazyrex-hub ./cmd/hub
cd ../../Messaging && lazyrex-hub test
```

Flags:

| Flag | Effect |
| --- | --- |
| `-file send.lua` | Run only this file of `tests/`. |
| `-run <Lua pattern>` | Run only tests whose name matches. The run fails if no test matches. |
| `-timeout 120s` | The time limit for one test file. |
| `-keep` | Keep each lab directory (logs, events, state). The processes stop anyway. Each kept lab gets a `KEEP` marker (see "Lab directories"). |
| `-clean <dir>...` | Remove the given lab directories (`/tmp/lrt-<digits>` of this user, with or without `KEEP`), then exit. Only a lab that the probe proves ended is removed (see "Lab directories"): a live or uncertain lab is refused, with the reason. |
| `-v` | Print the hub, plugin and runner logs. |
| `-lazyrex <dir>` | The LazyRex checkout. Default: `../LazyRex` beside the plugin, `LAZYREX_DIR`, or the checkout of the binary. |

Exit status: 0 when every file passes, 1 when a test or a check fails, 2 for a harness error (for
example a bad manifest).

## Lab directories

Each test file runs in its own lab directory, `/tmp/lrt-<digits>` (a Unix socket path must stay
short). When a file passes, the run removes its lab. When it fails (also by a file timeout, SIGINT
or SIGTERM), the lab stays with a `failed` marker for 24 hours (see below). A lab can hold read-only plugin cache entries; the removal makes each directory writable
first.

- A kept lab gets a file `KEEP`: JSON `{by, why, at, kind}`, for example `{"by": "lazyrex-hub test
  -keep", "why": "tests/send.lua: 3 passed, 1 failed, 0 problems", "kind": "keep"}`. Keep a lab
  only while a message cites it as evidence. A lab that you keep by hand needs a `KEEP` file too.
  Each removal rule keys on the kind:
  - `keep` (`-keep`): only `-clean` removes it (a Go harness test removes the `-keep` labs of its
    own run, see below).
  - `failed` (a failed file without `-keep`, by `lazyrex-hub test`, why `failed: <file>: ...`; or
    a failed Go lab test of the hub): the sweep removes it 24 hours after the marker's `at`, and
    only when the probe proves it absent. A passed file removes its lab at once.
  - `unproved` (the cleanup is not proved): only `-clean` removes it. An unproved cleanup is
    `unproved` on every path, also for a failed file or a failed Go lab test.
  - A marker without a kind (an older binary): by `lazyrex-hub test -keep` is `keep`; by
    `lazyrex-hub test` is `unproved`; any other, and a kind that the binary does not know, is
    protected as `unproved`.
- A lab whose cleanup is not proved stays too, also without `-keep`: a process survived the stop,
  a keeper did not stop cleanly, or the probe after the stop is not "absent". Its `KEEP` marker has
  `{"by": "lazyrex-hub test", "why": "cleanup not proved: <reason>", "kind": "unproved"}`, the file fails with a
  problem that names the lab, and the summary line names it:
  `FAIL tests/send.lua: 3 passed, 0 failed (lab kept: /tmp/lrt-123)`.
- No run removes failure evidence (a `KEEP` marker for an unproved cleanup). A Go harness test may
  remove an ordinary `-keep` lab that it made itself, after it passed. Every other kept lab is
  removed only by `lazyrex-hub test -clean /tmp/lrt-<digits>`, after the review. A lab server in a
  kept lab can still run, and `-clean` refuses that lab: stop that server first, by the `server_pid`
  in its `lab.json` (check that its command names the lab's `rex.sock`), never by a name pattern.
- The probe (`labProbe`) says whether a lab is live, absent or uncertain. Live: a process whose
  command line names the lab (this process and its ancestors do not count), or a server that
  accepts a connection on `rex.sock`. A lab has a name under each name of its root: on macOS
  `/tmp/lrt-1` and `/private/tmp/lrt-1`; a command line matches a name followed by `/`, or the name
  as a whole word. Absent: no `rex.sock`, no such process, and `lab.json` names no server pid or
  one that no longer runs; or `rex.sock` refuses the connection and `lab.json` names a server pid
  that no longer runs.
  Uncertain: every other case, for example a recorded server pid that runs, a refusal without
  `lab.json` (a live server with a full connection queue also refuses on macOS), another connection
  error, or a failed process listing (`ps -eww -o pid=,ppid=,command=`). Only an absent lab is
  removed.
- The sweep: at the start of each run, at most once per hour per user (the stamp file
  `/tmp/lrt-sweep-<uid>`), the command removes each `/tmp/lrt-<digits>` directory of this user that
  is absent and either has no `KEEP` file and is older than 24 hours, or has a `failed` marker
  older than 24 hours. It prints one line for each removal
  and for each uncertain lab, which it keeps. A failed process listing stops the sweep. (24 hours
  for the first release; then 1 hour.)
- A killed run (SIGKILL) cannot clean up. Its lab directory waits for the sweep.
- The lab keepers: the run starts each lab command (the lab server, the hub, the runner) under a
  keeper, the same binary with `LAZYREX_LAB_KEEPER=1`. The keeper leads its own process group and
  starts the command in it, without the keeper variables. The run controls it through two pipes:
  `start` before the command exists, then `stop`; the keeper reports the command's pid, its exit
  status, and the end of the stop. At `stop`, or when the control pipe ends (the run was killed),
  the keeper sends SIGTERM to its group and waits up to 10 s for the group to empty. The keeper's
  exit status: 0 is a clean stop; 125 means no `start` came, and nothing ran; 126 means the command
  did not start; SIGKILL means members remained or the group listing failed, and the keeper killed
  the whole group, itself included. The command's status comes only from the keeper's report. An
  escalation, a keeper that does not end, or a report without the command's status makes the
  cleanup not proved. The lab panes run in their own groups outside the keepers' groups: they end
  with the server, and the run checks them as survivors. So a killed run leaves no lab process;
  only its directory waits for the sweep.
- Still give the tool call that runs `lazyrex-hub test` a time limit above `-timeout`, or run it in
  the background: a killed run gives no result.
- The hub's own lab tests set `LAZYREX_LAB_SWEEP_ROOT` to a test directory: a test never sweeps the
  real `/tmp`.

## What one run does

First the command warns about each Lua multiple assignment where a later value reads an earlier
target, with file and line (`a, b = b, a`; rex-protocol.md, "Lua swap assignment is wrong"). A
warning never fails the run.

1. The command reads `lazyrex.json`.
2. A **parity sweep** runs first, in its own lab (see "Agent parity"). A failure fails the run.
3. For each `tests/*.lua` file, in name order:
   1. The command starts a new Rex server in a new directory `/tmp/lrt-*`. Every child gets the
      lab environment (see "Isolation"). The command writes the lab proof `lab.json`.
   2. It starts the hub (this binary, `-test`) with new state. The config loads the LazyRex home
      plugin, the plugins in `test.plugins`, and the plugin under test.
   3. It waits until each plugin is registered and the agent face lists the plugin's exports.
   4. It runs the file with `rex do lab/test/runner.lua`.
   5. It stops the hub and the server with signals to their own process groups, and checks that
      every process that it saw under them is gone. A survivor fails the file, and the output
      names its pid, start time and command. The harness sends a survivor no signal: on macOS a
      pid can be reused between a check and a signal, so the harness cannot prove that it owns it.
   6. Without `-keep` it removes the lab directory of a passed file, when the probe proves the lab
      ended. A failed file keeps its lab for 24 hours (a `failed` KEEP marker), and its FAIL line
      names it, so the printed log paths still exist. A removal that fails is a problem of the
      file. "Lab directories" below has the rules.
4. Tests in one file share one hub and run in order. Files do not share anything.

The same cleanup runs after a failure, a startup failure, a timeout and an interrupt (Ctrl-C).

## The manifest: `lazyrex.json`

```json
{
  "name": "messaging",
  "main": "messaging.lua",
  "events": "session",
  "notice": "New message. Call messaging_read.",
  "test": {
    "plugins": [{"source": "../Profiles"}],
    "agents": [{"id": "alice", "name": "Alice"}, {"id": "bob", "name": "Bob"}],
    "parity_fixture": "tests/parity_fixture.lua",
    "parity_routes": ["/forms/thread-menu"],
    "env": {"GIT_AUTHOR_NAME": "Lab", "WT_CACHE": "${LAB}/cache"},
    "args": {"worktrees": {"worktrees_root": "${LAB}/worktrees"}},
    "profiles": "tests/profiles.json"
  }
}
```

| Field | Meaning |
| --- | --- |
| `name`, `main` | The plugin name and its main file. Required. `main` is relative, has no `..`, is not a symlink, and resolves inside the manifest directory (docs/plugin-sources.md, section 2). |
| `depends` | Plugin names that this plugin needs. The harness refuses a missing dependency and a cycle, as the hub does. |
| `events` | `session` (default) or `all`: the Rex events that the plugin receives. |
| `notice` | The text that the hub writes into an agent pane for a pending notice of this plugin No control characters (the hub pastes it, then sends CR). Without it, a `notify` op of the plugin fails with code `no_notice`, and nothing in that batch is written. |
| `test.plugins` | Other plugins that the hub loads for the tests: `[{"source": "<dir>"}]`. Each source is a directory relative to the manifest directory, with its own `lazyrex.json`. The names come from those manifests. `lazyrex` is refused (the harness adds the home plugin), and so is a name used twice. |
| `test.agents` | Roster agents in the test hub config. `user` is always there. |
| `test.ui_slot` | The hub config `ui_slot` option: `"split"` or `"layer"` (an experiment; docs/hub.md). Another value fails at the manifest load. |
| `test.parity_fixture` | A Lua file that renders the routes that open only from another view. |
| `test.parity_routes` | Those contextual routes. The fixture must render each one. A route with no query (`/forms/worktree`) is checked by a render of that path with any query or none. A route with a query (`/forms/automation/new?profile=worker`) is checked only by a render of exactly that route. |
| `test.env` | Variables added to the lab environment. See "Isolation". |
| `test.args` | Plugin args (hub config `plugins[].args`, `rex.args` in the plugin), keyed by plugin name: the plugin under test, a `test.plugins` entry or `lazyrex`. Keys match `[a-z0-9_]+`; `hub_client_id`, `instance_id`, `plugin_session`, `main` and `lazyrex_sdk` are refused. |
| `test.files` | Plugin args for one test file: `{"gallery.lua": {"args": {"<plugin>": {"k": "v"}}}}`, keyed by the file's base name (the form of `-file`). For that file's lab, these args go over `test.args`; other files do not get them. The rules of `test.args` apply. An entry can also have `prompts`: a list of plugin names, or `false` for none. For that file's lab it replaces `test.prompts` (the names follow the `test.prompts` rules). The parity sweep uses `test.prompts`. |
| `test.watch_roots` | Plugin watch roots (hub config `plugins[].watch_roots`, docs/hub.md "File watches"), keyed by plugin name: the plugin under test, a `test.plugins` entry or `lazyrex`. Each path starts with `${LAB}/` and stays in the lab. The harness creates each directory before the hub starts. |
| `test.prompts` | The plugins that get the `prompts` grant (hub config `plugins[].prompts`, docs/hub.md "Agent prompts and acting as the user"): the plugin under test, a `test.plugins` entry or `lazyrex`. |
| `test.profiles` | A profile file, relative to the manifest dir. The harness copies it to `LAB/profiles.json`, the hub's profile file. A configured file that is missing is an error. Without it, the hub has only the builtin shell profile. |

The file is one JSON object of at most 65536 bytes. Unknown fields are refused. The harness owns the rest of the hub config (`server`, `state`,
`rex`, `view`, `home`, `agents`, `plugins`, `palette_lua`, `profiles`,
`test_roots`). The manifest cannot set them.

## Isolation

The harness gives the lab server, the hub, the runner and every process that they start (plugins
and workers are Rex blocks, so they inherit the server environment) a private environment:

| Variable | Value |
| --- | --- |
| `PATH` | Copied from the harness. |
| `HOME` | `LAB/home`. It is also the working directory of the server, the hub, the runner and the plugins, and the default `cwd` of a worker (a profile or a spawn can set another `cwd` inside the roots). |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME` | `LAB/home/.config`, `LAB/home/.local/share`, `LAB/home/.local/state`, `LAB/home/.cache` |
| `TMPDIR` | `LAB/tmp/` |
| `REX_SERVER` | The lab server. |
| `GOCACHE`, `GOMODCACHE` | The harness's `go env` values, when `go` is on `PATH` (the one exception below). |
| `test.env` | As the manifest gives them. |

No other variable is copied, with one exception: `GOCACHE` and `GOMODCACHE`. The harness runs
`go env GOCACHE GOMODCACHE` once per run and passes both values, so a Go build in a lab reuses the
user's caches. Without them, each lab built from a cold cache under its own HOME, and Go's
read-only module files stayed in the lab directory. Go locks and verifies both caches itself, and
both are content-addressed, so a normal lab build cannot corrupt them. A relative value, or one
under `/tmp` or the system temporary directory, is not passed; the run prints one line that says
why. `GOPATH`, `GOFLAGS`, `GOTOOLCHAIN` and `GOENV` are never passed: the lab HOME stays isolated,
and a lab's `go env -w` writes only the lab's own file. The harness runs `go env` once, in the caller's environment, and
Go can write its telemetry dir there (`go/telemetry` in the caller's config dir). `LANG`, `TERM` and similar variables are not set: Rex and the self-tests
do not need them (observed on the Rex build 88fd41c of 2026-10-16). If a tool needs one, set a fixed value in `test.env`.

Rules for `test.env` and `test.args`:

- `test.env` cannot set `PATH`, `HOME`, `TMPDIR`, `LAZYREX_AGENT`, `GOCACHE`, `GOMODCACHE`, any
  `XDG_*` or any `REX_*`.
- A value can name `${LAB}` (the lab dir) and `${PLUGIN}` (the dir of `main`). Any other
  placeholder is an error. `~` is not expanded and is refused.
- A value with a `/`, or one that starts with `.` or `~`, is a path. After expansion a path must be
  absolute, must have no `..` component, and must resolve inside the lab, the plugin dir or the
  manifest dir. Symlinks resolve on the longest existing prefix, so a symlink escape fails.
  `LAB-other` is not inside `LAB`. A relative path such as `cache/../../x` is refused.
- Exception: a regular executable file in a `PATH` directory (a tool, for example
  `/opt/homebrew/bin/git`).
- Other values (for example `Lab User`) are passed as they are.

In test mode the hub config has `test_roots` (the lab, the plugin dir and the manifest dir). The
hub then refuses a profile whose env sets a reserved key or names a path outside the roots, and
a profile `cwd` outside the roots. A spawn `cwd` must also stay in the roots. The default `cwd` is
`HOME`, which is the lab home.

The lab config also has `test_source_runs` with the plugin dir (docs/plugin-sources.md, section 4,
"Source runs"). The plugin under test runs from its source dir, not from a snapshot. A plugin that
writes into its own dir passes its suite but fails in a release, where the dir is read-only. The
home plugin and the `test.plugins` dependencies run from snapshots, as in a release.

Limits:

- This is not filesystem confinement. It stops accidental use of real-home state through `HOME`,
  `XDG_*` and `TMPDIR`. An explicit absolute path stays readable and writable by the same OS user.
- A profile `command` is trusted test code and is not checked.
- The path check runs when the lab starts (and when the hub loads profiles). A symlink that a test
  creates later is not checked.
- URLs contain `/`, so a URL value is refused as a relative path.

## Writing tests

A test file declares tests with `test(name, function)`. The functions below are global.

```lua
test('a reply acknowledges its question', function()
  local alice, bob = agent('alice'), agent('bob')
  local q = alice:call('messaging.send', {to = 'bob', body = 'Ready?', key = 'q1'})
  bob:call('messaging.send', {to = 'alice', body = 'Yes', key = 'a1', reply_to = q.id})
  expect(bob:call('messaging.read', {}).messages).to_equal({})
end)
```

### Agents

`agent(name, opts)` opens a test agent: a real lab pane that runs `lazyrex-hub testagent`. The hub
sees real pane ancestry for its calls. The suite's manifest sets the mode of all its agents:

- Roster mode: the manifest has the `test.agents` key (also as an empty list; it refuses `null`).
  The agent joins the roster agent `join`, and `a.id` is `name`.
- Identity mode: the manifest has no `test.agents` key. The agent reports the conversation
  identity `<kind>:<name>` from its pane (`a:report`, source `startup`). `agent` returns when
  `hub.identity.list` shows that identity current in the agent's pane, and fails after 15 s. If
  the identity is current in another pane, `agent` fails at once and names both panes. A failure
  closes the pane, and a later call opens a new one. `a.id` is the identity.

A second call with the same name returns the same agent. A call with another `kind` or `report`
option is an error. After `a:exit()`, the same name opens a new pane. A second pane that reports
the same kind and conversation ID makes the identity current in the new pane, and the old pane
has no current identity. That is a move, not "superseded" (another conversation replaces it).

Options:

- `join` (roster mode only): the roster agent to join as (default `name`). `false`: do not join.
  Identity mode refuses `join`.
- `kind` (identity mode only): `claude` (default) or `codex`. Roster mode refuses `kind`.
- `report` (identity mode only): `false` sends no report, and `a.id` is nil until a test reports
  (`a:report` sets `a.id` to the reported identity). Roster mode refuses `report`. `a.id` is the
  identity the agent reported last; another pane can report it later and make it current there.
- `title`: the tab label (default `name`). The hub uses it as the identity name.
- `unnamed`: `true` gives the tab no label, so Rex shows its generated label. The hub then names an identity after the terminal title (`a:title`).
- `keep_open`: `true` keeps the pane after the agent process exits (by default Rex closes the pane). Use it with `a:exit()`.
- `session`: the session of the pane (default: the tests session).

To move a suite to identity mode, delete `test.agents` from its manifest. A literal agent ID in a
test (`'alice'`) becomes `alice.id`, and `join = false` becomes `report = false`. The roster mode
ends when the hub removes `hub.join` and the roster (step (d) of the join removal); the manifest
then refuses `test.agents`.

Methods:

| Method | Path |
| --- | --- |
| `a:call(action, args)` | Through the agent face (`rex -C lazyrex`, the MCP path) to the plugin. The caller's timeout is 10 s, so a "timed out" from `a:call` can be the caller's own, not the hub's. |
| `a:call_short(action, args)` | `call` for a result over 48 KiB: `{bytes, value}`, as `v:get_short()`. |
| `a:call_raw(action, json)` | `call` with the arguments as JSON text, for an empty array: Rex Lua encodes `{}` as an object. |
| `a:direct(action, args)` | To the plugin client itself. |
| `a:hub(action, args)` | To the hub. |
| `a:set_label(label)` | Renames the agent's tab in Rex. The hub names an identity by its tab label. |
| `a:title(text)` | Sets the terminal title of the agent's pane (OSC 2), as an agent CLI does. |
| `a:print(text)` | Writes `text` to the agent's pane as program output (no newline is added; use `\r\n`), so a test can find known text on the screen (`block.method` with method `format`). |
| `a:program_status(state, opts)` | Reports the root program status record of the agent's pane (OSC 7501), for example `idle`, `working` or `blocked`, as an agent CLI does. `opts` may add `kind`, `app` and `msg` (plain text; the report carries it as base64), as Claude Code reports a prompt. The hub reads it for notice readiness (`tests/harness/tests/notice_ready.lua`) and for prompts (`tests/prompts`). |
| `a:input(reset)` | The bytes written to the agent's pane since the capture started. The first call starts the capture: the pane's terminal goes to raw mode without echo, so a key without Enter arrives too. `reset = true` clears the captured bytes after the read. |
| `a:exit()` | Ends the agent process, as the user quits an agent CLI. The agent cannot be used again. |
| `a:report{kind, conversation_id, source, block}` | `hub.identity.report` from a new process in the pane, like an agent hook. `block` is an optional hint (the hub ignores a wrong one). |
| `a:report_async(key, r, delay_ms)`, `a:report_result(key)` | Start a report process that waits `delay_ms` before it reports, and return when the process has started and two clock ticks have passed (a Linux start time has a 10 ms resolution); then wait for its outcome (`{result}` or `{error = {code, message}}`). The process start time is older than a report started after it (the stale-report order). |

A call from the runner itself (`H.perform(client, action, args)`) comes from outside every pane.
Use it to test the plugin's refusal of an unknown caller.

A call with a client that is not a nonempty string fails in the runner with the code `bad_client`
and a traceback; it never reaches Rex. A View's renderer client is checked the same way: the
harness uses a client only when its ID starts with `client:` and `client.inspect` reports that same
ID as connected. Every other candidate writes a `harness:` line to `runner.log` with the raw
`target.resolve` answer and the `client.inspect` answer or error: a malformed ID, an inspect error
(returned or raised), or an answer for another ID. Only a client ID that `client.inspect` reports
disconnected for that same ID is skipped without a line (`tests/harness/tests/resolve_guard.lua`).

### The UI user

`user` drives real renderer panes. The test hub config sets `view` to `lazyrex-hub testview`.
The hub starts it like the real renderer, so the hub's own view path runs: admission, durable
operations, effects and retries.

| Call | Effect |
| --- | --- |
| `user:open(tab)` | `hub.ui.navigate` to a tab, then waits for its view. |
| `user:view(tab or route, plugin, session)` | An open view (for example a form that an effect opened). `session`: only a view in that session. |
| `user:command(id, from)` | Runs a palette command, as the Rex palette does (`hub.palette.run`). `from` is a View: the palette opens in its pane, so the view is the source. |
| `user:panel(id, opts)` | Runs a palette command from a new pane, as the user does, and returns the view that it opens (a panel or a form). The pane is in a new window of `opts.session` (default: a new session). `opts.plugin`: another plugin. |
| `user:plugin_view(action, args)` | Calls the plugin's `view.*` from the runner, which is not the hub: for refusals. |
| `v:render()` | Asks for a new render and returns the tree. Fails if that render failed, even when the hub kept an older tree. |
| `v:get()` | The snapshot that the renderer gets (`hub.view.get`). The hub moves a tree's `status` here. |
| `v:get_short()` | `v:get()` for a snapshot over 48 KiB: `{bytes, value}`; each string over 64 bytes of the value is `{len, sha256}`. |
| `v:try_render()` | `{status = 'ok', tree}` or `{status = 'failed', error}`. |
| `v:action(node, action, input, key)` | A view action. Returns the final operation: `{status, result or error, effects, effects_error}`. |
| `v:draft(node, text, base_revision, key)`, `v:send(node, text, draft_revision, key)` | Draft save and send. |
| `v:effect_retry(key)`, `v:history(before)` | Retry failed effects; read an older history page. |
| `v:rename_tab(label)` | Renames the view's Rex tab, as the user does. The hub sends `view.rename`. |
| `v:tab_label()` | The label of the view's Rex tab. (`v.label` is the renderer's client label.) |
| `v:seen(reason)` | Reports that the user looks at the view (`hub.view.seen`, default reason `focus`), as the renderer does. The plugin gets `view.seen` later. In the lab no app is connected, so a navigation alone does not count as seen. |
| `v:crash()` | Ends the renderer process (`testview.crash`, exit status 2) with no word to the hub, as a renderer crash does. The hub sees the pane's child exit. |

An operation is final when its status is not `pending` and its effects are not `pending`. The
helpers read the durable record from the hub, so a `close_form` effect that closes the renderer
does not hide the result.

### Hub state

| Call | Returns |
| --- | --- |
| `notice(agent)` | `{found, pending, intent_rev}` |
| `invalidates(plugin?)` | The number of `hub.ui.invalidate` calls of the plugin. A count that stays the same over an interval proves only that the plugin made no `hub.ui.invalidate` call in that interval. It does not prove that no other idle render loop runs. |
| `calls(action, {after?, plugin?})` | The recorded hub service calls of the plugin (`hub.test.calls`): a list of `{seq, action, instance_id, args}`, oldest first, and the full reply `{calls, next, dropped}`. The hub records each call of the registered current instance, except `hub.test.*`, once, when it arrives: before the action faults run. A call that fails (also by a fault or a closed gate) is recorded too. `args` is the call's args object as the hub received it. `after` returns only later calls. `next` is the highest `seq` that the hub gave, for any plugin: keep it as a baseline (`local _, base = calls('none')`, then `base.next`). A `seq` belongs to one hub process: a hub restart starts again from 1 with an empty record, so a cursor does not cross a restart. `after` must be a whole number from 0. An unknown plugin is refused. The hub keeps the last 1000 calls of each plugin. `dropped` is true when it dropped a call after `after`; a test that proves "never" checks that `dropped` is false. |
| `renders(plugin?)` | The render counts of each view of the plugin (`hub.test.renders`): `{view_id, route, attempts, ok, failed, transient, causes, declared, sql_tracked, last_call_ms}`, in `view_id` order. `last_call_ms` is the time of the last `view.render` call, with its reply parts. `declared` is the declaration that the view has now (`{changes}`), nil for none. `attempts` counts each `view.render` call; `causes` counts each cause once for each attempt: `commit`, `invalidate`, `register`, `attach`, `snapshot`, `status`, `layout`, `changed`, `operation`, `test`. Causes that come during a render go to the next attempt. |
| `write_profiles(profiles)` | Replaces `LAB/profiles.json`, the only target. A table is encoded as JSON; a string is written as it is (for example malformed JSON). An empty table encodes as `{}`, so pass the string `{"profiles":[]}` for no profiles. The hub reads the file again on each profile call. |
| `H.args.lab` | The lab dir. |
| `H.args.sdk` | The SDK directory that the hub gets (`<checkout>/sdk/lua`), for a sandbox test that loads the SDK itself. Its `testing.lua` has `register_reply(fields)`, a `hub.register` reply with test limits for such a sandbox. |
| `tabs()`, `tab(key)`, `views()` | The declared tabs (key, title, route, status, session_id, revision) and the open views (with session, window and block). |
| `operation(key)` | A view operation's durable state. |
| `session(label)` | Creates a real lab session and returns its ID. |
| `close_session(id)` | Destroys a lab session (`session.destroy`), for example a thread's home. |
| `node(tree, id)` | A node of a tree, at any depth. |

### Writes from outside the plugin

`sql(sql, params?)` reads the plugin's SQL database (`hub.test.sql`, the committed state) and
returns rows keyed by column name: NULL is `nil`, a large integer is its decimal string, a BLOB
stays `{blob = '<base64>'}`. `params` is a list for `?` or a table for `:name`; a NULL parameter is
`{null = true}`. `sql_write(sql, params?)` runs one statement now, in its own transaction, as
another writer would (for fixtures, for example rows that an older version wrote). It returns
`{changes, id}` and renders the plugin's views again. It fails with `busy` while the plugin has an
open transaction, and it never matches an injection. Both take an optional third argument, the
plugin.

### Concurrent writes

`inject{match = {sql_commit_writes = <table>}, fail, fail_code?, after?, skip?, times?}` fails the
plugin's next `hub.sql` commit that changed that table: a handler commit, or an exec or batch
without `tx`. The table name is not case sensitive. `fail = '<message>'` is required; `ops` and
`replay` are refused. The error code is `injected_failure` (definite). `fail_code = '<code>'` sets
another code, for example `unavailable` (transient); it must be nonempty. Without `after`, nothing
is committed, as after a crash or a lost connection. With `after = true`, the commit happens, and
the plugin gets the error anyway, as after a lost reply. `times` is a count or `"always"`.
Injected commits never match an injection.
With `sql_param = <value>` beside `sql_commit_writes` (a nonempty string or an integer), the
injection matches only a commit whose transaction ran a statement with a parameter equal to the
value (in `params`, an array or an object). For example, the commit that inserts the run of one
date.

`inject{match = {sql_begin = true}, sql = {{sql = ..., params = ...?}, ...}, skip?, times?}` acts at the
plugin's next `hub.sql.begin`: the hub first commits the statements in a transaction of their own,
as another writer (no commit fault applies to it), and then begins the plugin transaction. So the
other writer commits after the plugin's last read outside the transaction and before its write
transaction. A failed statement fails the begin, and nothing of the injection commits.

`skip = n` lets the first `n` matching calls pass unchanged; the injection acts from the next
one. For example, `{match = {sql_commit_writes = 'runs'}, skip = 1}` acts on the second commit
that changes `runs`.

`inject` returns the injection ID. `injection(id)` gives `{consumed, remaining}`.
`cancel_injection(id)` ends an injection now (for example one with `times = "always"`): it matches
no later call, and the file check counts it as used up.

`with_injection(spec, n, fn)` runs `fn` and fails unless exactly `n` calls matched. An injection
with a count that is not used up at the end of the file fails the file.

A window hold, `inject{match = {window = key, caller = c}, hold = true}`, matches no commit. It holds
one `hub.spawn_window` call of the plugin with that key (and that caller, if `caller` is set). The
hub holds the call after Rex created the window, and before the hub records the window: the leaves
stay `sending`, as after a crash at that point. A hold has no `ops`, `replay`, `fail`, `skip` or
`times`; it holds one call. Then:

- `hub:restart{crash = true}` ends the hold with the hub process. Use it for the crash case.
- `cancel_injection(id)` releases a held call: the hub records the window as usual.
- A replay of the same key returns the recorded rows and never reaches the hold.

Rex delivers one request at a time to the hub. While a call is held, the next hub request waits
until the held call's caller times out (a plugin's `client.perform` timeout). So a test sees a hold
through Rex (the labeled panes of the new window), and restarts the hub before that timeout when it
tests the crash case. `hub:restart` does not use the hub: the runner signals the hub process. A
window hold is for this crash case only; for a caller that retries, use a gate fault ("Faults"),
which refuses calls and does not hold them. `tests/harness/tests/window_hold.lua` shows both cases.

### Fake Rex apps

No Rex app runs in a lab, so `hub.app.select_session` and `hub.view.app_action` refuse with
`no_app`. `app(name, {sessions = {...}, fail = code})` opens a fake app (test mode only): a second
Rex connection in the hub process whose hello says kind `app`, as the real app's does. It declares
`session.select` and `pane.insert_path`, shows the named sessions (`session.attach`), and answers
each call with `{}`, or with the error code `fail`. The hub finds it like a real app.

| Call | Effect |
| --- | --- |
| `a:calls()` | Every call that the app got: `{action, args}`. |
| `a:show(session)` | Shows one more session. |
| `a:fail(code, message)` | Answers later calls with this error code and message (`''`: `{}` again; the message defaults to `fake app error`). |
| `a:close()` | Closes the app. Apps last for the file; a hub restart closes them. |

`hub.app.select_session` reads the target session, so the hub must be attached to it: use a
session that the hub made (`hub.session.ensure`), as a template start does. Observed in the lab: a
session that only the runner made gives `session.view: conflict: stream is not attached`.

### Hub restart

`hub:restart()` stops the hub (SIGTERM to its process group) and starts it again with the same
config, lab server and state. `hub:restart({crash = true})` kills it (SIGKILL) instead, as a crash
would. The call returns when a new hub process is ready: every plugin is registered, the agent
face lists its exports, and each plugin is reachable by its Rex label `lazyrex-plugin:<name>`
(`agent.direct` needs the label, and a plugin can set it after it registers). It returns the new `hub.test.status` (with `pid`).

`hub.test.status` gives each plugin record `registered`, `client_id`, `exports`, `face`, `state`
(running, stopped, waiting, refused or retained), `error` (the refused start), `waiting` (the
dependencies that are not registered), `instance_id`, `block_id` and `child_pid`. A hub start (also
a restart) fails at once when a plugin is refused or retained: the error names the plugin and its
error. A waiting plugin is not ready yet: the harness waits, and its timeout text names the
dependencies.

The test-only config key `test_hold_recovery: true` holds plugin recovery at hub start until
`hub.test.recover`, at most 60 s. A Go lab test uses it to call an action before recovery reaches
a record (`plugstart_lab_test.go`). A hub without `-test` refuses the key.

- The runner asks for the restart with the result line `LRT restart term|kill`; `lazyrex-hub test`
  restarts the hub when it reads that line. Only the hub's own process group gets a signal, and
  only while its waiter has not reaped it.
- Processes that the old hub left stay tracked: the lab cleanup checks them at the end.
- After `hub:restart({crash = true})`, Rex keeps the lost hub client for a while. A call to the
  client name `lazyrex-hub` alone (`-C lazyrex-hub`, as `fake-agent.sh` reports) then fails:
  `client target "lazyrex-hub" is ambiguous`. The real hook and `lazyrex-hub testagent cli` pick
  the connected hub client (spawn_adopt.lua).
- The old hub log stays as `hub.<n>.log`.
- Agents and views are lab panes: they stay. What the hub does with plugins and views at a restart
  is the hub's own behavior, which the test checks.
- A test view attaches again after a restart, as the real renderer does: it watches for a new hub
  client every 200 ms, then attaches and subscribes. Wait for `attached` in `H.views()` before a
  view action (restart.lua).
- A test view serves the hub's callbacks (view.release, view.changed) before its first subscribe,
  as the real renderer does. An attach or subscribe that fails is tried again, also on the same hub.
- The file `LAB/testview-hold` controls the test views of one route: line 1 is the route, line 2
  the mode. `hold` (the default) keeps them between attach and subscribe, at most 20 s; the
  subscribe is not sent yet. `fail-subscribe-once` makes the first subscribe fail, simulated by
  the test view. `retire-wait` stands for unsaved text: the test view accepts `view.retire`, but it exits
  only after the file stops naming `retire-wait` for its route. `testview.status` reports `held`, `hold_expired`, `subscribed`,
  `subscribe_failures` and `changes` (the count of `view.changed` notifications).

### Faults

`fault(spec)` arms a one-shot hub fault and returns its id. `fault_fired(id)` reports whether it
fired. A fault that has not fired at the end of the file fails the file. An unknown id is an error.

| Spec | Effect |
| --- | --- |
| `{point = 'spawn-create', mode = 'drop', op = key}` | `hub.spawn` sends no create request and gets `disconnected`. The spawn stays pending; no pane exists. |
| `{point = 'spawn-create', mode = 'refuse', op = key}` | No create request; Rex error `refused`. The spawn fails. |
| `{point = 'spawn-create', mode = 'lose-answer', op = key}` | The create request runs. If Rex answers yes, the answer becomes `disconnected`: the pane exists and the spawn stays pending. An accessory pane is not adopted by its label, so it stays pending (resolveSpawns only counts candidate panes). If Rex answers with an error, that error stays and the fault stays armed. |
| `{point = 'spawn-create', mode = 'fail-sending', op = key}` | The commit of the sending state fails, so the hub sends no create request. The spawn fails with an error that starts with `not_created`. Also for `hub.spawn_window`: `op` then matches a leaf key (`window/<key>/<n>`), and every leaf of the window fails. |
| `{point = 'action', action, when = 'before', mode, key, plugin}` | A call of `action` from `plugin` (default: the plugin under test) with `args.key == key` (if given) runs no handler. `sql_param` (any action point): only a call whose `args.params` has a parameter equal to it, for example `hub.sql.query` with an op key. `fail` replies code `failed`, or `fail_code` if given; `lose-reply` always replies code `disconnected`. |
| `{point = 'action', action, when = 'after', mode, key, plugin}` | The handler runs once. If it succeeds, its reply is replaced (`failed` or `disconnected`). If it fails, its own error is the reply and the fault stays armed. |
| `{point = 'watch-nudge', mode = 'drop', key, plugin}` | The hub does not send the next `hub.changed` nudge of that plugin's watch `key` (any watch if `key` is not given). The repeat schedule continues (docs/hub.md, "Nudges"). |
| `{point = 'status-read', mode = 'fail', key = session}` | The next read of a pane's program status in that session (any session if `key` is not given) fails after the hub attaches the session. The hub then treats the state as unknown. |
| `{point = 'status-read', mode = 'hold', key = session}` | The notice loop's next status read of an agent with an unknown state, in that session, waits after the read until `fault_release(id)` (at most 30 s). A test can change the status in between: the older result must not apply. A hold that is still closed at the end of the file fails the file. |
| `{point = 'start-admission', mode = 'hold-before' or 'hold-after', key = plugin, hold_ms}` | The next start of that plugin (any plugin if `key` is not given) waits until `fault_release(id)`, at most 30 s, or `hold_ms` ms. `hold-before` waits after the lifecycle lock, before the admission; `hold-after` waits after the admission, before the retire and the spawn. Rex gives the hub one request at a time, so a hub request that waits on the held start holds the queue, and the release cannot arrive: such a test uses `hold_ms`. |
| `{point = 'plugin-create', mode = 'refuse' or 'lose-answer' or 'hold-send', key = plugin, hold_ms}` | The next create request of a block for that plugin's instance (`spawn`; any plugin if `key` is not given). `refuse`: Rex refuses, and nothing is created (a known outcome). `lose-answer`: the request runs; if Rex answers yes, the answer becomes `disconnected` (an unknown outcome: the block exists, and the intent stays open). If Rex answers with an error, that error stays and the fault stays armed. `hold-send`: the spawn gets `disconnected` at once, and the hub sends the request only after `fault_release(id)`, at most 30 s, or `hold_ms` ms. The intent is unresolved until the block appears. The event `fault_sent` comes when the request was sent. |
| `{point = 'prompt-answer', mode = 'close', when = 'before-write' or 'before-enter'}` | The open prompt of the next `hub.prompt.answer` closes at that binding check, as a rebind would close it. before-write: the answer is refused with zero bytes. before-enter: the paste is written and Enter is not (`partial`). |
| `{point = 'prompt-answer', mode = 'refuse', when = 'write' or 'enter', fail_code = code}` | The next answer write (`write`: the keys or the paste; `enter`: the Enter) is not sent and fails with `fail_code` (default `not_found`). |
| `{point = 'prompt-answer', mode = 'lose-reply', when = 'write' or 'enter'}` | The next answer write is sent, and its reply is lost (`disconnected`). |

| `{point = 'view-render', mode = 'hold', key = route, plugin, hold_ms}` | The next render result of a view of `plugin` with that route (any route if `key` is not given) waits after the plugin answered `view.render`, before the hub uses it, until `fault_release(id)`, at most 30 s, or `hold_ms` ms. It holds the plugin's view worker, not the hub's request queue, so a test can commit, invalidate or register during the hold (render_deps.lua). |
| `{point = 'op-call', mode = 'hold-after-sent', key = plugin, hold_ms}` | Stop handler only. The next `hub.admin.stop` of that plugin with an `attempt` (a manager stop op; any plugin if `key` is not given) waits after the attempt's `sent_at` commit, before its first effect (the row insert and the retire), until `fault_release(id)`, at most 30 s, or `hold_ms` ms (1 to 60000). `fault_state` reports `held = true` while it waits, and the event `fault_held` marks the start of the wait. The handler runs in the manager goroutine, not in the hub's request queue, so other actions run during the hold. A lab test stops the hub there to leave a sent attempt with no effect. The start handler has no such point: it holds the hub's lock at that place. |
| `{point = 'op-call', mode = 'hold-before-retire', key = plugin, hold_ms}` | Replace handler only. The next `hub.admin.replace` of that plugin with an `attempt` (a manager update or restart; any plugin if `key` is not given) waits after the attempt's `sent_at` commit and its early checks, immediately before the retire of the old instance, until `fault_release(id)`, at most 30 s, or `hold_ms` ms (1 to 60000). `fault_state` and `fault_held` are as for `hold-after-sent`. The handler holds no `h.mu` there, but it holds the reload lock and the plugin's lifecycle lock, so a reload and the plugin's other lifecycle calls wait until the release. A lab test closes the instance's block during the hold: the retire then finds the block gone, marks the plugin stopped, and the replace refuses `stopped` after its `sent_at` (the real stopped-after-retire path). |
| `{point = 'action', action, when = 'before', mode = 'gate', key, plugin}` | A barrier for a caller that retries. From the call that fires it until `fault_release(id)`, every matching call gets code `unavailable` and runs no handler. The hub retries a view operation with that outcome every 500 ms, so the operation passes after the release. A gate that is still closed at the end of the file fails the file. `fault_state` reports `gated`, the number of refused calls. `fault_release` of a gate that has not fired is an error. |

- `nth = n` (action, status-read, start-admission, plugin-create, prompt-answer and view-render faults; not op-call) fires on the n-th matching call; the default is 1. Every armed
  fault that matches counts the call, also when another fault fires. An after fault counts only
  calls whose handler succeeded. A call that a closed gate refuses counts for no fault: it does
  not reach the hub.
- A hub request cannot wait for a test: Rex delivers the hub's requests one at a time, and every
  other caller waits behind a held request (rex-protocol.md, "The hub has one request queue"). So
  a barrier refuses calls (gate); it does not hold them.
- `op` matches the `hub.spawn` key. Only `hub.spawn` has the spawn-create point; `hub.spawn_window` does not, except mode `fail-sending`.
- `action` must be a hub action that a plugin can call. `hub.test.*` actions are refused. A field
  that does not apply to the point is refused. Every field is a string, except `nth` (a positive
  integer).
- Action faults apply only to calls from a registered plugin. An agent call through the agent face
  is not affected.
- Limit: `lose-reply` and `disconnected` are injected error replies. The transport still delivers
  them. A test proves the behavior for that error code, not for a real lost connection.
- Faults exist only in test mode: a hub without `-test` does not declare `hub.test.fault`, and its
  dispatch refuses `hub.test.*`.

#### Lab faults (-lab-fault)

`-lab-fault point=<p>,mode=<m>[,op=<key>]` is a hub start flag, not a `hub.test.fault` spec: a test
file cannot arm these points with `fault(spec)`. The flag is repeatable, and each fault fires once.
It is for labs only: the hub refuses it with `-real-server`. The valid points are in `events.go`
(`faults.Set`). A fired fault emits the event `fault` with its point and mode.

- `point=block-create,mode=exit-before`: a plugin spawn records its spawn intent, then the hub exits
  with status 86 before it sends the block create request. Test: none in this repository.
- `point=block-create,mode=exit-after`: the hub tries to send the block create request and does not
  wait for an answer. If the send fails, the hub logs the error. In both cases the hub emits the
  event and exits with status 86. Test: none in this repository.
- `point=notice-written,mode=gate`: after the terminal write of a notice and before the hub records
  it as delivered, the notice loop waits until the test creates `<state>/gate.release` (the hub
  removes the file). Test: none in this repository.
- `point=palette-start,mode=fail`: the hub start waits for the first palette write attempt (a
  failed write also counts), then runs the palette stop. The stop write follows the ownership and
  failure rules in docs/hub.md, "Stop and exit status". The start then fails, and the hub exits 1.
  Test: `TestHubPaletteStartFailsLab` (`PALETTE_LAB=1`).

### Waiting

`wait(what, fn, timeout)` calls `fn` until it returns a value other than `nil` or `false`, and
returns that value. It runs `fn` under `pcall`: an error in `fn` is kept and `wait` tries again.
After `timeout` seconds (default 10) it fails with `what` and the last error.

Read the result from the return value. Return a table for several values. Do not assign an outer
local inside `fn`, and do not read an outer local in `fn` that the test changes later. In Rex's Lua,
a caught error splits a function's locals from its closures, in both directions (rex-protocol.md,
"Rex Lua runtime"), so the test or `fn` can read a stale value. The runner's `xpcall` around each test
does not prevent it.

```lua
-- Do not: the write to row can be lost after a caught error in fn.
local row
wait('the row', function() row = sql('SELECT * FROM runs WHERE id = 1')[1]; return row end)

-- Do: read the return value.
local row = wait('the row', function() return sql('SELECT * FROM runs WHERE id = 1')[1] end)
local got = wait('both', function()
  local a, b = tab('a'), tab('b')
  return a and b and {a = a, b = b}
end)
```

### Assertions

`expect(v).to_be(x)`, `.to_equal(x)` (deep), `.not_to_be(x)`, `.to_match(pattern)`,
`.to_contain(x)`, `.not_to_contain(x)`, `.to_have_length(n)`, `.to_be_truthy()`,
`.to_be_falsy()`. The colon form `expect(v):to_be(x)` works too. `assert_eq(a, b, msg)`,
`assert_true(v, msg)`. `expect_error(fn, pattern)` returns the error; `pattern` matches the
message or equals the code. A failure names the line in the test file.

## Agent parity

Every user action must also be an agent action (an MCP tool). The harness checks it.

- A palette command names `action = '<export>'` or `user_only = '<reason>'`.
- An action descriptor in a view tree (a form submit, a list item, a menu, a widget, a composer
  action, nested nodes too) names `tool = '<export>'` or `user_only = '<reason>'`.
- The named export must be in the plugin's `exports`, and the agent face must list it: the plugin
  must declare `hub.forward`.
- The tool can also be a hub action that agents can call (`agentHubActions` in
  `hub/cmd/hub/agentface.go`, for example `hub.spawn` or `hub.stop`). The parity report reads the
  agent face from Rex (`client.inspect`), and the action must be on it. Another `hub.*` name fails
  as not an export.
- `$back`, `$cancel` and the history `load` action are exempt (renderer mechanics). The exempt list
  is `parityExemptNames` in `hub/cmd/hub/testmode.go`.

The parity sweep renders every declared tab and opens every palette command. Then it fails on
any violation and on any required route that no render reached: an unchecked route does not pass.
A form that opens only from another view needs the manifest fixture (`test.parity_fixture`). Each
file also ends with a check of every tree that it rendered.

Limit: the check proves export and discovery. It does not prove that the view action and the
agent action run the same handler. Test both paths for each flow: `user:...` through the view and
`agent(...):call(...)` as a tool.

Outside test mode, the hub only logs a descriptor that names neither field (once per route, node
and action). It does not change behavior.

## Limits that tests must respect

- **One control message is at most 64 KiB** for a `rex do` script (rex-protocol.md, "Rex Lua
  runtime"). The runner measures each request before it sends it and fails with
  `request_too_large`. Hub test replies stay under 48 KiB. A larger tree arrives in
  chunks of one revision; a change during the read starts it again. Nothing is truncated.
- **The hub has one request queue** (rex-protocol.md). No `hub.test.*` handler calls a plugin. The
  runner never answers a callback, so it polls instead: short sleeps, then a deadline.
- A render waits up to 25 s, which is longer than the hub's 10 s plugin call timeout. So a render
  that times out (for example a tree over 64 KiB) is reported as failed.

## Test mode in the hub

`-test` declares the `hub.test.*` actions and refuses them at dispatch without it. The hub refuses
`-test` with `-real-server`, and it refuses `-test` without a positive lab proof. In test mode the
hub asks no Rex server whether the config names the real one: the proof is enough, so a test run
never reaches the real server. The pane helpers (`testagent`, `testview`) check the same proof for
their `REX_SERVER`. The proof:

- the config server is `unix://<dir>/rex.sock`;
- `<dir>/lab.json` belongs to the current user and names that socket and a server pid;
- that pid is alive, and its command line has `--listen unix://<dir>/rex.sock` and
  `--state-dir <dir>/state`.

`lazyrex-hub test` and `lab/server.sh` both write `lab.json`. So a hub started by hand with
`-test` on a `lab/server.sh` lab passes the proof. `-test` also needs `test_roots` in the config
(see "Isolation"); without `-test`, the hub refuses `test_roots` and `test_source_runs`.

Without `-test`, the hub (and `lazyrex-hub status` and `stop`) asks the rex CLI on PATH for its
default server (`rex --autostart=false server status --json`, without `REX_SERVER` and `-S`,
`hub/cmd/hub/realserver.go`). The config names the real server if:

- it is the macOS path of the default server (no question to Rex);
- it is the default endpoint, after the path is cleaned, unescaped and its links resolved;
- it answers with the `instance_id` of the default server;
- or the answer is uncertain: the default server answers with no `instance_id` or no endpoint, the
  default query fails with an error other than "rex server is not running", an answer is not status
  JSON, or no default server runs and the config server does not answer.

A config server that does not answer while the default server answers at another path is not the
real server. On Linux the real server is at another path than on macOS; before this check, the hub
missed it. Observed on Rex `0.0.1792158932-g88fd41`: with no default server, `server status` exits
1 with "Error: rex server is not running".

## Harness self-tests

- `plugins/lazyrex/`: the home plugin and its tests, with a messaging stub (`tests/stubs/messaging/`)
  and the harness fixture in `test.plugins`. Run `lazyrex-hub test plugins/lazyrex`.
- `tests/harness/`: a fixture plugin (its state is the SQL table `kv(key, value)`, JSON values)
  and tests of the harness itself (agents, views, effects, limits, injections, assertions). Run
  `lazyrex-hub test tests/harness`.
- `tests/harness-identity/`: a plugin with no actions and no `test.agents`, so its tests run the
  identity mode of `agent`. Run `lazyrex-hub test tests/harness-identity`.
- `tests/harness-fail/`: files that must fail, each for the reason in its first line. Its plugin
  (`fixture.lua`) is a small plugin of its own with only what these files use: a plugin loads only
  files inside its own directory (docs/plugin-sources.md, section 2).
- `tests/isolation/`: a probe plugin as the plugin under test, a smaller probe of its own as a
  dependency (`isodep/`, which loads no source outside its directory), and a spawned worker. They
  report their environment, args and working directory. The plugin under test also reports its
  own dir and a `${PLUGIN}` path from `test.env`: the two match, also after a crash respawn. The
  tests also check the test-mode profile refusals.
- `TestHarnessIsolation` runs `tests/isolation` with an outer `HOME` and `XDG_CONFIG_HOME` in a
  temporary dir (never the real home) that hold a sentinel, and an outer secret variable. No lab
  process finds the sentinel through `HOME` or `XDG_CONFIG_HOME` or gets the secret. The outer
  `HOME` does not change, and no lab file contains the secret.
- `HARNESS_LAB=1 go test -run TestHarness ./cmd/hub` (in `hub/`) runs both, and also checks an
  empty `-run` match and an interrupt. It checks that no lab process survives.

## Go lab tests

A Go lab test of the hub (`hub/cmd/hub/*_lab_test.go`) starts its own lab Rex server. No lab test
reads a socket from an env var. The one exception, `replyguard_lab_test.go` (`REPLY_LAB_SOCKET`),
goes with the hub.store removal.

`HARNESS_LAB=1 go test ./cmd/hub` runs the tests gated on `HARNESS_LAB` (rexLab,
harnessBinary). The tests behind their own gates (`PALETTE_LAB`, `SHUTDOWN_LAB`, `SETUP_LAB` and
`HUBCTL_LAB`) also start their own servers, but they skip unless their gate is set.

The thread view's lab tests (`view/*_lab_test.go`) build the view and the hub and start their own
lab server too:

| Gate | Command (in `view/`) | What it runs |
| --- | --- | --- |
| `MODE_LAB=1` | `go test -count=1 -v -run 'Lab$' .` | The thread-view labs on a real pane: FocusWindow, ShortPane, Print, ReloadLeavesNoLiveRows, OpenWindow, NewlineKeys, Resize, LateMessage, LiveRegionResize and ComposerGrow (lead-labenv2: part of the main lab). |
| `CHUNK_LAB=1` | `go test -count=1 -v -run 'Lab$' .` | The reply parts labs (TestReplyPartsHoldLab, TestReplyPartsTimingLab); not in the main lab. |
| `HUBUNAVAIL_LAB=1` | `go test -count=1 -v -run 'TestNoHubTextLab$' .` | The view text when no hub answers; not in the main lab. |
| `RESIZE_BURST_LAB=1` (with `MODE_LAB=1`) | `go test -count=1 -v -run 'TestLiveRegionResizeBurstLab$' .` | The no-wait resize burst, a known limit (view/README.md); off in the main lab. |
| `TEAPROBE_LAB=1` | `go test -count=1 -v -run 'TestTeaProbe' .` | The Bubble Tea and Rex facts (docs/rex-protocol.md); it records, and fails only when the lab fails. |
| `VIEW_TEA_TRACE=1` (with a gate above) | | Each view process logs its renderer writes to `<lab>/tea-<pid>.log`. |

The main lab is the full lab that runs on hub main after each push. It sets `HARNESS_LAB`,
`PALETTE_LAB`, `SHUTDOWN_LAB`, `SETUP_LAB` and `HUBCTL_LAB` (together about 40 s more on Linux).
Then it runs the thread view labs: `MODE_LAB=1 go test . -count=1 -v -run 'Lab$'` in `view/`,
with its own log and summary line. `RESIZE_BURST_LAB` stays off (a known limit). A red view step
is a red main, as a red hub step is. The first lines of each main lab log name the tip and the
envs.

- A test that runs the hub binary uses `o.startLab` and `o.startHub` (`TestSessionRenameKeptLab`).
- A test that runs an in-process `Hub` uses `rexLab(t)` (`rexlab_test.go`). It starts only the lab
  Rex server (`o.startLab`, no hub), with the test binary as the keeper. It skips without
  `HARNESS_LAB=1` or the rex CLI. It fails if the socket is not inside the lab dir, and it sets
  the test's `REX_SERVER` to the lab server. The test sets its Hub's `cfg.Server` to `l.server`.
  The cleanup stops the server and ends the lab dir with `endGoLab`.
- Each test gets a new server. An in-process hub claims state for the whole server (the UI
  session, the hub's client name), so a shared server would make a result depend on test order.
