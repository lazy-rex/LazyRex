# Messaging tests

Start an isolated Rex server with `spikes/lab/server.sh /tmp/rexlab-messaging`.
Run the plugin tests from the repository root:

```sh
REX_SERVER=unix:///tmp/rexlab-messaging/rex.sock node spikes/03-ui-framework/tests/messaging.mjs
```

Set `MESSAGING_PLUGIN` to test a candidate Lua file. The test refuses sockets
outside a temporary directory. It runs the real Lua plugin against a controlled
hub. It does not test SQLite, tab rendering, or hub recovery.

The checks cover send retries, acknowledgement races, direct and group replies,
thread creation, membership, archive isolation, concurrent changes, unread
counts, tab declarations, informational messages, and read cursors.

## Thread tools

- `messaging.thread_list` lists the caller's threads and unread counts.
  Set `include_archived=true` to include archived threads.
- `messaging.thread_create` takes `members`, optional `title`, and `key`.
  The caller is always a member. One other member with no title returns the
  existing direct thread. A named or larger thread creates a group.
- `messaging.thread_archive` takes `thread`, `key`, and optional `archived`.
  The default is `true`. Set it to `false` to restore the thread.

Create and archive keys belong to the caller. Repeat the same key and content
when an outcome is unknown. A retry returns the saved result. An old archive
retry does not undo a newer restore.

Archive affects only the caller. It hides the thread from that participant's
active list and tabs. It preserves messages, drafts, unread state, and delivery.
New messages do not restore an archived thread. The Messages page lists archived
threads with a Restore action. Archive is not deletion or mute.

## Read and acknowledgement

`messaging.read` still returns unacknowledged messages without changing state.
Optional `thread` limits the result to a peer or group. The result includes
`next_cursor`, a decimal store revision. Pass it as `since` for later arrivals.
Keep a separate cursor for each filter. Omit `since` after interruption to recover
all unacknowledged messages. Revisions are compared as decimal strings, without
floating-point conversion. Messages are ordered by their committed revisions.

Set `reply_expected=false` on a send to mark it as informational. The history
shows a no-reply note. This flag does not suppress delivery or acknowledge the
message. Use `messaging.ack` for handled messages that need no written reply.

## UI integration requirements

The plugin declares unread counts in tab titles and supplies `tree.status`.
The renderer clears the status after the user's unread messages are acknowledged.
Archived threads disappear from the declared tabs. Their stored drafts remain.
The Archive action sets `closes_view=true`; the renderer must protect unsaved
composer text before it submits that action.

Observed on Rex 1203: the controlled-hub suite passes. The earlier group reply
check also accepted a direct reply sent to the caller instead of the original
sender. The suite reproduced that defect against the unchanged plugin. The
candidate restricts the group alternative to group recipients.


## Conversation identity adapter

Run the hook boundary tests without Rex:

```sh
python3 spikes/03-ui-framework/tests/session-start.py
```

Start an isolated server with `spikes/lab/server.sh /tmp/identity-lab`.
Build `hub/cmd/mcp` from the candidate checkout. Then run:

```sh
REX_SERVER=unix:///tmp/identity-lab/rex.sock MCP_BIN=/absolute/path/to/mcp node spikes/03-ui-framework/tests/identity-adapter.mjs
```

The test uses real Rex caller attribution and a controlled hub. It checks:

- The adapter joins its legacy mailbox before tool discovery.
- Discovery does not report a conversation.
- Each Codex call uses `_meta.threadId`, including after the conversation changes.
- Concurrent calls keep each report next to its dispatch.
- Calls without valid thread metadata do not use an inherited conversation ID.
- A rejected identity report does not stop legacy dispatch in stage 1.

Observed on Rex 0.0.1792158932-g88fd41: the adapter test passes.
The eight hook tests pass. They check explicit server selection, JSON arguments,
prompt recovery, irrelevant input, missing commands, missing hubs, ambiguous hubs, lost hub clients, and deadlines.
These tests do not verify the hub's identity state or lifecycle ordering.


### Combined hub and adapter

Start the candidate hub in an isolated lab with a static `claude` agent.
Run the transport test with the candidate adapter:

