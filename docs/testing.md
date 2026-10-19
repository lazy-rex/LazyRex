# Testing plugins

A plugin repository contains only Lua: the plugin and its tests. `lazyrex-hub test` runs the
tests against the **real hub**, on an isolated lab Rex server that the command starts and stops.
No test touches your own Rex server.

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
| `-keep` | Keep each lab directory (logs, events, state). The processes stop anyway. |
| `-v` | Print the hub, plugin and runner logs. |
| `-lazyrex <dir>` | The LazyRex checkout. Default: `../LazyRex` beside the plugin, `LAZYREX_DIR`, or the checkout of the binary. |

Exit status: 0 when every file passes, 1 when a test or a check fails, 2 for a harness error (for
example a bad manifest).

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
      plugin, the plugins in `requires`, and the plugin under test.
   3. It waits until each plugin is registered and the agent face lists the plugin's exports.
   4. It runs the file with `rex do lab/test/runner.lua`.
   5. It stops the hub and the server with signals to their own process groups, and checks that
      every process that it saw under them is gone. A survivor fails the file, and the output
      names its pid, start time and command. The harness sends a survivor no signal: on macOS a
      pid can be reused between a check and a signal, so the harness cannot prove that it owns it.
4. Tests in one file share one hub and run in order. Files do not share anything.

The same cleanup runs after a failure, a startup failure, a timeout and an interrupt (Ctrl-C).

## The manifest: `lazyrex.json`