```sh
REX_SERVER=unix:///tmp/identity-lab/rex.sock MCP_BIN=/absolute/path/to/mcp node spikes/03-ui-framework/tests/identity-transport.mjs
```

The test creates its own pane and rebinds the lab's static `claude` agent.
It closes its session after the test. Do not use a shared lab agent.
The test delays input to an older real hook, completes a newer hook, and then releases the older input.
The hub must reject the older report as stale.
The test also calls `messaging_read` through the adapter and checks the resulting conversation identity.
Observed on Rex 0.0.1792158932-g88fd41: both checks pass with the stage-1 candidate hub.

`identity-lab.mjs` also accepts `IDENTITY_LAB` to select a separate fixture directory.
The directory must contain its lab server, candidate hub, and `fakeclaude` profile.

## Conversation identity migration and native actions

The messaging suite now checks identity migration through the real Lua plugin.
The controlled hub supplies complete alias data and paged namespace scans.
The suite checks these behaviors:

- Migration merges legacy and canonical unread mail in one revision-checked batch.
- A conflicting write causes a rescan before migration commits.
- Historical message IDs and revision cursors stay unchanged.
- Groups, direct threads, archives, and drafts use canonical conversation IDs.
- Old send and thread operation keys return their original results after migration.
- Different nonempty drafts cause an explicit migration conflict. Neither draft is deleted.
- Only the hub can assert the original requester through `hub.forward`.
- Direct plugin calls still use the caller PID that Rex supplies.

The worktree suite checks the same forwarding boundary and its existing recovery cases.

Run the combined test with an isolated Rex server and matching hub and view binaries:

```sh
REX_SERVER=unix:///tmp/native-lab/rex.sock HUB_BIN=/absolute/path/to/hub VIEW_BIN=/absolute/path/to/view node spikes/03-ui-framework/tests/identity-native.mjs
```

The test owns its hub and three agent panes. It uses actual `rex -C lazyrex do` commands.
It checks legacy mailbox migration, original cursors, old-key retries, and a caller that never joins through MCP.
It also checks clear/resume isolation and a hub kill followed by restart.
The `lazyrex` client must disappear when the hub dies, so the restarted label is unambiguous.
Observed on Rex 0.0.1792158932-g88fd41: the messaging, worktree, and combined native suites pass.

## Profiles directories

Start `spikes/lab/server.sh /tmp/rexlab-profiles-codex`, then run:

```sh
node spikes/03-ui-framework/tests/profiles-directories-lab.mjs
```

`PROFILES_LAB` can name another `/tmp/rexlab-profiles-*` directory. The test refuses
other socket paths. It uses the actual Rex Lua runtime and terminal methods, and
creates Git repositories only inside the lab. It closes its two sessions and
keeps the fixture directory for inspection. No hub or renderer is required.
URI rejection cases substitute only the `pwd` response; all directory listings
and Git queries use the real tools. Twelve checks passed on Rex
`0.0.1792158932-g88fd41` (2026-10-17).

The hub's spawn focus and malformed-success checks use the same isolated server:

```sh
cd spikes/03-ui-framework/hub
SPAWN_LAB_SOCKET=/tmp/rexlab-profiles-codex/rex.sock go test ./cmd/hub -run 'TestSpawn(Focus|Malformed)' -count=1 -v
```

The tests create their own sessions. They exercise the real spawn path and Rex's
server focus. A test seam can lose a create reply or move focus before the reply
returns. They do not connect a Rex app or claim app navigation behavior.


### Start forms and spawn environments (Profiles pair, 2026-10-17)

```sh
spikes/03-ui-framework/tests/profiles-lab-setup.sh /tmp/rexlab-profiles-<you>
```

The script builds the hub and the renderer into the lab and writes `init.lua`, `hub.json` (plugins
`lazyrex` and `profiles`, `palette_lua`) and `profiles.json`: `fake_claude` (agent, arg `task`;
prints `agent task=<task>` and its directory) and `logtail` (accessory; prints its directory). It
creates the fixture `repo` (Git, `main`) with the worktree `repo-wt` (`feature`) and the folders
`repo/sub/deeper`. It prints the commands that start the server, the hub and the two tests.