```json
{
  "name": "messaging",
  "main": "messaging.lua",
  "events": "session",
  "requires": [{"name": "profiles", "path": "../Profiles/profiles.lua"}],
  "test": {
    "agents": [{"id": "alice", "name": "Alice"}, {"id": "bob", "name": "Bob"}],
    "transcript": false,
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
| `name`, `main` | The plugin name and its main file. Required. |
| `events` | `session` (default) or `all`, as in the hub config. |
| `requires` | Other plugins that the hub loads for the tests. Paths are relative to the plugin. |
| `test.agents` | Roster agents in the test hub config. `user` is always there. |
| `test.transcript` | The hub config `transcript` option. |
| `test.parity_fixture` | A Lua file that renders the routes that open only from another view. |
| `test.parity_routes` | Those contextual routes. The fixture must render each one. |
| `test.env` | Variables added to the lab environment. See "Isolation". |
| `test.args` | Plugin args (hub config `plugins[].args`, `rex.args` in the plugin), keyed by plugin name: the plugin under test, a `requires` entry or `lazyrex`. Keys match `[a-z0-9_]+`; `hub_client_id`, `instance_id` and `plugin_session` are refused. |
| `test.profiles` | A profile file, relative to the manifest dir. The harness copies it to `LAB/profiles.json`, the hub's profile file. A configured file that is missing is an error. Without it, the hub has only the builtin shell profile. |

Unknown fields are refused. The harness owns the rest of the hub config (`server`, `state`,
`rex`, `view`, `home`, `agents`, `plugins`, `transcript`, `palette_lua`, `profiles`,
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
| `test.env` | As the manifest gives them. |

No other variable is copied. `LANG`, `TERM` and similar variables are not set: Rex and the self-tests
do not need them (observed on the Rex build 88fd41c of 2026-10-16). If a tool needs one, set a fixed value in `test.env`.

Rules for `test.env` and `test.args`:

- `test.env` cannot set `PATH`, `HOME`, `TMPDIR`, `LAZYREX_AGENT`, any `XDG_*` or any `REX_*`.
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

`agent(id, opts)` opens a test agent: a real lab pane that runs `lazyrex-hub testagent`. The hub
sees real pane ancestry for its calls. Options:

- `join`: the roster agent to join as (default `id`). `false`: do not join.
- `title`: the tab label (default `id`). The hub uses it as the identity name.
- `session`: the session of the pane (default: the tests session).

Methods:

| Method | Path |
| --- | --- |
| `a:call(action, args)` | Through the agent face (`rex -C lazyrex`, the MCP path) to the plugin. |
| `a:call_raw(action, json)` | `call` with the arguments as JSON text, for an empty array: Rex Lua encodes `{}` as an object. |
| `a:direct(action, args)` | To the plugin client itself. |
| `a:hub(action, args)` | To the hub. |
| `a:set_label(label)` | Renames the agent's tab in Rex. The hub names an identity by its tab label. |
| `a:report{kind, conversation_id, source}` | `hub.identity.report` from a new process in the pane, like an agent hook. |

A call from the runner itself (`H.perform(client, action, args)`) comes from outside every pane.
Use it to test the plugin's refusal of an unknown caller.

### The UI user

`user` drives real renderer panes. The test hub config sets `view` to `lazyrex-hub testview`.
The hub starts it like the real renderer, so the hub's own view path runs: admission, durable
operations, effects and retries.

| Call | Effect |
| --- | --- |
| `user:open(tab)` | `hub.ui.navigate` to a tab, then waits for its view. |
| `user:view(tab or route, plugin, session)` | An open view (for example a form that an effect opened). `session`: only a view in that session. |
| `user:command(id, from)` | Runs a palette command, as the Rex palette does (`hub.palette.run`). `from` is a View: the palette opens in its pane, so the view is the source. |
| `user:plugin_view(action, args)` | Calls the plugin's `view.*` from the runner, which is not the hub: for refusals. |
| `v:render()` | Asks for a new render and returns the tree. Fails if that render failed, even when the hub kept an older tree. |
| `v:get()` | The snapshot that the renderer gets (`hub.view.get`). The hub moves a tree's `status` here. |
| `v:try_render()` | `{status = 'ok', tree}` or `{status = 'failed', error}`. |
| `v:action(node, action, input, key)` | A view action. Returns the final operation: `{status, result or error, effects, effects_error}`. |
| `v:draft(node, text, base_revision, key)`, `v:send(node, text, draft_revision, key)` | Draft save and send. |
| `v:effect_retry(key)`, `v:history(before)` | Retry failed effects; read an older history page. |
| `v:rename_tab(label)` | Renames the view's Rex tab, as the user does. The hub sends `view.rename`. |
| `v:seen(reason)` | Reports that the user looks at the view (`hub.view.seen`, default reason `focus`), as the renderer does. The plugin gets `view.seen` later. In the lab no app is connected, so a navigation alone does not count as seen. |

An operation is final when its status is not `pending` and its effects are not `pending`. The
helpers read the durable record from the hub, so a `close_form` effect that closes the renderer
does not hide the result.

### Hub state

| Call | Returns |
| --- | --- |
| `store(key, plugin?)` | `{value, rev}`; an absent record is `{rev = '0'}`. |
| `notice(agent)` | `{found, pending, intent_rev}` |
| `batches()` | The number of `hub.store.batch` calls of the plugin. |
| `invalidates(plugin?)` | The number of `hub.ui.invalidate` calls of the plugin. A count that stays the same over an interval proves only that the plugin made no `hub.ui.invalidate` call in that interval. It does not prove that no other idle render loop runs. |
| `write_profiles(profiles)` | Replaces `LAB/profiles.json`, the only target. A table is encoded as JSON; a string is written as it is (for example malformed JSON). An empty table encodes as `{}`, so pass the string `{"profiles":[]}` for no profiles. The hub reads the file again on each profile call. |
| `H.args.lab` | The lab dir. |
| `tabs()`, `tab(key)`, `views()` | The declared tabs (key, title, route, status, session_id, revision) and the open views (with session, window and block). |
| `operation(key)` | A view operation's durable state. |
| `advance_rev(n)` | Moves the store revision counter forward (never back). |
| `session(label)` | Creates a real lab session and returns its ID. |
| `close_session(id)` | Destroys a lab session (`session.destroy`), for example a thread's home. |
| `node(tree, id)` | A node of a tree, at any depth. |

### Writes from outside the plugin

`write(ops)` commits store ops in the plugin's namespace now, as another writer would:
`{key, value}` or `{key, delete = true}`, without `if_rev`. Use it to set up data that an older
plugin version wrote. `put_operation{key, fingerprint, result}` saves an operation record (what
`hub.store.op` returns), for example the result of an older plugin version.

### Concurrent writes

`inject{match, ops, replay, times}` commits `ops` (store batch ops, without `if_rev`) just before
the plugin's next `hub.store.batch` that matches. `match` is `{op_key = ...}`, `{op_prefix = ...}`,
`{writes = <record key>}` or `{updates = <record key>}`. `updates` matches a batch that writes the
key with an `if_rev` other than `'0'`: a change of a record that the plugin read, not its creation.
`replay = true` commits a copy of the matching batch first (another
invocation saved the same operation). `times` is a count or `"always"`. `fail = '<message>'` then fails the
matching batch with that message: it commits nothing, as a crash or a lost connection would.
The error code is `injected_failure` (definite). `fail_code = '<code>'` sets another code, for
example `unavailable` (transient). `fail_code` needs `fail` and must be nonempty.
Injected commits never match an injection.

`skip = n` lets the first `n` matching batches pass unchanged; the injection acts from the next
one. For example, `{match = {updates = key}, skip = 1}` acts on the second change of a record.

`inject` returns the injection ID. `injection(id)` gives `{consumed, remaining}`.
`cancel_injection(id)` ends an injection now (for example one with `times = "always"`): it matches
no later batch, and the file check counts it as used up.

`with_injection(spec, n, fn)` runs `fn` and fails unless exactly `n` batches matched. An injection
with a count that is not used up at the end of the file fails the file.

A window hold, `inject{match = {window = key, caller = c}, hold = true}`, matches no batch. It holds
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
| `a:fail(code)` | Answers later calls with this error code (`''`: `{}` again). |
| `a:close()` | Closes the app. Apps last for the file; a hub restart closes them. |

`hub.app.select_session` reads the target session, so the hub must be attached to it: use a
session that the hub made (`hub.session.ensure`), as a template start does. Observed in the lab: a
session that only the runner made gives `session.view: conflict: stream is not attached`.

### Hub restart

`hub:restart()` stops the hub (SIGTERM to its process group) and starts it again with the same
config, lab server and state. `hub:restart({crash = true})` kills it (SIGKILL) instead, as a crash
would. The call returns when a new hub process is ready: every plugin is registered and the agent
face lists its exports. It returns the new `hub.test.status` (with `pid`).

- The runner asks for the restart with the result line `LRT restart term|kill`; `lazyrex-hub test`
  restarts the hub when it reads that line. Only the hub's own process group gets a signal, and
  only while its waiter has not reaped it.
- Processes that the old hub left stay tracked: the lab cleanup checks them at the end.
- The old hub log stays as `hub.<n>.log`.
- Agents and views are lab panes: they stay. What the hub does with plugins and views at a restart
  is the hub's own behavior, which the test checks.
- A test view attaches again after a restart, as the real renderer does: it watches for a new hub
  client every 200 ms, then attaches and subscribes. Wait for `attached` in `H.views()` before a
  view action (restart.lua).

### Faults

`fault(spec)` arms a one-shot hub fault and returns its id. `fault_fired(id)` reports whether it
fired. A fault that has not fired at the end of the file fails the file. An unknown id is an error.

| Spec | Effect |
| --- | --- |
| `{point = 'spawn-create', mode = 'drop', op = key}` | `hub.spawn` sends no create request and gets `disconnected`. The spawn stays pending; no pane exists. |
| `{point = 'spawn-create', mode = 'refuse', op = key}` | No create request; Rex error `refused`. The spawn fails. |
| `{point = 'spawn-create', mode = 'lose-answer', op = key}` | The create request runs. If Rex answers yes, the answer becomes `disconnected`: the pane exists and the spawn stays pending. An accessory pane is not adopted by its label, so it stays pending (resolveSpawns only counts candidate panes). If Rex answers with an error, that error stays and the fault stays armed. |
| `{point = 'action', action, when = 'before', mode, key, plugin}` | A call of `action` from `plugin` (default: the plugin under test) with `args.key == key` (if given) runs no handler. `fail` replies code `failed`, or `fail_code` if given; `lose-reply` always replies code `disconnected`. |
| `{point = 'action', action, when = 'after', mode, key, plugin}` | The handler runs once. If it succeeds, its reply is replaced (`failed` or `disconnected`). If it fails, its own error is the reply and the fault stays armed. |

| `{point = 'action', action, when = 'before', mode = 'gate', key, plugin}` | A barrier for a caller that retries. From the call that fires it until `fault_release(id)`, every matching call gets code `unavailable` and runs no handler. The hub retries a view operation with that outcome every 500 ms, so the operation passes after the release. A gate that is still closed at the end of the file fails the file. `fault_state` reports `gated`, the number of refused calls. `fault_release` of a gate that has not fired is an error. |

- `nth = n` (action faults only) fires on the n-th matching call; the default is 1. Every armed
  fault that matches counts the call, also when another fault fires. An after fault counts only
  calls whose handler succeeded. A call that a closed gate refuses counts for no fault: it does
  not reach the hub.
- A hub request cannot wait for a test: Rex delivers the hub's requests one at a time, and every
  other caller waits behind a held request (rex-protocol.md, "The hub has one request queue"). So
  a barrier refuses calls (gate); it does not hold them.
- `op` matches the `hub.spawn` key. Only `hub.spawn` has the spawn-create point; `hub.spawn_window` does not.
- `action` must be a hub action that a plugin can call. `hub.test.*` actions are refused. A field
  that does not apply to the point is refused. Every field is a string, except `nth` (a positive
  integer).
- Action faults apply only to calls from a registered plugin. An agent call through the agent face
  is not affected.
- Limit: `lose-reply` and `disconnected` are injected error replies. The transport still delivers
  them. A test proves the behavior for that error code, not for a real lost connection.
- Faults exist only in test mode: a hub without `-test` does not declare `hub.test.fault`, and its
  dispatch refuses `hub.test.*`.

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
  `request_too_large`. Hub test replies stay under 48 KiB. A larger record or tree arrives in
  chunks of one revision; a change during the read starts it again. Nothing is truncated.
- **The hub has one request queue** (rex-protocol.md). No `hub.test.*` handler calls a plugin. The
  runner never answers a callback, so it polls instead: short sleeps, then a deadline.
- A render waits up to 25 s, which is longer than the hub's 10 s plugin call timeout. So a render
  that times out (for example a tree over 64 KiB) is reported as failed.

## Test mode in the hub

`-test` declares the `hub.test.*` actions and refuses them at dispatch without it. The hub refuses
`-test` with `-real-server`, and it refuses `-test` without a positive lab proof:

- the config server is `unix://<dir>/rex.sock`;
- `<dir>/lab.json` belongs to the current user and names that socket and a server pid;
- that pid is alive, and its command line has `--listen unix://<dir>/rex.sock` and
  `--state-dir <dir>/state`.