- `profiles-lab.mjs`: the forms end to end with keys. It takes each form pane and resizes it to
  90x80, so one capture shows the whole form. It destroys its sessions. Cases: the agent form with
  a worktree choice, the accessory form with a typed directory and Split, `~` typed into Directory,
  typed text and then a worktree choice (the choice wins through the field `rev`), and a broken
  profiles file. `rex send` types text that `send-key` has no key for, such as `~`.
- `profiles-plugin.mjs`: the real plugin in Rex Lua with a controlled hub (in-memory store,
  scripted `hub.spawn` results): pending start, replay with changed values and context, Check
  again, failure, a broken profiles file while pending, shared arg fields, an explicit directory
  choice after a failed start (field value and `rev`), directory text decoding, an old draft with
  `browse` and `prefix` (it loads; the old browser actions are refused).

Both passed in a fresh lab made by the script on Rex `0.0.1792158932-g88fd41`.

`tests/spawn-env-lab.mjs` needs a lab server started with stale variables
(`LAZYREX_AGENT=stale-agent CODEX_THREAD_ID=stale-thread`) and a hub on `env.json` with the plugin
`tests/spawnprobe.lua` and the agent profile `envdump`. It checks that the built-in shell has
neither variable and that an agent has its fresh agent ID. It stops its workers.

## Tunnels lab

`tests/tunnels-lab.mjs` runs the Tunnels plugin end to end. It needs a lab server and a lab hub.

```sh
LAB=/tmp/rexlab-tunnels-claude; W=$PWD/spikes/03-ui-framework
mkdir -p $LAB/int; echo 'pcall(require, "lazyrex_palette")' > $LAB/init.lua
# hub config: lazyrex and tunnels (events "all"), state $LAB/int, palette_lua $LAB/lazyrex_palette.lua
# Fake mode (no network). XDG_CONFIG_HOME keeps the route files in the lab.
XDG_CONFIG_HOME=$LAB/xdg LAZYREX_TUNNELS_OPENTUNNEL=$W/tests/fake-opentunnel.sh FAKE_OT_DIR=/tmp/fake-opentunnel-lab \
  LAZYREX_TUNNELS_PROFILE_PREFIX=lazyrexlab- LAB_CONFIG=$LAB/init.lua spikes/lab/server.sh $LAB &
$W/dev.sh $LAB/hub.json &
node $W/tests/tunnels-lab.mjs
```

Real mode opens one public tunnel to a 127.0.0.1 test server (the maintainer's approval, 2026-10-17).
Start the lab server without `LAZYREX_TUNNELS_OPENTUNNEL` and without `XDG_CONFIG_HOME`, then run with `TUNNELS_REAL=1`.
A new profile waits about a minute for certificate verification.
The test deletes the `lazyrexlab-<port>` profile at the end and checks that no launchd plist remains.

Fake mode also runs step 9: "Other port…", an `up` that fails after it registered the service, a block closed between `up` and `down` (the plugin runs `down` afterwards), and a failing `serve`. The fake models the launchd registration as `$FAKE_OT_DIR/<profile>.service`; the flag files `fail-up`, `slow-up` and `fail-serve` in `$FAKE_OT_DIR` select a failure.

Step 10 (fake only) checks cleanup without an open panel: the session with the only panel closes while `up` runs, and a `down` that fails (flag `fail-down`) is retried across a plugin restart.
Step 11 (fake only, flag `slow-down`) checks that a running cleanup `down` owns its profile: a Forward of the same port is refused, and a plugin restart starts no second `down`.

Observed on Rex 0.0.1792158932-g88fd41 with opentunnel 0.5.2, 2026-10-17: in fake mode, all 16 checks (steps 1 to 11) pass three runs in a row on the merge of `integ` (2bd07e1). Real mode passed steps 1 to 8 before steps 10 and 11 and the cleanup changes; the coordinator and Codex asked for no more real runs.
On `panels` be65f63, one fake run stopped at 9b with a new panel pane on "Waiting for the hub". It did not happen on `integ`. On a failure the test prints the screens of all `lazyrex-form:*` panes.
Not tested: an unknown `session.new_block` result (adoption needs the argv proof; no lab fault exists for it), a missing opentunnel binary, and a concrete non-loopback listen address.