`lazyrex-hub test` and `lab/server.sh` both write `lab.json`. So a hub started by hand with
`-test` on a `lab/server.sh` lab passes the proof. `-test` also needs `test_roots` in the config
(see "Isolation"); without `-test`, the hub refuses `test_roots`.

## Harness self-tests

- `tests/harness/`: a fixture plugin and tests of the harness itself (agents, views, effects,
  limits, injections, assertions). Run `lazyrex-hub test tests/harness`.
- `tests/harness-fail/`: files that must fail, each for the reason in its first line.
- `tests/isolation/`: a probe plugin, loaded as the plugin under test and as a dependency, and a
  spawned worker. They report their environment, args and working directory. The tests also check
  the test-mode profile refusals.
- `TestHarnessIsolation` runs `tests/isolation` with an outer `HOME` and `XDG_CONFIG_HOME` in a
  temporary dir (never the real home) that hold a sentinel, and an outer secret variable. No lab
  process finds the sentinel through `HOME` or `XDG_CONFIG_HOME` or gets the secret. The outer
  `HOME` does not change, and no lab file contains the secret.
- `HARNESS_LAB=1 go test -run TestHarness ./cmd/hub` (in `hub/`) runs both, and also checks an
  empty `-run` match and an interrupt. It checks that no lab process survives.
