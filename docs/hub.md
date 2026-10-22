# Spike 3: UI framework

> Note: this document was written during the spike phase. Paths such as `spikes/03-ui-framework/`
> refer to the original research repository. In this repository, that code is at the root
> (`hub/`, `view/`, `plugins/`, `tests/`), and each plugin has its own repository.


**Question:** can the hub drive Rex as the navigation layer for plugin TUIs? Every pane runs at a route, the hub keeps a plugin session's tabs equal to the declared set, and navigation moves every connected app.

Design: `docs/architecture.md`, "UI framework (proposed)". Check the Rex agent guide (`rex __internal skill`) for documented layout and app features.

Lab only (`spikes/lab/server.sh`). Nothing runs on the real server.

## Ownership

| Part | Owner | Path |
|---|---|---|
| View contract and renderer (`lazyrex-view`) | Codex | `view/`, section "Renderer contract" |
| Hub: views, routes, reconciler, navigator, renderer check-in | Claude | `hub/` (copied from spike 2) |
| Messaging plugin: view, draft and send actions | Claude | `plugins/` (copied from spike 2) |

## Renderer contract

Status: **approved by Claude with changes, 2026-10-16**. Implementation is in progress.
This section defines schema version `1`. Claude owns the hub and plugin side.
Codex owns the renderer and the schema types under `view/`.
The first proof uses the real renderer, hub, and messaging plugin in a lab.
Custom renderers use the same interface. Renderers never change Rex layout.

### Transport and admission

Launch: `lazyrex-view -view-id <id> -server <endpoint> [-real-server] <plugin> <route>`. `-server` is the hub's own endpoint (`server` of hub.json, as loaded). A Linux Rex pane has no `REX_SERVER`, and the renderer never guesses a socket: without `-server` it takes `REX_SERVER` (a renderer that an older hub started, after a reload), with neither it refuses, and a `-server` and a `REX_SERVER` that name different sockets refuse. This argv is an interface between two binaries of one release: an older renderer refuses `-server`, so the hub and the renderer ship together.
The hub passes the view ID and its endpoint (`-server`) as arguments.
The lab launcher sets `REX_SERVER` before it starts Rex.
`route` is the canonical path, for example `/thread/42`; `plugin` is `messaging`.
The hub records this pair before it starts the pane.
The renderer requires its inherited `REX_SESSION` and `REX_BLOCK`.
The spike refuses the real Rex socket.

The renderer opens a Rex control connection and declares `view.changed` through `client.keymap`.
It resolves `lazyrex-hub` to exactly one connected client and uses that full client ID.
All calls below use `client.perform`; their JSON arguments appear in `args`.
The receiver returns application results through `client.reply.result`.
Neither side trusts the optional Rex request fields `session_id` and `block_id` as caller proof.

1. Call `hub.view.attach {view_id, schema_version: 1}` from the renderer's pane.
2. The hub checks caller ancestry against the stored pane and checks that the view is open.
3. The hub binds the current client to the view and the user context.
4. The hub returns `{view_id, plugin, route, user, hub_generation, attachment}`.
5. The renderer checks the returned plugin and route against its launch arguments.
6. Call `hub.view.subscribe {view_id, attachment}` to subscribe and receive the first snapshot.
7. Use `hub.view.get {view_id, attachment, route}` for later fetches.

`attachment` is an opaque token for this attachment, not a security credential.
Every later call includes `view_id` and `attachment`.
The hub checks both against the Rex caller's client ID.
A new attachment replaces the old attachment. Old calls receive `stale_attachment`.
The hub derives the plugin, user, and action authority. A renderer cannot supply another user.
The renderer can start before the hub records the pane returned by `session.new_window`.
An attach error with a message that starts with `not_ready` is a temporary startup condition.
The renderer retries it every 100 ms for up to three seconds.
Other definitive attach refusals stop attachment and show an error.
Transport failures remain uncertain and use reconnect backoff.

### Snapshots and invalidation

`hub.view.subscribe` and `hub.view.get` return `{schema_version: 1, view_id, route, revision, tree}`.
`revision` is a decimal string. It increases within the current `hub_generation`.
The renderer compares revisions numerically, without converting them to floating-point values.
A hub restart requires a new attachment, subscription, and snapshot.

The hub returns its current snapshot without waiting for a plugin callback.
If no snapshot exists, it returns a tree with a loading node and schedules plugin work.
Plugin output replaces the snapshot only if it belongs to the current view and attachment generation.
The hub schedules this work outside the Rex request handler.
The plugin can then call hub storage without a nested call cycle.
After a store commit in its namespace, the hub renders the plugin's open views again: every view,
or only the views whose declared dependencies match ("Render dependencies", below).
A plugin calls `hub.ui.invalidate {routes?}` for changes outside storage.

The hub calls `view.changed {view_id, attachment, revision}` on the renderer after a snapshot changes.
The renderer checks the requester against the current hub client ID.
It records the highest announced revision and immediately replies `{received: true}`.
It fetches outside the callback handler. It coalesces repeated invalidations into one pending fetch.
A callback means “fetch current state”; it does not contain a tree patch.
A failed callback remains pending at the hub until delivery or attachment replacement.
The hub must not wait for a callback while it answers a renderer request.

Subscribe completes only after the subscription is installed.
The returned snapshot includes changes made before subscription completion.
If a callback arrives during a fetch, the renderer fetches again when its snapshot is older than the announced revision.
A revision gap requires a full fetch, not replay of intermediate revisions.
A disconnected renderer retains its local text and shows that the hub is unavailable.
It resolves the current hub and repeats attachment, subscription, and fetch when the connection returns.

### Render dependencies

A `view.render` reply can add `"deps": {"prefixes": [...], "changes": [...]}` beside `tree`
(renderdeps.go). The hub then renders the view again only for the triggers that it declares.

- `prefixes`: a committed key (written or deleted, from `hub.store.batch`) marks the view when it
  starts with a prefix. `""` matches every key. Missing (or no `deps`): every commit of the plugin
  marks the view. An empty list: no commit marks it. A batch without record writes (notices only)
  marks only the views without prefixes. A conflict and a replay of a saved op commit nothing.
- `changes`: a subset of `agents`, `plugins`, `workers` and `profiles`. The hub renders every view
  of every registered plugin on such a change. Missing: as that. An empty list: no change marks the
  view. The plugin still gets `hub.changed {what}`, so it can call `hub.ui.invalidate {routes}`.
- Rex Lua encodes an empty table as `{}`: the hub reads `{}` as the empty list, so
  `deps = {prefixes = {}}` is the explicit empty list.
- Malformed deps (not an object; a list that is not strings; more than 64 prefixes; a prefix over
  512 bytes; an unknown name in `changes`; an unknown field) count as no declaration. The tree is
  shown, and the event `view_deps_invalid {plugin, view_id, route, error}` comes when the error text
  changes.
- What to declare: each key that the render reads with `hub.store.get` (a key as a prefix also
  matches longer keys: only extra renders), and the prefix of each `hub.store.scan` (a scan without
  a prefix needs `""`). A missing prefix gives a stale view until another trigger. `calls()` in a test
  shows the reads (testing.md).

The other triggers do not use the declaration:

- Every open view of the plugin: plugin registration, `hub.ui.invalidate {}` and a tab status
  change.
- The open views of one tab: a navigation to that tab.
- Only that view: `hub.ui.invalidate {routes}` (views whose route equals one of 1 to 64 non-empty
  strings), a new attachment, a view without a snapshot, a pair layout change, a transcript change,
  the end of one of its operations (which also sends `view.changed` when the tree is unchanged),
  and `hub.test.view.render`. Before render dependencies, each trigger in this list except the
  first, and a navigation, rendered every view of the plugin.

Lifetime and races:

- A declaration belongs to the plugin instance that rendered it. A new registration clears all
  declarations of the plugin, and a result from before it installs nothing.
- A failed render (an error, no tree, or a transient failure) and a malformed declaration leave the
  view with no declaration until its next good render. A closed view loses its state; a render of
  it that ends later installs nothing. A hub restart starts with no declarations.
- The hub takes a view's causes before it calls `view.render`. While that render runs, every
  trigger marks the view without the filter (the hub does not know the declaration that the
  running render gives), and the view renders again after it. So a commit that changes the
  declaration is never lost. There is at most one pending render for each view.

### Render IDs

The hub gives each `view.render` call a new render ID in `args.render` (a string; "r-" and 128
random bits in hex; renderreads.go). The ID is open from just before the call until the call
returns, for any outcome. Then it is closed, and the hub never makes it again.

- The plugin sends the ID with each `hub.sql.query` of that render (the SDK does it). The ID is a
  read-only token: every other `hub.sql` action refuses an argument `render`, and so does a query
  with `tx` (`refused`, "a render cannot write"). See docs/sql.md section 9.
- A plugin that does not know the field ignores it: the view.render arguments allow extra fields.
- The hub records the tables that each tokened query of the render reads (SQL read tracking,
  below).

### SQL read tracking (the default for each render)

Read tracking is the default. A `view.render` reply can add `"refresh"` beside `tree`
(renderreads.go). It is not part of the tree, and the renderer never sees it.

- Absent or null: the render is tracked. The hub gives the view the tables that the render read,
  if the render succeeded, stayed tracked, and read at least one table. Then a SQL commit renders
  the view again only when it changed one of those tables, or the schema.
- `"all"`: the opt-out. Every SQL commit of the plugin renders the view again. A view sends it
  when it shows state that a SQL commit does not change and that it does not invalidate. The SDK sends it for every render in which a hub call failed (also a
  caught error, or a call that never reached the hub): the hub cannot see such a call, so absence
  would track an incomplete read set.
- Any other value counts as `"all"`. The hub logs the first such value of each view, also for a
  render that is untracked or read no table (at most 64 lines in all): "view <id>: unknown refresh
  <value>; using all".
- A render that read no table, an untracked render (another hub call than a tokened
  `hub.sql.query`, a failed query; docs/sql.md section 9), and a failed render get no table set:
  every SQL commit renders the view again.
- Each render states it again. The hub keeps no state between renders: a new registration or a
  hub restart drops the table sets, and the next render installs its own.

A render is tracked when the hub knows each table that it read. It is untracked when, while it is
open, the plugin makes any hub call that is not a `hub.sql.query` with the open render ID of the
same plugin instance (for example a query without `render`, a closed or unknown ID, another
plugin's ID, `hub.store.get`, `hub.store.scan`, `hub.store.ops_scan`, `hub.agents`). Each such
call makes every open render of the plugin untracked; the views of renders that ended keep their
tables. A new hub action counts as such a call unless it is a `hub.sql.query` with `render`. A
render is also untracked after a query with its ID fails with an error other than `conflict`,
also before the statement runs (invalid arguments, a database that does not open): the hub cannot
know the tables, and after it reads a `sqlite_` table other than
`sqlite_schema` (`sqlite_sequence` and `sqlite_stat1` change without a mark).

Marks and races (the rules of "Render dependencies" apply):

- A SQL commit marks a view in flight, whatever tables its render then gives, so the view renders
  again after it. A schema change marks every view of the plugin.
- A new registration clears every table set of the plugin; a render from before it installs
  nothing. A closed view loses its set. A failed or transient render clears the set.
- The table set and the store `deps` are separate inputs: the set filters SQL commits only, and
  `deps.prefixes` filters store commits only. The other triggers do not use the set.
- `hub.test.renders` shows `sql_tracked` (true when the view has a table set) for each view. It
  shows no table names.

The home plugin reads no table: each render calls hub actions, so it is untracked. Its views
render again through `hub.changed` marks (agents, workers, plugins, profiles). The launch notice
is Lua state of the plugin: a change of it calls `hub.ui.invalidate` for the Agents… panel. The lab
test `plugins/lazyrex/tests/refresh.lua` checks each path without a render request.

### Version 1 view tree

The root is an object with `title`, `back`, and ordered `nodes`.
`back` is an action descriptor or `null`.
Every node has a nonempty `id`, a `type`, and an optional `label`.
IDs are unique within the tree and remain stable while the logical control remains the same.
An action descriptor is `{name, label, enabled}`.
The hub validates each action against the plugin's current declaration.
Text is plain text. The renderer does not execute escape sequences in plugin text.
Unknown optional fields are ignored. An unsupported schema version produces a visible error.
An unknown node type produces an error at that node and disables its actions.

| Node type | Additional fields | Behavior |
|---|---|---|
| `list` | `items: [{id, label, detail, action}]` | Select one stable item ID. |
| `history` | `messages: [{id, author, text, at}]`, `before`, `has_more`, `page_size`, `load_action` | Messages are oldest first. `before` is an opaque cursor or `null`. |
| `composer` | `text`, `draft_revision`, `save_action`, `send_action` | The first snapshot supplies the saved draft. Later snapshots do not overwrite dirty local text. |
| `form` | `fields: [{id, label, kind, value, required, options}]`, `submit_action` | Version 1 kinds are `text`, `multiline`, `boolean`, and `select`. Options have `value` and `label`. |
| `widget` | `text`, `action` | Show a value and an optional action. |
| `loading` | `text` | Show progress without accepting domain actions. |
| `error` | `text`, `retry_action` | Show an understandable error and an optional retry action. |

Arrays must encode as JSON arrays, including empty arrays.
`page_size` is a positive integer selected by the plugin, not an unbounded renderer request.
A history page contains at most `page_size` messages.
A load action receives `{before}` and replaces the displayed history page.
The renderer preserves the selected message when it occurs in the new page.
It does not accumulate all history pages in memory.

The root may also have an optional `status`: `{state, kind, msg}`.
`state` is `idle`, `working`, `done`, `blocked`, or `error`. `kind` (`permission`, `question`, or `auth`) applies only to `blocked`. `msg` is one line of plain text.
The renderer reports the status to Rex with the Program Status Protocol (OSC 7501, `app=lazyrex`). Rex shows it on the tab header when the tab is not focused, and in the session picker (Rex docs; not yet observed).
The renderer reports on its first snapshot and then only when the status changes. A missing status or an unknown state clears the record. A closed view clears it; a reload keeps it.
The plugin chooses the text and the state. For example, a thread with unread messages for the user has `{state = "done", msg = "3 unread from Codex"}`.

A plugin may declare its tabs again to change only a title (for example "Team (3)"). The hub renames the open tab with `session.set_window_label`, and keeps the tab's closed state and crash count.

The renderer retains text, cursor, focus, and scroll state by node ID across revisions.
A node type change invalidates the old control state.
A removed dirty control retains its unsaved text in a recovery state until the user resolves it.
A new snapshot never silently discards that text.

### Actions and operation results

Call `hub.view.action` with:

```json
{
  "view_id": "view:example",
  "attachment": "opaque-attachment",
  "node_id": "threads",
  "action": "open",
  "revision": "12",
  "operation_key": "renderer-generated-unique-key",
  "input": {"item_id": "42"}
}
```

The root Back control uses the reserved node ID `$back`; plugins cannot assign that ID.
Form submission uses `input.values`, keyed by field ID.
A list uses `input.item_id`. A history action uses `input.before`.
Widget and Back actions use an empty `input` object.
The reserved renderer action names are `open`, `load`, `submit`, `retry`, and `$back`.
The hub forwards these names unchanged to the plugin action `view.action`.
Its arguments are `{route, node_id, action, input, user, operation_key}`.
The hub forwards draft saves to `view.draft.save` with
`{route, node_id, user, operation_key, base_revision, text}`.
The hub forwards sends to `view.send` with
`{route, node_id, user, operation_key, draft_revision, text}`.
The plugin declares `view.action`, `view.draft.save`, and `view.send` through `client.keymap`, with argument schemas.
The hub supplies `user` from the renderer binding, never from renderer input.

The hub validates the view, node, action, and input before admission.
Admission durably records the operation and returns `{status: "accepted", operation_key}`.
The hub executes plugin work after it returns this response.
After admission, the renderer waits for `view.changed`.
It then calls `hub.view.operation {view_id, attachment, operation_key}` once for each pending operation.
It does not poll admitted operations during a healthy connection.
No handler waits for a plugin that can call back into the hub.
The hub issues a view invalidation when an operation completes.
The renderer also queries a pending operation after reconnect or an uncertain result.
Recovery queries use backoff with jitter. `client_unavailable` also requires backoff with jitter.
All renderers, agents, and plugins share the hub's Rex queue, which permits 16 waiting requests.

Operation lookup returns one of:

- `{status: "pending", operation_key}`: the operation was admitted but has no final result.
- `{status: "succeeded", operation_key, result}`: the recorded result is final.
- `{status: "failed", operation_key, error: {code, message}}`: the recorded failure is final.
- `{status: "not_found", operation_key}`: no durable admission exists.

A retry keeps the same key and identical semantic arguments, including the original revision.
Attachment tokens can change during recovery; they are excluded from the operation fingerprint.
The hub scopes keys to `(plugin, user, key)`, and checks existing operations before checking the old view revision.
A renderer keeps its operation key when a replacement view reconciles the same operation.
The same key with different semantic arguments returns `operation_conflict`.
A missing operation permits a retry with the same key; it does not prove that an earlier request cannot still arrive.
Durable admission and plugin storage deduplication must handle that race.

Transport timeout or disconnect means the outcome can be unknown.
The renderer never treats either condition as a successful mutation.
Application errors include `not_ready`, `stale_attachment`, `stale_view`, `draft_conflict`, `operation_conflict`, `unavailable`, `invalid_input`, and `unsupported_schema`.
The UI shows the error message and retains unsaved input.
The hub must not label a partially executed mutation as definitively failed without reconciliation.

### Draft save

Call `hub.view.draft.save` with the common view fields plus:
`{node_id, operation_key, base_revision, text}`.
`base_revision` is the saved draft revision, not the tree revision.
A missing draft has revision `"0"`. Revisions must not reset after a send clears a draft.
The hub admits this operation through the same asynchronous protocol as actions.
The plugin stores drafts by the bound user and thread.
It writes only when the stored revision equals `base_revision`.
Success returns `{draft_revision, text}` through operation lookup.
A conflict returns `draft_conflict`; the renderer fetches current state without overwriting local text.
The UI requires explicit resolution before it overwrites another view's draft.

The renderer permits one save in flight per composer.
Each edit updates local text immediately and queues the newest text for saving.
When a save completes, the renderer compares the saved text with its current text.
It shows “Saved” only when those texts match and no save remains pending.
Otherwise, it shows “Unsaved” and saves the newer text against the returned revision.
The renderer retains the exact request for an uncertain save until it can reconcile that operation.

A native close can kill the renderer before its latest save completes.
Only acknowledged saves have a durability guarantee in this spike.
Unsaved text survives a hub outage while the renderer stays alive.
Renderer or Rex process loss can lose unsaved text; a local journal is not part of this draft contract.

### Send

Call `hub.view.send` with the common view fields plus:
`{node_id, operation_key, draft_revision, text}`.
The renderer first saves the text selected for sending and obtains its draft revision.
It then fixes that text and revision in the send request.
Later typing remains local until this send reaches a known outcome.
Enter sends. Shift+Enter inserts a newline when the terminal reports distinct keys.
Visible Send and Back controls remain available to mouse and touch users.
If key negotiation cannot distinguish Shift+Enter, the UI reports that limitation instead of claiming newline support.

The plugin checks the draft revision and text before sending.
In one storage transaction, it creates the message, updates the mailbox and notice intent,
records the operation result, and clears the matching draft with a new revision.
If the current draft differs, the transaction fails with `draft_conflict` and creates no message.
Success returns `{message_id, cleared_draft_revision, draft_revision}`.
A repeated operation returns its recorded result even when the draft has changed since the first send.

The renderer clears its composer only if local text still matches the sent text.
It preserves newer local text and saves it against the revision returned by the completed send.
A lost reply requires operation lookup or a retry with the same key and content.
It never creates a new send key merely because a reply was lost.

### Review gate and verification

Claude approved the contract on 2026-10-16 after five changes. Those changes are included above.
The main review point is the deferred operation protocol, which prevents hub-plugin call cycles.
The hub owns admission, caller checks, snapshots, and durable operation results.
The plugin owns domain validation, draft revisions, and the atomic send transaction.
The renderer owns local input and presentation state.

Verification must cover the real entry points, not only schema decoding:

- Attach from the expected pane; reject a different pane and a replaced attachment.
- Change a view between subscribe and fetch; show the newest revision without resetting input.
- Save from two views; report a conflict without losing either view's local text.
- Lose a send reply; recover one message and preserve text typed after submission.
- Restart the hub; attach again and recover pending operations without duplicate effects.
- Close a renderer; restore its acknowledged draft in a replacement renderer.
- Exercise Enter, Shift+Enter, mouse controls, narrow layouts, and text selection in a lab terminal.

The full first proof remains below. Touch behavior requires a real supporting Rex client.

## First proof

The messaging home and two thread tabs. Checks: duplicate open, native close, renderer crash, hub restart during a spawn and during a send, draft restore, continuous layout drift, two apps with different sizes.

## Results

### Hub reconciler, step 1 (Claude, lab, Rex 1203, 2026-10-16)

`hub/cmd/hub/ui.go`, test plugin `tests/uitest.lua`, stub renderer `hub/cmd/stubview`, test `tests/reconcile.sh`. All 10 checks pass:

- A plugin declares tabs (`hub.ui.tabs`). No session exists before the first navigation.
- `hub.ui.navigate` creates the plugin session (`lazyrex:<plugin>`), opens every declared tab in order without focus, and each stub renderer checks in (`hub.view.attach`) from its pane.
- Tab order drift (a `move_window`) snaps back.
- A native tab close closes the view only. The tab stays closed until a navigation to it, which reopens it in its declared place.
- A tab the plugin stops declaring closes.
- A renderer that exits gets a new view in the same place, and the new renderer checks in.

Found while testing:

- A renderer can start before the hub records its pane. `hub.view.attach` returns `not_ready`, and the renderer retries (added to the contract).
- Event handlers closed tabs while the worker reordered them (`move_window: not_found`). Now handlers only record state, and one worker per plugin makes every Rex change.
- A stopped hub or plugin stays listed as lost under its label during Rex's grace period, so a label lookup returns `ambiguous`. Renderers count only connected candidates. Tests address clients by full ID.
- After 3 renderer exits, a tab stays closed until a navigation (bounded respawn).

### View services and messaging views, first integration (Claude, lab, Rex 1203, 2026-10-16)

`hub/cmd/hub/viewsvc.go` (snapshots, subscribe, durable operations, view.changed), messaging plugin views (`view.render`, `view.action`, `view.draft.save`, `view.send`), with Codex's renderer `/tmp/lazyrex-view-spike3`. Observed by screen capture and the hub event log:

- The inbox tab renders the plugin's conversations. A thread tab renders Back, history, the composer and Send.
- Typing in the composer saved drafts through the hub (store revisions 1, 2) and showed "Saved".
- Enter sent the message. One store transaction wrote the message, the recipient's mailbox and notice, the thread index, and the cleared draft. The history showed the message, and the composer cleared.
- Rex Lua encodes empty lists as `{}`. The hub turns empty objects in list fields into `[]`, and adds `back` and `before` as `null` when Lua drops them.

- Back and inbox Open, through real renderer input (Codex): the hub recorded `{navigate: home}` and `{navigate: thread/codex}`.
- Hub restart with open views (twice): every open view kept its view ID, pane and renderer. A plugin restart declares its default tabs again, so a tab added only by a test hook closes.
- After a restart, the hub clears all attachments. Two of three renderers attached again without input. One, running Codex's earlier build, did not.
- Codex's current renderer (lab, 2026-10-16):
  - Two views editing one draft: view A saved; view B kept its own text, showed `draft_conflict` with A's saved text, and its "Keep my text" action saved against the current revision.
  - Renderer killed: the hub opened a replacement view, which showed the saved draft.
  - Idle hub restart: the same renderer attached again with no terminal input and showed its saved draft. The first run took 6.8 s (exponential backoff, 250 ms doubling to 8 s). Codex's renderer now wakes on the hub's client events, with a 2 s backoff cap: the second run attached again 173 ms after hub start.
  - Codex also fixed a renderer bug: after a restart, the hub's first snapshot is "Loading" (snapshots are in memory only), and that snapshot pushed unsaved composer text into recovery. The renderer now keeps its controls during a loading-only snapshot. Proposed hub improvement: keep snapshots across restarts.

Fixed after Codex's review:

- A replay after a lost reply returned the current draft revision, not the one the original save or send produced. A renderer could then overwrite a newer draft without a conflict. Now `hub.store.batch` takes `op.result_revs {field: record key}`: the store writes the committed revision into the saved operation result in the same transaction, and a replay returns that result exactly.
- Startup race (found by Codex with the faster renderer recovery): a renderer attached 19 ms before the plugin registered. `view.render` failed with `client_unavailable`, and the hub cached a permanent Error tree. Now a transient render failure keeps the current tree (Loading on a fresh attach), and `plugin_registered` renders the plugin's views again. Only a definite plugin error renders Error. Codex verified it in the lab: same renderer, correct screen, no input.
- Startup race (found by Codex in the source, not reproduced): the hub set its `lazyrex-hub` label before it declared its actions. A renderer woken by the label could get `not_supported` from `hub.view.attach`, which the contract treats as final. Now the hub sets the label after `client.keymap`.

Hot reload (lab, Rex 1203, 2026-10-16): run `./dev.sh <hub config>`. It runs the hub and polls the sources once a second.

- `plugins/<name>.lua` changed: `hub.admin.stop` and `hub.admin.start`. Plugin registration renders the open views again. Observed: the thread view showed the new plugin's render.
- `view/*.go` changed: `go build`, then `mv` over the configured renderer path, then `hub.ui.reload`. The hub sends `view.reload {view_id, attachment}` to each attached renderer. The renderer saves its drafts, waits for open operations, exits Bubble Tea, and calls `exec` on its startup path. Same PID, same pane, new attach. Observed: 3 renderers, PIDs unchanged, 3 new attaches within 12 ms, Codex's unsaved draft kept. The renderer refuses to reload with a draft conflict, a dirty form or removed controls with text, and says why. An old renderer without `view.reload` answers `not_supported` and keeps running.
- `hub/**/*.go` changed: build, then restart the hub. Renderers and plugins attach again. Observed: all views attached again.
- Rex has no action that restarts a pane's process (Rex 1203 API reference), so `exec` is the renderer's job.

Group thread (the maintainer's request, 2026-10-16; lab, then live on the real hub): `messaging.lua` has groups. The group `team` (title "Team") has the whole roster as members. It is a pseudo-peer, so the thread route, the tab and the inbox entry work unchanged. A message to `team` goes into every other member's mailbox in one transaction, and into one group thread record. `read` and `ack` accept a message addressed to the agent or to its group. A reply with `reply_to` in the group acknowledges the original. Observed in the lab: Claude to team reached Codex and the user, not Claude; Codex's reply reached Claude and the user and cleared Codex's mailbox entry; a send from the user's Team composer reached Claude and Codex. The inbox counts group messages under Team.

Created threads (the maintainer's request; lab, then live): Messages has a "New thread" form, an optional title and a checkbox per participant (a v1 `form` node). Submit makes a group of the user and the chosen participants. The group lives in the record `groups`, and its ID comes from the operation key (`g-...`), so a replay finds the group instead of making a second. The plugin declares the new tab and returns `{navigate}`. One participant and no title opens the existing direct thread. Agents send to a group ID; only members can. Observed in the lab: "Design" was created, its tab opened and attached, and Claude's message reached Codex and the user, not Claude. Threads have no Back control: the Rex sidebar lists them.

Read state (proposed; hub and plugin done, renderer in progress by Codex): the renderer turns on focus reporting (DECSET 1004). On focus-in, and on each new snapshot while it has focus, it calls `hub.view.seen {view_id, attachment, reason}`. The hub forwards `view.seen {route, user, reason}` to the plugin in the view worker. `reason` is `focus` (the pane got focus, or a navigation showed it), `content` (new messages while focused), `connect`, or empty (an older renderer). Queued reports coalesce and `focus` wins. A plugin can refresh its data on `focus` without a refresh/render loop. A registered plugin calls `hub.ui.invalidate {}` when its own data changed outside the store (for example a scan finished): the hub renders its open views again. Messaging then acknowledges the user's unread messages in that thread. A background tab gets no focus-in, so it marks nothing read. Not verified: that Rex forwards focus-in and focus-out to a pane.


Fixed while testing hot reload: renderers that exited while no hub ran left their views `open` forever, because nobody got `child_exited`. Now the reconciler checks each open view that is not attached: a pane with no child closes the view, and a new view opens. Observed: two such views were replaced after a hub restart.

Not tested yet: a spawn whose outcome is unknown, navigation on a real app (the lab has no app), agent replies arriving in the user's thread.

Gallery (Claude, lab then real hub, Rex 1203, 2026-10-16): the messaging plugin adds a `Gallery` tab at route `/gallery`. The gallery shows every renderer control live: a widget, a list with a disabled item, a form (text, multiline, two booleans, select), sample Markdown messages and a composer. Gallery state, history and draft are in the records `gallery/state`, `gallery/history` and `gallery/draft`. Gallery actions do not create messages or threads. Observed in the lab: a mouse click on Apply stored `submitted form: name=Ada, notes=, opt_a=false, opt_b=true, size=medium`; a composer send appended to `gallery/history` only. Observed on the real hub: the Gallery view attached after the hot reload.

## Profiles and workers

A **profile** is a named command, for example "Rails Server" or "Codex". A **worker** is one running copy of a profile: the hub starts it in a new tab of the hub's session. Status: lab only (Claude and Codex, Rex 1203, 2026-10-16). Not deployed.

**Profiles file.** The config key `profiles` names a JSON file `{"profiles": [...]}`. Each profile has `id` (`^[a-z][a-z0-9_]{0,31}$`), `name`, `kind` (`agent` or `accessory`), `command` (argv), optional absolute `cwd` and optional `env`. Env names that start with `LAZYREX_` are refused. Optional `args` declares named values, for example `{"name": "task", "label": "Task"}` with `"command": ["claude", "${task}"]`. A spawn passes `args: {"task": "..."}`. Each `${name}` must be a whole command argument (not the program), and the hub replaces it with the value as one argument: no shell runs, so any text is safe. An arg with no value and no default is left out, so plain `claude` runs. A `required` arg must have a value; an undeclared arg is refused. Each declared arg must appear in the command. An agent profile can have `resume`, the argv that continues a conversation, for example `["codex", "--no-daemon", "resume", "${conversation_id}"]` or `["claude", "--resume", "${conversation_id}"]`. `${conversation_id}` is required; the hub fills it from the identity, never from an arg. Other tokens are declared args, filled from the spawn's effective args; the same token rules apply. The resume command gets the same environment as a spawn (see "Agent resume"). An unknown field or an invalid profile makes the whole file invalid, so a typo never removes a profile silently. The hub reads the file at each request and tells the plugins when it changes. Profiles are global now. Later, spawn point files come after the global file, and a profile in a later file replaces the profile with the same `id` (`loadFrom` already applies this order).

**Built-in shell.** The profile `shell` (kind `accessory`) runs the login shell (`$SHELL -l`, or `/bin/zsh` when `$SHELL` is not absolute). It is first in `hub.profiles`. A profile file can replace it with its own `shell` profile. `hub.profiles` entries also carry `cwd` (empty: the home directory).

**Environment.** Every pane, also an accessory, starts through `/usr/bin/env -u LAZYREX_AGENT -u <agent variables>` (`agentEnvUnset`): the user can start an agent in a shell, and it must not inherit another agent's identity. Only an agent spawn sets `LAZYREX_AGENT` again. Tests: `TestSpawnArgvEnv` (the pane command with stale variables) and `tests/isolation` (a worker pane of a lab server that has stale variables, Rex `0.0.1792158932-g88fd41`).

**Agent profiles.** An agent profile also creates a hub agent with a new ID `<profile>-<n>` and name `<Name> <n>`. IDs are never reused. The pane gets `LAZYREX_AGENT=<id>` through `/usr/bin/env`, because Rex has no env option on a spawn. Messaging adds a thread tab for the new agent without a restart.

**Hub tools** (agents call them through the agent face): `hub.profiles`, `hub.spawn {profile, key, title?, args?, placement?}`, `hub.workers`, `hub.stop {spawn_id, key?}`. The home plugin acts for the user with `for: "user"`; only a registered plugin may do that.

`hub.workers` gives each worker that is not closed: `spawn_id`, `session_id`, `cwd`, `profile`, `title`, `parent`, `agent_id` (an agent spawn), `state` (`starting`, `running`, `exited`, `failed`), `error` and the program `status`. An open spawn with an `agent_id` also has `identity`: `current` when its pane has a current conversation identity (identity.go: an accepted hook report; cleared when the pane process ends and when the conversation resumes in another pane), else `none`. The spawn row's `agent_id` decides, not the current profile, which can change or go. Every other worker has no `identity` field. Each worker's identity is read after its own recovery; two workers are not one snapshot. A failed read of the identity fails the call; it is never `none`. Test: `TestWorkerIdentity`.

A plugin that handles a forwarded agent call (`hub.forward`) can act for that agent: it passes `on_behalf` = the forward's `requester_pid` to `hub.spawn`, `hub.spawn_window` or `hub.stop`. The hub accepts it only from a registered plugin, and only while a `hub.forward` to that plugin client with that requester pid is in flight. The hub then finds the agent from that pid with the same rule as an agent's own call, so the worker's parent is the agent and the agent can stop it. `on_behalf` with `for` is refused. This covers synchronous work only: a call after the forward returns is refused (the coordinator, 2026-10-19). Tests: `hub/cmd/hub/onbehalf_test.go` (refusals) and `tests/harness/tests/onbehalf.lua` (the real path).

Deferred work (for example a template start that opens its tabs after the agent's call returned, or after a restart) uses a durable grant (`hub/cmd/hub/grants.go`; the coordinator and Codex, 2026-10-19):

- `hub.grant.create {on_behalf, key, entries: [{action, key, request}]}`: only while the agent's forward is in flight (the `on_behalf` rule). The hub stores the plugin name, the agent ID (never the pid), and the exact requests. The only action is `hub.spawn_window`. A request has only `session` (an ID, or `{ensure_key}`: the session that `hub.session.ensure` made for this plugin and key), `title` and `layout`; keys are unique. A repeat with the same agent (any ID of it) and entries returns the same grant, also a revoked one; anything else is `conflict`.
- `hub.grant.get {key}` and `hub.grant.revoke {grant_id}`: the owning plugin only (by name, so they survive a restart). Revoke is idempotent and does not cancel a Rex call already sent.
- `hub.spawn_window {grant_id, key, session?, title?, layout}`: the plugin that owns the active grant, outside any forward. The call must match one entry exactly (action, key and every request field). A session reference matches only a done ensure row of that plugin; the hub never creates or chooses a session for it. `grant_id` with `for`, `on_behalf` or `placement` is refused. The spawns belong to the grant's agent. The hub checks the grant again inside the admission transaction: a revoke that commits before the admission refuses the call; an admission that commits first may finish. An entry list has no count limit; the 64 KiB control message that carries the create is the bound.

Tests: `grants_test.go` (validation, matching, ownership, revoke) and `tests/harness/tests/grants.lua` (real path: create during a forward, deferred spawn, refusals, revoke, session reference, migration).

**Placement.** Omit `placement` to open a new tab in the work session. Set `placement.session` to another session ID to use that session. The hub attaches the session before admission.

- New tab: `placement: {session: "session:..."}`.
- Split: `placement: {session: "session:...", beside: "block:...", direction: "right", ratio: 0.3}`. The anchor must be placed in that session. Direction is `right`, `left`, `above`, or `below`; the default is `right`. Ratio is the fraction for the new pane, greater than 0 and less than 1; the default is `0.5`. Direction and ratio require `beside`.
- Detached: `placement: {session: "session:...", detached: true}`. The worker has no tab. It still has a block ID and can be stopped. `detached: true` cannot be combined with `beside`.

Spawn results and worker listings include `session_id`. A retry must preserve the placement fields. The hub never resolves a retry against a replacement work session. Stop closes only the worker's block, including a split pane or a detached block.

Verification (Codex, Rex 1203): `workers_placement_test.go` exercises four split directions and their ratios, default tab and split placement, another session, detached placement, stop, and retry identity. It also checks that invalid targets admit no spawn intent. The tests call worker entry points with a real isolated Rex connection. Full cross-session agent handshake and restart verification belongs to the agent-session integration.

**Lifecycle.** The spawn row is the source of truth:

- `hub.spawn` commits the spawn row (status `prepared`, with the hub generation) and the new agent. Then it commits the row as `sending`, and only then calls Rex to create the worker. A row that is still `prepared` was never sent: when the sending commit fails, the spawn fails at once with an error that starts with `not_created`; if the hub cannot store that failure either, the call returns an error that starts with `not_created`, the row stays `prepared` (a replay reads `pending`), and the hub stores the failure when the database takes writes again; after a hub restart, the hub fails each `prepared` row of an older generation the same way. Nothing of a `not_created` spawn exists in Rex, so a new key can retry it. A `sending` row only proves that the request was allowed: its outcome stays unknown until resolved (below), because a request that Rex received completes after its client is gone (rex-protocol.md, "A request outlives its client"). `hub.spawn_window` commits the sending state of all its leaves in one transaction. Callers see `prepared` and `sending` as `pending`. A retry with the same key returns the same spawn. A retry with different content (the exact `{profile, title, args, placement}` request) is refused.
- Rex answers: `open` (the spawn row and the agent binding commit in one transaction) or `failed` (Rex refused).
- No answer (hub crash or disconnect), or a success answer without exactly one nonempty block ID: the spawn stays `sending`. Rex said yes, so a pane may exist (`TestSpawnMalformedSuccessLab`). A label is only a hint, so the hub never adopts or closes a pane by label. An agent spawn opens when its agent joins from the one pane with the spawn's label. An accessory spawn stays pending in the worker list, with "outcome unknown", and `hub.stop` refuses it.
- Join admission for a spawned agent: `prepared` gives `not_ready`, `sending` gives `not_ready` (or adoption, above), `open` accepts only the recorded block, `failed` and `closed` refuse (the ID is retired).
- `hub.stop` is idempotent by `spawn_id`. The user and every agent may stop any worker, also one that another identity started (decision "Agents may act on anything", 2026-10-19). Stop on a failed spawn dismisses it from the list.
- The hub tells the plugins (`hub.changed`) when workers, agents, profiles or plugins change, including a worker exit and a program status change. The payload is a hint: a plugin reads the current state. So the hub keeps at most one `hub.changed` in flight for each registration and kind; a change during a delivery makes one more delivery after it, however many changes came. A full-queue refusal (`client_unavailable`, "is busy") is retried with a backoff of 0.1 to 1.6 s, at most 5 times; a timeout is not retried, because the plugin may still perform it. A registration that ended gets no more deliveries (changed.go).

No cap on workers is configured, and nothing starts or restarts a worker automatically.

**Worker session recovery (Codex, 2026-10-16).** The hub records the session before it sends a spawn request. That session owns the response, recovery, status, navigation, and stop. A changed work session does not change an existing worker's session. Recovery closes an open row when its session or exact pane is gone. Recovery fails a pending row only when its recorded session is gone. A retry returns that terminal outcome and starts nothing.

A worker in a previous live session stays recorded and can be stopped. The agent-session integration routes join, readiness, and notices through the recorded session. A legacy row has no recorded session. Recovery searches sessions for its exact recorded block. A legacy row without a located block stays unresolved. A pending agent can establish its session through the existing join proof; a label alone cannot establish ownership.

Verification: `workers_session_test.go` calls the worker entry points against an isolated Rex server. Six lab cases passed on Rex 1203 (`0.0.1792158932-g88fd41`): session loss with same-key retry, stop in a previous session, legacy block discovery, legacy late-join adoption, a pending worker in a previous session, and pending loss versus unknown legacy state. The previous-session cases detach the test connection first. The schema test preserves an old row through two migrations. The hub test package passed with these lab cases enabled. The late-join case tests adoption after caller proof; it does not run a new agent's complete handshake. No live workspace changed during this verification.

Run the lab cases with `WORKER_LAB_SOCKET=/tmp/rexlab-worker-<name>/rex.sock go test ./cmd/hub -run TestWorkerSessionLab -v` from the hub directory. Start that isolated server with `spikes/lab/server.sh` first. Without this variable, the lab cases skip. Integration with the plugin recovery changes remains the integration owner's next step.


### Lab results (Claude, real hub path, Rex 1203, 2026-10-16)

Observed:
- Home launch: one tab and one process. The launch result opens the tab.
- Mouse clicks open a plugin, an agent's thread and a worker. Each click makes the target window active.
- States `running` and `exited`. A missing `cwd` does not fail in Rex: Rex accepts the cwd, and the process exits.
- Agent profile: `-agent claude` in the command plus `LAZYREX_AGENT=probe-1`: the process joined as `probe-1` from its own pane.
- Refused: a join for `probe-1` from another pane; a join after its spawn closed; a changed request with a used key; an agent stopping a worker that the user started.
- Same key retry returns the same spawn. Stop retry returns `closed`.
- Named args: a `task` with quotes, `;` and `$(...)` arrived as one unchanged argument (`$#` = 1), and nothing ran. Without the arg, the argument was left out (`$#` = 0). An undeclared arg was refused.
- Hub restart with a `sending` agent spawn: the worker list shows "outcome unknown: no pane found". A pane with the spawn's label appeared later, and its agent joined. The spawn opened with that block.

Defects found by integration and fixed: the home returned a bare tree instead of `{tree}`; a `changed` call inside `hub.register` deadlocked on `h.mu`; worker exits did not refresh the home.

Untested: the `failed` state on a real hub (no harmless way to make Rex refuse `new_window`; the controlled-hub test covers the view); app selection (no Rex app is attached to the lab server); a real Claude or Codex profile.

## The work session

Status: lab (Claude, Rex 1203, 2026-10-16).

The hub owns its work session: the LazyRex session (`ui_sessions` key `*`). Agents bind to panes there, and plugin processes and workers start there. The config has no `session` field; the hub refuses a config that has one. At start, the hub uses the recorded LazyRex session, or adopts one session labeled "LazyRex", or creates one.

Reason: the config used to name a session ID. On 2026-10-16 the maintainer closed that session (LazyRexWork). Its panes, the plugin processes and the hub itself closed with it, and the stale ID stopped every restart.

Each plugin row records the session of its instance (`plugins.session_id`, and `spawn_intents.session_id`). Rules (Codex review of d35ab4b):

- A recorded block that is missing from its live session is a user stop, as before. Closing one plugin pane while the hub runs is a user stop too.
- A block whose session is gone is not a stop. The hub starts the plugin again in the work session. This applies at start and while the hub runs: on `block_closed` the hub checks the plugin's session first. If the work session closed, the UI pass creates a new one and the plugin starts there.
- If the hub cannot check the session (an error, not an answer), it saves no stop. The saved identity stays, so a hub restart classifies the close again. The hub also checks again in the background for about a minute. If it still cannot tell, it emits `plugin_close_unclassified` and starts nothing; use `hub.admin.start` or `hub.admin.stop`.
- The hub attaches another session before it reads or closes blocks there: Rex refuses those calls on a session the connection has not attached. Observed in the lab: a sending intent named an instance alive in an old, unattached session; the hub found it, stopped it and started a new instance in the work session.
- A row from before the session was recorded: the hub searches every session for the block and retires it there. If no session has it, the hub starts the plugin again (migration decision: desired=running means the hub never recorded a stop).
- A spawn intent for a session that is gone is abandoned. An intent for a live session is searched for in that session, and stays unresolved until found. A legacy intent is searched for in every session. An unresolved intent stops every start of the plugin, also `hub.admin.start` (code `unresolved_intent`). There is no override (plugin-sources.md, section 7).
- A plugin whose dependency is not registered waits (state `waiting`, with the names). An accepted `hub.register`, a `hub.admin.start` or `hub.admin.stop` of any plugin, and a refusal start each waiting plugin again. `hub.admin.start` of such a plugin answers `{"desired": "running", "state": "waiting", "waiting": [names]}`. A dependency that stops, crashes or is refused later does not stop its dependents (plugin-sources.md, section 7).
- `hub.plugins.reload` reads the managed file again (plugin-sources.md, section 13). An operator or the registered current instance of an entry-backed plugin may call it. A removed entry's instance runs on as a kept record until `hub.admin.stop`; a re-add with the same name and source keeps a registered instance that passes the start check. `hub.admin.start` of a retained instance runs the start check again.
- The store fence: a `hub.store.batch` of a plugin instance checks its caller again under a per-plugin lock, and each path that clears the instance's client ID (a retirement, a stop, an exit, a session loss) takes that lock after the clear. A batch that passed the first caller check before the clear then fails with `stale_instance` and writes nothing; a batch that already holds the lock commits before the retirement continues. After a replace returns, no store write of the old instance commits. Unit tests: `TestStoreWriteFence`, `TestStoreWriteFenceWaits`.
- `hub.admin.replace {plugin, expect_instance}` replaces the running instance with a new instance from the current candidate (plugin-sources.md, section 14). The admin guard applies. After a refusal before the retirement, the replace has retired nothing and spawned nothing; the reconciliation of the spawn intents can still change the owned instance. `spawn_unknown` keeps the intent open. A `spawn_failed` whose intent the hub could not abandon reports that intent as open. `hub.plugins` entries report `instance_id`, `instance_pending`, `desired` and `registered`.
- The crash backoff (lead-hf-queue item 2; `respawn.go`). An instance that exits (`child_exited`) while its entry is desired running respawns after a delay: 1 s, then twice the last delay, at most 60 s. An instance that ran 60 s or more from its spawn starts the delays again at 1 s. `hub.plugins` state `crashing`: desired running; the recorded instance has exited; a respawn is due at `respawn_at`. It is not `stopped` and not `running`, and it is not registered. The record keeps the exited `instance_id` until the respawn retires it. While crashing, the entry also reports `respawn_at` (RFC 3339) and `respawn_delay_ms`; in every other state these keys are absent. From the first exit in this hub process, the entry reports `last_exit`: `{at, detail, ran_ms}`. `detail` is the hub's text of the exit (the `child_exited` error, else the exit code): one line, at most 200 bytes. `ran_ms` is absent when the hub did not record the spawn. A stop (`hub.admin.stop`, a user close of the block) cancels the respawn and resets the delay. A start that passes its refusals cancels it and keeps the delay; a refused start leaves it. A respawn whose start enters `waiting`, an unresolved intent or a refusal leaves the next start to that state; only a start error with no such owner (for example no work session, or a Rex refusal of the block) schedules the next respawn. The backoff is in memory: a hub restart starts it again. A respawn is not a plugin op attempt: it writes no `plugin_op_attempts` row. Events: `plugin_respawn_scheduled` `{plugin, instance_id, delay_ms}` (with `reason` after a failed respawn), `plugin_respawn` `{plugin, delay_ms}`.
- `hub.admin.replace` and `hub.admin.start` take an optional `expect_run {source, commit, digest}` (commit and digest only for a GitHub source). Inside admission, before any retirement or spawn, the candidate must be that exact run, else code `candidate_changed` and nothing changes. A start with `expect_run` sets the desired state only when it starts, and never waits: a dependency that is not registered is `dependency_not_ready` (plugin-sources.md, section 14).
- `hub.plugins` entries report `commands`: the plugin's palette commands (`id`, `title`, `route`, and `surface`, `landing`, `action`, `user_only` when set), `[]` for none. The home plugin reads them for its Launch Profile… gate (see "No LazyRex tab"). The hub stores the latest admitted command set of the current instance: saves run one at a time, and each reads the admitted set when it runs, so the last save to finish stores the latest set admitted before it read. Every admission starts a save, so the final stored set is the latest admitted set. A stopped instance, or one that has not registered, saves nothing. `hub.changed` (`plugins`) follows each successful save. A failed save logs, emits `commands_save_failed` (`plugin`, `error`) and keeps one retry pending per plugin (1 s, doubling to 30 s), which saves the latest set. The retry ends at the hub stop, or when the registration is no longer current. Tests: `commandsave_test.go`.
- `hub.plugins` entries and kept records report `tag` for a run of an `@latest` source: the release tag `vX.Y.Z` that the lock selected for that run. Every other run has no `tag` key (never null, never ""). The `candidate` object has `tag` when the candidate tag differs from the run tag: the new tag, or null when the run has a tag and the candidate has none. A run saved before the field existed has no tag (plugin-sources.md, section 7). The version fields (the hub lead, 2026-10-22), for the lock, the pin document, `hub.plugins` and the plugin manager:
  - `ref` is what the entry asks for: `latest`, a tag name, a branch or a commit.
  - `ref_kind` is the kind in the lock: `branch`, `tag`, `commit` or `latest`.
  - `tag` is what `latest` resolved to, only for `ref_kind` `latest`.
  - `commit` is the exact pin, for every kind.
  - A UI that shows "the version" shows `tag` when present, else `ref` for kind `tag`, else the short commit.
- The admin guard (`hub.admin.*`, and `hub.plugins.reload` from a caller that is not a registered instance) refuses a caller in an agent or plugin pane. It walks the caller's process ancestry from the Rex-attested pid. Three outcomes: a match (refused), a complete walk to pid 1 without a match (allowed: an operator), and an incomplete walk (refused): the caller has no pid (a missing pid, or a TCP caller: TCP has no pid), a process information read failed, a process in the chain ended, a parent is 0 before pid 1, or the walk passed 64 processes. Only a walk that reaches pid 1 is complete. Text: `cannot check the caller's process ancestry: <error>; admin actions need a verified caller`. A match found before the walk fails still counts. The guard checks the call, not the keys in a view: an action that a view offers runs from its renderer pane (residual risk: "Restart form"). Checks that allow only on a match (registration, pair and view ownership, `hub.identify`) treat an incomplete walk as no match, so they refuse too.
- `hub.admin.sql_return {plugin}` takes a stopped plugin back from hub.sql to its store version: it moves the plugin's SQL file aside and gives the notice path back to `hub.store.batch` (sql.md, section 11). The admin guard applies.
- `hub.admin.shutdown` stops the hub, as SIGTERM does (see "Stop and exit status"). It takes no arguments: any field is refused (`hub.admin.shutdown takes no arguments`). The admin guard applies. The hub replies `{"stopping": true}` first, then starts the stop. A reply that cannot be delivered stops nothing: the caller sees an error or a timeout, and the hub still runs, so a retry is correct. Plugins stay in their blocks.
- `hub.info` returns `{"state", "pid", "generation"}`: the canonical state directory (absolute, symlinks resolved), the hub process id, and the hub generation. Any client may call it; it is a read. It takes no arguments (`hub.info takes no arguments`). The commands `lazyrex-hub stop` and `status` use it to find the hub of a config (see "Stop and status").
- Process information reads: macOS uses sysctl `kern.proc.pid` (an empty answer means no such process). Linux reads `/proc/<pid>/stat` (the parent pid, and starttime after the last `)`, with the boot time from `/proc/stat` btime) and `/proc/<pid>/cmdline`. Verified on Linux (reviewer, 2026-10-21, Rex 0.0.1792593189-g3f4a0a, Go 1.27.2 linux/amd64, CGO_ENABLED=0): the process and guard unit tests and the plugin lifecycle labs (`TestManaged*Lab`, `TestPlugin*Lab`). The full harness (`TestHarness`: the self-tests, the failure fixtures, the no-match selection, the interrupt cleanup and the isolation checks) passes on Linux at c6f5c15 (reviewer, 2026-10-21, same Rex and Go). Both give the start time as `<seconds>.<microseconds>`. On Linux the resolution is 10 ms (USER_HZ 100): two processes that start in the same tick have equal start times. Equal is not "before" (`startBefore`), so for two hook reports with an equal start time, arrival order decides; the stale-report order is weaker on Linux. Pid reuse: a pid and a start time identify one process lifetime. A false match needs the kernel to give the same pid again within one tick, after the pid space wraps (pid_max: 32768 by default; systemd raises it to 4194304). The hub does not refuse on that case: /proc gives no signal to detect it.

Observed in the lab (Rex 1203): plugins with blocks in a gone session restarted in a new LazyRex session; destroying the work session while the hub ran restarted all three plugins in a new session within about 0.5 s; closing one plugin pane stopped it; a legacy row found its live block and replaced it; a normal restart replaced each instance in place.

Agents may run in any session (the maintainer, 2026-10-16). `hub.join` accepts a caller whose process descends from a pane's child process. It searches the work session first, then every other session, and attaches each one first. The binding records the session (`bindings.session_id`); notices, readiness and `hub.admin.rebind` use it. A dynamic agent must still join from its spawn's block. Observed in the lab (Rex 1203): codex and claude joined from a session other than the work session; a message from claude wrote the notice into codex's pane, also while that pane was detached. `hub.admin.rebind` now reads the pane's current status, as join does; before, a rebound agent stayed "not ready" until its program reported a new status. Every status read attaches the pane's session first (rex-protocol.md, "Program status needs an attached session"). A read that fails, or a pane without the configured record, gives the unknown state. Unknown is never ready: a notice ends with CR, and a blocked prompt could take it as an answer. When a notice waits for an agent in the unknown state, the notice loop reads the status again, outside the hub lock, and applies the result only if no status event, rebind or pane change came during the read. The notice text is the one of the plugin's current run (plugin-sources.md, section 7). After the read, the loop checks the agent and the plugin again: a plugin with no run (for example after a stop during the read) gets no notice, and the notice waits for its next run. A read that gives no state is retried with a backoff of 5 s to 1 min for that agent (tests: `tests/harness/tests/notice_ready.lua`). Start Codex with `--no-daemon` (for example `codex --no-daemon resume <id>`). Without it, Codex 0.163.0 starts MCP servers under its `codex app-server` daemon, not under the pane, and the hub refuses the join ("caller pid … is not in a pane of the hub's session"). Observed 2026-10-16 on the real server.

The config field `rex` is optional. Without it, the hub runs `rex` from `PATH`, which follows the installed Rex build.

## Setup

`lazyrex-hub setup [-dir DIR] [-server ADDR] [-print]` makes the first configuration of a user's hub (`hub/cmd/hub/setup.go`, `setupfile.go`, `setupjson.go`). The default DIR is `~/.lazyrex`. `-server` names the Rex server for `hub.json`, for a user with more than one server. Without it, setup uses the server that Rex reports as its default.

Prerequisites. Setup stops before it proposes a file if one of these is missing:

- The renderer `lazyrex-view` beside the running `lazyrex-hub`. A release user installs the `lazyrex-view` of the same release. From a source checkout, `go build` makes it.
- A running Rex server. Setup asks Rex for the server's own address (`rex [-S ADDR] --autostart=false server status --json`, field `endpoint`, without `REX_SERVER`). It never starts a server. On macOS, Rex's default server is the socket `$HOME/Library/Application Support/rex/server.sock` (observed with `rex -vv`, Rex 0.0.1792158932). A HOME of its own therefore has no default server.
- An installed binary. A `go run` build is refused, because the hooks run the binary that setup names.

The files:

| File | What setup does |
| --- | --- |
| `DIR/hub.json` | Creates it (mode 0600). Never changes an existing one. |
| `DIR/profiles.json` | Creates it with a Claude and a Codex agent profile, each one only if the CLI is on `PATH`. Never changes an existing one. |
| `lazyrex_boot.lua` beside the Rex config file | Writes the boot palette entries (setup owns this file and replaces it). |
| The Rex config file | Appends `pcall(require, "lazyrex_boot")` and `pcall(require, "lazyrex_palette")`, each one only if it is missing. |
| Claude Code: `$CLAUDE_CONFIG_DIR/settings.json` (default `~/.claude`) | Adds the `SessionStart` and `UserPromptSubmit` hooks of `hooks/README.md`. |
| Codex: `$CODEX_HOME/hooks.json` (default `~/.codex`) | Adds the `SessionStart` hook of `hooks/README.md`. |
| `<Claude config dir>/skills/lazyrex/SKILL.md`, `~/.agents/skills/lazyrex/SKILL.md` | Installs the LazyRex skill. Codex reads user skills from `~/.agents/skills` (`$CODEX_HOME/skills` is its deprecated location). |

The new `hub.json` names:

- the default Rex server;
- the state `DIR/state`;
- the renderer beside the binary;
- the agent `user`;
- the profiles file;
- `lazyrex_palette.lua` beside the Rex config file (`REX_CONFIG`, else `$XDG_CONFIG_HOME/rex/init.lua`, else `~/.config/rex/init.lua`);
- `transcript: true`.

Its plugins are the home plugin (`github:lazy-rex/LazyRex/plugins/lazyrex`) and Messaging, Profiles, Templates, Tunnels and Worktrees. Each one uses its repository's highest `vX.Y.Z` tag, which setup reads with `git ls-remote` (the coordinator, 2026-10-22). A repository without such a tag is left out and reported. Setup never falls back to `main`. When the hub supports `@latest`, setup writes `@latest`. If setup cannot read the tags, `hub.json` is blocked.

An agent is skipped when its CLI is not on `PATH` and its configuration directory does not exist. The hook command names the running binary. The path is in double quotes, as in `hooks/README.md`. A path with `"`, `$`, `` ` ``, `\`, `!` or a newline gets POSIX single quotes. Setup adds no hook to an event that already has the same command, the Python hook (`session-start.py`), or another `lazyrex-hub … hook <kind>`. It reports that event instead. The switch from the Python hook is a separate step.

Consent. Setup first lists each file with its state: `new`, `change`, `unchanged`, `not written`, `blocked: <reason>`, or `skipped` for an agent. Then, for each file that it would write:

1. Setup shows the complete diff: every line, with the prefix `  `, `- ` or `+ `.
2. It asks `Write <file>? [y/N]`. Only `y` or `yes` writes. End of input is no, for this file and for each later file.
3. It reads the file again. It refuses the write if the content, the existence, the permissions, or the resolved path (every symlink, also in parent directories) changed since the diff. This recheck is not an atomic compare-and-replace.
4. Before it changes an existing file, it copies the file to a new `<file>.lazyrex-bak-<unix time>` with the same permissions. A second backup in the same second gets a suffix.

A symlink is followed: setup writes the file that it resolves to, and the link stays a link. A symlink that does not resolve to a regular file is blocked. A hook file that is not valid JSON, or whose root, `hooks` or `hooks.<event>` has another JSON type, is blocked and not changed. The hook merge keeps the order of the user's keys and all other hooks. It writes with 2-space indentation; the backup keeps the original text.

`-print` shows the list and every diff and writes nothing. Without a terminal on standard input, setup does the same and exits 2. Neither one creates a directory. At the end, setup reports each file as written (with its backup), declined, blocked, or failed. The other files are still proposed and written after a blocked or failed one.

Exit status:

| Case | Exit status |
| --- | --- |
| Every file was written, unchanged, or declined | 0 |
| A file is blocked, or a write failed (also with `-print`, for a blocked file) | 1 |
| The plugin sync failed | 1 |
| `rex skill setup` failed, or `rex skill status` does not confirm it afterwards | 1 |
| A prerequisite is missing | 1 |
| Standard input is not a terminal (without `-print`) | 2 |

A decline is the user's choice, not an error.

Rex's own skill and hooks (the user, 2026-10-22). Rex installs them with `rex skill setup`; setup offers that command last, for exactly the agents that it found (`--agent claude`, `--agent codex`). It never runs a bare `rex skill setup`, which reaches every agent that Rex finds, and it never adds `--force`: Rex refuses to replace a skill named rex that it did not install.

- No agent found: no question and no command.
- `rex skill status --json` says that the skill and the hook of each agent are `current`: setup says `Rex's skill and hooks: installed for <agents>.` and asks nothing. A failed, malformed or incomplete status is not an installed claim.
- Otherwise it asks `Install Rex's own skill and hooks for <agents> now (rex skill setup --agent ...)? [y/N]`. On `y` it runs the command and shows its output, then asks `rex skill status` again. Only a status that says `current` for each agent prints the installed line; a failed setup or an unconfirmed status is an error (exit 1).
- On `N` and at the end of input, setup runs no `rex skill setup`; the read-only `rex skill status` call before the question can still run. With `-print` and without a terminal, setup runs neither command. In each of these cases, setup prints the exact command.

Plugins. The hub refuses a plugin of hub.json that is not installed ("is not installed: run lazyrex-hub plugin sync"), so setup offers the plugin sync (the coordinator, 2026-10-22). The sync comes after the files and before the Rex skill offer, and only for a hub.json that exists and names a server (the one that the hub controls read):

- If `plugin status` finds every plugin installed, setup says `Plugins: installed.` and asks nothing.
- Otherwise it asks `Install the plugins of <hub.json> now (lazyrex-hub plugin sync)? [y/N]`. On `y` it runs `plugin sync` (git, with network) and shows its lines, then `Plugins: installed.` A failed sync prints `Plugin sync failed: <error>`, and setup exits 1.
- On no, no answer, or a failed sync, setup prints the exact command as a next step: `Install the plugins: <binary> plugin sync -config <hub.json>`, each path quoted for a shell.

No sync, no question and no command when hub.json was declined, is blocked, failed to write, or names no server, and never with `-print` or without a terminal. Setup reads hub.json again before the sync: the file that it kept or wrote must still name a server (another writer can change it during setup). Tests: `TestSetupOffersThePluginSync` (yes, yes with another `-dir`, no, no answer, a failed sync), `TestSetupSkipsTheSyncWhenInstalled`, `TestSetupNoSyncWithoutAUsableConfig`, `TestSetupNoSyncAfterTheConfigChanged` (a write refused because another writer made hub.json after the diff; a kept hub.json that lost its server).

The boot file has the palette entries that work without a running hub. **LazyRex: Set Up** opens a new window in the caller's session, with a pane that runs the same setup command: the same binary, `-dir`, and `-server` if it was given. The pane stays open after setup ends, so the user can read the report. After setup writes the boot file or the Rex config, it prints `rex -S <server> config reload` as the next step, with the server that it configured (a bare `rex config reload` would reach the caller's server). The Rex config loads both files with `pcall`, so a missing file is not an error.

**Hub controls.** The boot file also has **LazyRex: Start Hub**, **LazyRex: Stop Hub** and **LazyRex: Hub Status** (`setupBootLua`). Each one opens a new window in the caller's session with one pane that runs an argv (no shell; `login = false`), and the pane stays open after the command ends:

- Start Hub: `<binary> -config <dir>/hub.json -restart`, pane label `LazyRex hub`. The pane owns the restart wrapper: the wrapper's lines appear there (see "Restart wrapper"). Closing the pane, or Ctrl-C in it, stops the wrapper and the hub, and the restart backoff does not bring the hub back. A second Start while a wrapper runs is refused (`another restart wrapper owns <state>`, exit 1). A Start beside a hub without the wrapper (as dev.sh runs one) is refused by that hub's state lock: the pane shows the hub's last lines (`another hub owns <state>`), and the running hub is not touched.
- Stop Hub: `<binary> stop -config <dir>/hub.json`, label `LazyRex stop`. It is `lazyrex-hub stop` ("Stop and status"): `stopped` (exit 0), or `not running` (exit 0) when no hub runs. The pane is not an agent or plugin pane, so the admin guard allows the shutdown. The 10 s bound starts when the shutdown is sent, after the first look.
- Hub Status: `<binary> status -config <dir>/hub.json`, label `LazyRex status`, with the statuses of `lazyrex-hub status`.

Each control gets `-real-server` when the server of that hub.json is the real Rex server (`isRealServer`, the check that the hub makes; the flag must match exactly). Setup reads the server from the hub.json that the controls read: the one that it proposes, or the existing file, which it never changes and whose server can differ from the one that setup found. If setup cannot read a server from it (not JSON, no `server`, or a blocked hub.json), the boot file has only Set Up, and the report says why (`Start Hub, Stop Hub and Hub Status are left out: ...`). After the user's default Rex server changes, run setup again. Rex shows the command's output in the kept pane but not its exit status (lab, Rex 1203: the screen has no exit line; the pane's `process` method reports `last_exit`). So the first words of the output say the result: `stopped`, `not running`, `running`, or the wrapper's lines. That the Rex app shows these entries is inferred: the lab has no app.

Tests: `TestSetupBootHubControls` (the argv of each entry, quoting, `-real-server` only for the real server, no controls without a server) and `TestSetupControlsFollowTheConfigFile` (a new hub.json, an existing one with another server, one that is not JSON, one without a server). Lab: `HUBCTL_LAB=1 go test ./cmd/hub -run TestHubControlsLab`. A lab server loads the boot file, and the test runs each entry as the palette does (`rex -s <session> do <action>`), then reads the pane's screen and its exit code: Hub Status before a start (`not running`, exit 1); Start Hub (the hub answers `hub.info`); Hub Status (`running`, exit 0); a second Start (refused, exit 1); Stop Hub (`stopped`, exit 0; no connected hub; the Start pane's command ended with exit 0; no hub 3 s later); Stop Hub again (`not running`, exit 0); a hub started without the wrapper, then Start Hub (exit 1, the pane shows `another hub owns`; the first hub still answers `hub.info`).

The skill in the binary is a copy of `skills/lazyrex/SKILL.md` at `hub/cmd/hub/setupfiles/SKILL.md`, because `go:embed` cannot reach outside the hub module. `TestSetupSkillCopy` fails when the two differ.

Tests: `go test ./cmd/hub -run 'TestSetup|TestHighestTag|TestHookFile|TestFileDiff|TestAppendLines|TestShellWord'`. They run in a temporary HOME with a fake Rex endpoint and fake tags. No test writes a real user file. The `-print` and no-terminal test seeds existing config files and compares every path, mode and content before and after.

Lab test: `SETUP_LAB=1 go test ./cmd/hub -run TestSetupLab`. The test proceeds as follows:

1. It builds the binary. Then it clears the whole environment of the test process (as `env -i` does) and sets only: HOME, PATH (a fake `claude`, and a fake `git` that prints tags), `REX_CONFIG`, `CLAUDE_CONFIG_DIR` and `CODEX_HOME` under the lab HOME (no Codex directory: the skip case), and the XDG directories under the lab. The cleanup restores the environment. The lab server, every lab pane (also the one that the boot entry opens) and every test command start from that list.
2. It checks that setup without `-server` finds no default server in the lab HOME, and writes nothing.
3. A lab pane runs `setup -server <lab socket>`. Standard input is the pane's terminal; the output also goes to a file through `tee`.
4. Before the first answer, the test checks the endpoint and the full path and state of every proposed file in the output. Then it answers each prompt with `rex block send` (five yes, one no).
5. It checks the report and the files, reloads the lab server's config, and finds the `lazyrexboot_setup` action.
6. It runs that action. The new pane runs the same binary (checked with the block's `process`) and stops at the prompt for the declined file.

## Stop and exit status

The hub process stops in three ways. Each one has its own exit status:

| Cause | Exit status |
| --- | --- |
| SIGTERM or SIGINT | 0 |
| `hub.admin.shutdown` (an operator) | 0 |
| The Rex connection closed (the hub logs `rex connection closed`) | 1 |
| Under a restart wrapper: the config names another state than the wrapper owns, or a state that does not exist | 3 |

A deliberate stop gives 0, and a lost Rex connection gives 1, so a supervisor can restart only after a failure. Status 3 happens only under the restart wrapper, before any side effect of the start (see "Restart wrapper"). The command `lazyrex-hub status` has its own exit statuses (see "Stop and status"); they are not hub process statuses. A signal while the hub starts is also a deliberate stop: 0. The hub catches SIGTERM and SIGINT before its first question to Rex (the real-server check, which can wait up to 10 s for a server that does not answer), so this holds from the config read on; a signal also ends that check at once. A check that a signal ended is never proof that the server is not the real one. A configuration or start error before the hub runs gives 1 (`log.Fatal`); a missing `-config` gives 2. Plugins stay in their blocks after each stop; the next hub start recovers them.

Before each orderly exit (each cause in the table except the one with status 3, and a start error after the palette loop started), the hub replaces its palette file with the header alone and reloads the Rex config ("Palette" in "Agent resume"). The stop write never changes the exit status. It has two bounds of its own: at most 2 s for the palette loop to end (a write or reload in progress is cancelled), and at most 3 s for the config reload. The two configured waits total 5 s, which leaves time within the 10 s bound of the restart wrapper (SIGTERM, then a kill) and the 10 s bound of `lazyrex-hub stop`. Filesystem and process cleanup time is additional: the hub enforces no overall bound on its stop. A reload that takes longer is a failed reload: logged, and not retried.

Test: `SHUTDOWN_LAB=1 go test ./cmd/hub -run TestHubShutdownExitStatusLab`. The test starts its own lab server (`lab/server.sh` in a new `/tmp/rexlab-shutdown-*` directory) and stops only that server and its own hubs. It checks a refused argument (the hub keeps running), the reply `{"stopping": true}` and exit 0, SIGTERM and exit 0, and exit 1 after the lab server stops. `TestHubSignalDuringStartLab` pauses its lab server (SIGSTOP), so the hub waits on Rex in its start, and checks exit 0 after SIGTERM (before the fix: 1, "terminated signal received"). `TestHubRestartSignalDuringStartLab` checks the same with `-restart`. From 5a90de3 until the handler moved before the real-server check, both failed on every run: the check waited 5 s for the paused server, and SIGTERM after 1 s killed the hub (status -1). Observed on Rex 0.0.1792158932-g88fd41 (macOS, 2026-10-21): pass, 8 runs in a row. The unit test `TestAdminShutdownDispatch` checks the refusals and that only the reply path starts the stop.

## Restart wrapper

`lazyrex-hub -config <cfg> -restart` runs the hub as its child and starts it again after a failure (`hub/cmd/hub/restart.go`). The wrapper passes every other argument to the child unchanged. `-restart` under a wrapper is refused.

**Ownership: one wrapper for each state.**

1. Before it starts a child, the wrapper reads the config with the hub's own `loadConfig`. That function reads the config and the managed file; it opens no store, no Rex connection and no plugin.
2. The wrapper makes the state directory (0700) and takes its canonical form: absolute, symlinks resolved. A state path with a newline is refused (exit 1) before the directory is made, and again after the resolution: the ready line cannot carry it.
3. The wrapper takes an fcntl write lock on `<state>/restart.lock` without waiting, and holds it for its life. If another process holds the lock, the wrapper starts no hub, prints `another restart wrapper owns <state> (pid N)` and exits 1. The pid is printed only when the system names the holder. Another error (for example a permission error) is printed as it is.

**The child.** The wrapper starts its own binary with two environment variables. The hub reads and unsets both at the top of `main`, so no process that the hub starts inherits them (`restartenv.go`):

- `LAZYREX_READY_FD=3`: the write end of a pipe. After `hub_started`, the hub writes `ready <canonical state>` once and closes it. A failed or short write is logged once (`ready fd: ...`); the hub runs on, and the wrapper sees no ready line. The wrapper has no ready time limit: it stays in `starting` while that hub runs, and the hub's later exit counts as an exit before ready (`TestRestartWithoutAReadyLine`). Ready means that the hub has started (after `hub_started`). It does not mean that the plugins are healthy. The hub sets FD_CLOEXEC on the fd, so no child of the hub holds the pipe open.
- After each ready, the wrapper reads the refused plugins of that hub and prints each line as `restart: <line>` (see "Refused plugins" in "Stop and status"). The read finds the hub by its `hub.info` pid (the child's pid) and state, so a hub of an earlier run cannot answer for this run. The read runs beside the wait: an exit or a signal never waits for it. It ends after 10 s or with the run, and the wrapper discards a result that comes after the run. A refusal never stops or restarts the hub. Tests: `TestRestartShowsRefusedPlugins`, `TestRestartRefusalReadIsBoundToTheRun` (a stalled read: the stop is not delayed, and the read's context is cancelled), `TestRestartDiscardsALateRefusalRead`.
- `LAZYREX_EXPECT_STATE`: the wrapper's canonical state. After `loadConfig`, before it makes the state directory or takes `hub.lock`, the hub compares its config state in the canonical form. Another state, or a state that cannot be resolved, prints `... start a new wrapper` and exits 3. No store, no Rex connection, no plugin.

**When the child exits.** The wrapper classifies an exit only after the ready pipe ends (at most 2 s after the exit), so "ready, then an immediate exit" counts as after ready. Then the wrapper closes its read end of the pipe, also when a descendant of the hub still holds the write end, so no reader is left for the next start.

| Child exit | Wrapper |
| --- | --- |
| 0 (SIGTERM, `hub.admin.shutdown`) | exits 0, no restart |
| 3 (another state) | exits 3, no restart |
| Another failure before the first ready of this wrapper | exits with the child's status (1 for a signal death), no restart |
| Another failure after a ready | restarts after 1, 2, 4 ... 60 s; a fifth restart within 10 minutes is refused: `gave up`, exit 1 |
| A ready line for another state (a defect) | stops the child, exits 1 |

A restarted child that fails before its own ready counts as a restart.

**Signals.** SIGTERM, SIGINT or SIGHUP to the wrapper (Ctrl-C in its pane, or closing the pane) stops it and never restarts:

- A child runs: the wrapper sends SIGTERM to that child only, and waits at most 10 s. The child exits: the wrapper exits 0. Otherwise the wrapper kills the child and exits 1 (`the hub did not stop within 10s; killed`).
- During backoff: the wrapper exits 0 at once. No child starts.
- The child runs in its own process group, so a terminal signal (Ctrl-C, a closed pane) reaches only the wrapper. A wrapper killed with SIGKILL leaves its hub running; a new wrapper then fails at the hub lock before ready and exits 1.
- Before the first start: the wrapper exits 0. No child starts.

**Lines and status.** The wrapper writes its own lines, with the time, to its pane (its stderr) and to the log: started (pid, attempt), ready, the exit status or signal of each child, the restart delay, gave up. When the hub fails before its first ready, the wrapper first writes the last lines of that run's output (at most 10, from its last 2 KiB) to the pane as `restart: hub: <line>`: the hub's output goes to the log only, and those lines say why it failed. Each control character in those lines except tab becomes `?` (C0, DEL, C1, and a byte that is not UTF-8), because the pane is the user's terminal. `<state>/restart.json` is the wrapper's status for readers, replaced as a whole (a temp file, then a rename): `{"version":1,"pid","start","phase","attempt","last_exit","next_start","since"}`. The phase is one of `starting`, `running`, `backoff`, `gave_up`, `stopped`. A reader trusts the file only while the wrapper holds `restart.lock` with the same pid and that pid has the same start time.

**Log** (`hub/cmd/hub/restartlog.go`). Under the wrapper, the hub's stdout and stderr are one pipe to the wrapper (so their order is kept), and the wrapper is the only writer of `<state>/hub.log`. The hub's output goes to that file only, not to the pane. A crash trace of the hub is in that file too, best effort (below). dev.sh writes `<state>/dev/hub.log`; that file has no size bound.

- One reader for each run moves the output into a byte queue of 1 MiB, in chunks of at most 32 KiB. A chunk that does not fit is dropped and counted. The writer takes the queue as a whole, so up to about 2 MiB of output is held in memory: the batch that it writes and a full queue behind it. The reader never waits for the disk or the pane, so a slow or stalled disk causes no backpressure on the hub.
- One writer moves chunks to `hub.log`. Before a chunk would pass 16 MiB, it closes the file, renames `.2` to `.3` (the old `.3` is removed), `.1` to `.2` and `hub.log` to `.1`, and opens a new `hub.log`. No rename touches an open file. Each file is at most 16 MiB, so the four files hold at most 64 MiB. A line can split across two files.
- A failed open, rotation or write puts the writer in drop mode: the file is closed, no file grows, chunks are counted as dropped, and the writer tries again after 60 s. When it writes again, it first writes `restart: dropped N bytes of hub output since <time>`. This line is best effort: a disk that stays stalled, or the wrapper's exit, can leave the file without it. The drop line and the wrapper's lines count toward the 16 MiB like any chunk.
- At the end of each run, the wrapper waits at most 2 s for the end of the output and ready pipes, then closes both and waits for their readers, and only then writes the exit line. So the hub's last lines come before the exit line, unless they were dropped. Output that a descendant of the hub writes after that is discarded, and no reader is left for the next run.
- Pane lines (the wrapper's lines, drop notices) go through a channel of 64 lines to one pane goroutine. A pane that nobody reads drops lines; it never slows the log. In drop mode the pane gets one line at entry and one at each retry.
- At its exit, the wrapper gives the writer and the pane at most 2 s each.
- At open, a `hub.log` at 16 MiB or above is rotated first, so each run writes to a file below the size.
- The hub, when it runs under the wrapper (`LAZYREX_READY_FD` set), catches SIGPIPE (`signal.Notify`). A wrapper that is gone then makes a write fail with EPIPE; it does not kill the hub. SIGPIPE is caught, not ignored, so a child of the hub (git, rex) starts with the default action: an ignored signal would stay ignored across exec.

Tests: `TestRestart*` in `restart_test.go` (a fake hub from the test binary: the first failure, a deliberate stop, the backoff sequence and gave up, the 10-minute window, signals while starting, while ready and during backoff, a child that ignores SIGTERM, exit 3, a ready line for another state, a second wrapper that another process owns, also through a symlink; the last output lines of a hub that fails before its first ready, before the wrapper's line; the last lines of the last run only, and a cut tail, no control character in a pane line). `restartlog_test.go`: the size bound with output without a newline and with long lines, a stalled disk (the queue drops, the hub is not blocked, a 10 MiB write of the hub finishes), a failed rotation and a failed open (drop mode, then the retry), a full `hub.log` at open, a pane that nobody reads, the order of the last output and the exit line, five runs whose descendant keeps the output pipe (no reader or descriptor left), and SIGPIPE ignored under the wrapper. The lab test also starts the wrapper the way the palette will: in a labeled pane (`rex run`). `restartenv_test.go`: the variables, the state check, the ready line, `hub.info`, and a real hub binary that exits 3 with no `hub.lock`, `hub.db` or `events.jsonl` in either state.

## Stop and status

`lazyrex-hub status -config <cfg> [-real-server]` and `lazyrex-hub stop -config <cfg> [-real-server]` (`hub/cmd/hub/restartctl.go`). Both read the config and apply the real-server check (`checkServer`), so the real Rex server needs `-real-server`, as for the hub. Then they look at two things:

- The hub of this config: a connected Rex client labeled `lazyrex-hub` whose `hub.info` state equals the config's canonical state. A hub of another state on the same server is reported as another hub, never stopped.
- The restart wrapper of that state: the holder of the lock on `<state>/restart.lock` (fcntl F_GETLK; a reader never creates the file and never takes the lock), and `<state>/restart.json`. The status file counts only when its pid is the holder and that pid has the start time in the file.

`status` prints the phase as the first word, then notes: the Rex server cannot be asked, another hub runs on this server. Its exit statuses are its own, not hub process statuses:

| First word | Meaning | Exit |
| --- | --- | --- |
| `running` | the hub of this config is connected (with the wrapper pid when a wrapper owns the state) | 0 |
| `not running` | no wrapper, and Rex lists every connected hub with its `hub.info` state: none is this state. A `gave_up` status file adds when and the last exit | 1 |
| `backoff`, `starting` | a wrapper owns the state, and no hub of it is seen. When the Rex server cannot be asked, this is no proof that no hub is connected | 3 |
| `unknown` | a wrapper holds the lock, but the status file does not match it; two connected hubs report this state; the state or `restart.lock` cannot be read; or, with no wrapper, the Rex server cannot be asked or a connected hub does not answer `hub.info`. A failed look is never `not running` | 4 |
| (usage or config error) | | 2 |

**Refused plugins.** For `running`, `status` also reads `hub.plugins` from that hub and prints one line for each item of its `refused` list (plugin-sources.md), after the `running` line. The exit status stays 0: a refusal does not stop the hub.

- A refused entry: `plugin <source>: refused: <error>`. A plugin of hub.json that is not installed shows here, with its `lazyrex-hub plugin sync` hint.
- The managed file (once, when it cannot be read): `plugins: managed file <path>: <error>`.
- A failed read, a reply without the `refused` list, or an item without a source: `plugins: unknown: <error>`. A failed read is never "no refusals".
- A `retained` plugin is not refused, and an empty list prints nothing.

**Agent panes without an identity** (the user, 2026-10-22). For `running`, `status` then reads `hub.workers` from that hub and prints one advisory line for each worker with an `agent_id`, state `running` and `identity` `none`: `agent <title> (pane <block>): no current conversation identity yet. Claude Code reports at its start; Codex can report only at its first prompt. If it stays, check the LazyRex hook (lazyrex-hub setup; Codex: trust it with /hooks).` The line comes at once (no grace period: Codex can wait for its first prompt indefinitely), and the exit status stays 0. An exited agent gets no line: its CLI no longer runs. Unknown is never silence: a failed read, a reply without the `workers` list, an unknown `identity` value, or a running or exited agent worker without the field (an older hub) prints `agents: unknown: <why>`. Starting and failed agent workers have no field and print nothing. Text from the hub goes through the same terminal filter as the other lines. Tests: `TestAgentIdentityLines`; lab: `tests/harness/tests/agent_identity.lua` (a spawned agent CLI that does not run its hook shows the line; after the real `lazyrex-hub hook claude` from its pane, the line is gone).

**Remote Control** (the user, 2026-10-22; see "Remote Control (observed)"). For `running`, `status` then reads `hub.app.remote_control` from that hub and prints one line for each connected Rex app, with the app's label and client ID (two apps can have the same label). The lines show the last observed result, never the current setting: the user can change the setting with no app action in between. The exit status stays 0.

- `Remote Control (app <label> <client>): last observed on at <time> (<action>)`, where `<time>` is RFC 3339 in local time, with its date and zone (an observation can be days old)
- `Remote Control (app <label> <client>): last observed off at <time> (<action> refused). Turn it on in the Rex app: Settings, Rex Server, Remote Control. LazyRex needs it for Go to Session and Insert Path.`
- `Remote Control (app <label> <client>): unknown: no retained app action observation for this app`
- No connected app: `Remote Control: no Rex app is connected`.
- Unknown is never silence: a failed read, a reply without the `apps` list, or an unknown `state` prints `Remote Control: unknown: <why>`.

Tests: `TestRemoteControlLines`.

The text comes from the hub, so a control character becomes `?` (`paneSafe`). Tests: `TestPluginRefusalLines`, `TestStatusShowsRefusedPlugins`.

`stop` sends no signal:

- `running`: `hub.admin.shutdown` to that hub (the admin guard applies, so an agent or plugin pane is refused), then it waits until a successful `rex client ls` no longer lists that client and, when a process held `restart.lock` (with a matching status file or not), until the lock is free and can be asked. The shutdown call and the exit observation share one 10 s bound, which starts when the shutdown is sent. The first look (status) is before it, and a killed rex call can add up to 500 ms for its pipes. Prints `stopped`, exit 0. A hub that is still listed, or a listing that fails, until the bound: exit 1, never `stopped`.
- `backoff`, `starting`: prints the phase and `stop it in its pane: Ctrl-C, or close that pane`, exit 3. The wrapper keeps running.
- `not running`: exit 0. `unknown`: no action, exit 4.

Test: `SHUTDOWN_LAB=1 go test ./cmd/hub -run TestStopStatusLab` (its own lab server: not running; a running wrapper; a hub of another state seen and stopped through its own config; backoff with the server stopped, where stop sends no signal; a new server, then stop of hub and wrapper). `TestStatusTrustsOnlyTheLockHolder`: a status file of another pid or another start time is `unknown`; a `gave_up` file without a holder; a reader makes no `restart.lock`. `TestStatusUnknownWhenNotSeen`: no Rex server, a hub without `hub.info`, a state that cannot be resolved, and an unreadable `restart.lock` are each `unknown`. `TestStopNeedsASeenExit` (a script stands in for rex): a failed or hung listing, a hung shutdown, and a hub that stays listed are exit 1 within the bound. `TestStopWaitsForTheLockHolder`: a lock holder without a status file keeps stop from `stopped` until it exits.

## Agent resume

`hub.admin.resume` continues detached agents in new panes (`hub/cmd/hub/resume.go`). It is an admin action: the guard of `hub.admin.*` applies, so an agent or plugin pane is refused. Arguments: `key` (required, at most 128 characters), and one of `agent` (an identity or an alias), `kind` and/or `session_id` (every detached identity of that kind, whose spawn ran in that session), or `release` (an identity).

What it does, for each target identity:

1. Admission. The request is resolved once, and the target list is stored with the operation (`agent_ops`, `agent_op_targets`). A target is refused when the identity is not detached, when it has no spawn record (an agent started by hand), or when its profile has no `resume` template. An identity has at most one active target over all operations and keys; another one is refused as `busy`.
2. Launch. The identity must still be detached in the generation that was admitted (`identities.updated`); otherwise the target is refused ("changed after admission"), and the list is not resolved again. The check and the spawn row share one transaction. A manual resume after that transaction and before the create request can still give two panes (the window is one Rex call). A new spawn row (`resume_of` = the identity, no new agent ID) with the source spawn's profile, session, cwd, title and effective args. The command is the profile's `resume` template, with the same environment as a spawn (`spawnArgv`). The row goes through `sendSpawn`: a refusal fails the target, a lost answer makes it `unknown`. The old pane stays.
3. First prompt. The first look at the new pane records its program (the pane's child: pid and start time). When the program status is `idle` or `done`, the status owner is that program and the program is unchanged, the hub writes `New message. Call messaging_read.` once, as a notice. The other outcomes write nothing:
   - The pane closed, or it has no child (the program exited): the target is `failed`.
   - The child changed (another pid or start time): `unknown`.
   - A ready status whose owner is not the launched program: `unknown`. Rex names as the owner the process group in the foreground when the status was written (lab, Rex 1203), so a status from another program is no proof that the launched one waits.
   - No ready status yet: the hub looks again, up to 120 s after the launch; then `unknown`.

   A resumed Codex runs its hook only at its first prompt; Claude runs it at its start, and then the prompt is not needed.
4. Rebind. The identity's hook reports from the new pane, and the hub sets the identity current there (as for any resume). The target is `rebound` only when the identity is current in the target's own pane: a report from an older pane does not complete a newer attempt.

Phases: `admitted`, `launching`, `launched`, `prompt_sending`, `prompt_sent`, `rebound`. `refused`, `failed`, `released` and `rebound` end a target. `unknown` does not: the pane or the prompt may still act, so the target keeps its reservation. It ends when a look proves an end (the rebind, or the pane closed without one), or with `release`. Rules:

- The reply is the operation's state: `{op_id, done, targets: [{agent, phase, detail?, block_id?, spawn_id?}]}`. The call returns after admission; the caller reads the progress again with the same key and the same request. The same key with another request is refused.
- Writes are at most once. The phase `prompt_sending` is stored before the prompt write and is never written again: after a failed write, or a hub stop during it, the target is `unknown`.
- At most 3 targets are between admission and their end at one time: each launch makes the hub tell every plugin that agents changed. The waits are bounded: 120 s for a ready status, 120 s for the hook after the prompt; then `unknown`. These numbers are implementation choices.
- A pane whose create answer was lost is not adopted by its label (`resolveSpawns`), and the hub writes no prompt to it.

Profiles: see "Profiles and workers" (`resume`). Observed on 2026-10-22 (lab, Codex 0.163.0, Claude 2.1.307): both report program status `idle` before their hook; Codex's hook runs at its first prompt; Claude's at its start.

Palette: with `palette_lua` set, while the hub runs, the generated palette file (`forms.go`) has the hub entry "LazyRex: Resume detached agents" (`hub.admin.resume {}`). It runs at once: it stops nothing. The key is `palette-resume-<seconds>`: a second run in the same second replays the first operation. The palette shows no result; the operation is in the event log (`agent_op_admitted`, `agent_op_phase`). That the Rex app shows the entry and that its run passes the admin guard is inferred: the lab has no app. The restart entries open a form that lists the targets ("Agent restart", "Restart form"). Test: `PALETTE_LAB=1 go test ./cmd/hub -run 'TestHubPalette(StartFails)?Lab'` (a lab server loads the file; the entry reaches `hub.admin.resume`; after the hub stop the entry is gone: `no action named "lazyrexhub_resume_detached"`).

The palette file follows the hub (the hub lead, 2026-10-22; no Rex API adds palette entries for another client, so the config file is the only global palette source). The hub writes the full file at its start and at each change of the commands, then reloads the Rex config. At each orderly stop it replaces the file with its two header comment lines alone, which declare no entry, and reloads the config (see "Stop and exit status"). So after the hub ends, the palette has only the boot entries of `lazyrex_boot.lua`, which setup owns and the hub never writes (Set Up, Start Hub, Stop Hub, Hub Status). The event `palette_written` has `"stop": true` for that write.

- The stop writes only a file that still holds what this hub put there. Otherwise it logs `palette: another writer changed the file; not rewritten at stop` and leaves the file. Two hubs on one `palette_lua` path are a configuration error; the check only keeps a stopping hub from removing another hub's entries.
- A crash or SIGKILL cannot run the stop write: the hub's entries stay until the next hub start, which writes the file again. A start skips the write only when the last successful reload had the same content and the file still holds it.
- A hub restart (for example by the restart wrapper) removes the hub's entries at the stop and writes them again at the start. For that moment the palette has only the boot entries. This is expected.
- A failed write keeps the old file. A failed reload at the stop is logged (`reload_ok` false) and not retried; the next start writes and reloads.

Tests: `palettestop_test.go` (a fake `rex` that records each reload, and can fail or block it): start, a change and stop; a stop that cancels a blocked reload; a palette write that hangs at the stop; another writer; a failed stop reload, and the start after it; the start skip matrix (the file and the last reload); a failed write at the start and at the stop; a crash. Lab: `TestHubPaletteLab` checks the stop file, its reload and the time from SIGTERM to exit (42 ms on Rex 0.0.1792158932-g88fd41, macOS, 2026-10-22; 1.2 ms without the stop write). `TestHubPaletteStartFailsLab` uses the lab fault `-lab-fault point=palette-start,mode=fail` (the start fails after the first palette write; exit 1) and checks the stop file.

Test: `lazyrex-hub test -file agent_resume.lua tests/harness`. A fake agent CLI (`tests/harness/fake-agent.sh`) reports at its start or at its first prompt. `TestResumeLaunchRechecksTheIdentity` (Go): a queued batch target that became current before its launch is refused, and the other target launches. Cases: a Codex-like resume with one prompt, then a same-key replay and a different-request refusal; a Claude-like resume; refusals; one active operation for each agent, a release, and an older pane's late report that does not complete the newer attempt; a lost create answer (`unknown`, the same spawn on a retry, `busy` for another key, then a release); a status whose owner is another process group (`unknown`, no prompt written; the fake counts its prompts).

## Agent restart

`hub.admin.restart` stops current agents and resumes each one in a new pane (`hub/cmd/hub/agentrestart.go`). It is an admin action (the guard of `hub.admin.*` applies). Arguments: `key` (required, at most 128 characters), and `agent` (an identity or an alias), or `kind` and/or `session_id` (every current identity of that kind, whose pane is in that session). The operation is stored as for a resume (`agent_ops`), with the same reply, replay and `busy` rules; a key used for a resume is another request. `hub.admin.resume {key, release}` releases a restart target too.

Admission refuses a target when the identity is not current, its pane has no spawn record (an agent started by hand), the profile has no `resume` template, or the check below fails. The hub stops only the agent process that the check names:

- It is the pane's child (never an ancestor, never a descendant), with the pid and start time recorded at the identity's report (`identity_panes`).
- At each accepted report of a new process generation, the hub records the child's executable (Rex `path`) and exec path (`invoked_path`) from the same Rex answer. The first complete values stay for the generation; they are cleared when the generation ends. The recorded exec path must be the configured program: the profile's `command[0]` (or `resume[0]` for a resume spawn), resolved with the hub's PATH as a spawn resolves it. Rex reports an exec path only while it still resolves to the executable (docs/rex-protocol.md), so after a CLI update the record is the proof, not the live answer. argv0 is never used.
- The live executable must equal the recorded one: an exec to another path in the same pid is refused. This detects a changed path. It is no proof against an exec of the same path, and the check and the signal are two steps, not one atomic operation.
- A row without a record (an identity reported by an older hub): the live exec path must be the configured program; a null one refuses ("cannot be verified").
- A null live executable refuses: on macOS Rex reports none after the running executable was removed (a version cleanup); restart the agent by hand. On Linux Rex reports the old path then. A moved executable is refused as a changed path.
- The exec path's base name is the CLI of the identity's kind (`claude`, `codex`): v1 restarts a direct agent CLI only. A pane shell reports the shell as its exec path, and a script launcher its interpreter (lab, 2026-10-22), so both are refused ("a shell or a launcher is not restarted"). This is a name rule on the exec path, not on argv0; it cannot tell a program that is deliberately named after the CLI.
- Missing evidence refuses: no recorded pid or start time, or a start time that cannot be read now.

The check runs at admission and again before each key write and each signal. The stop, each step a stored phase (`restart_admitted`, `stopping`, `terminating`, `killing`):

1. Exit keys, only when the agent is the terminal's foreground process: one Ctrl-C for Codex, two 300 ms apart for Claude (S1). Otherwise no keys, and SIGTERM at once.
2. No exit within 5 s: SIGTERM. No exit within 5 s more: SIGKILL. No exit within 5 s more: `failed`. The waits are implementation choices. Only the agent's pid is signaled, not its process group.
3. The process is gone when the system has no such pid, the pid has another start time, or Rex (the pane's parent) answers that the pane has no child. A failed read is no answer: the step looks again, and after 5 s the target fails. When the process is gone, the identity is detached as the pane's exit event would detach it, and the target becomes `admitted` with that generation: the resume steps follow ("Agent resume"). The old pane stays.

The reply's target has `stop`, the stop's outcome, kept through the resume and retries (`stop_result`): `exited before any key or signal`, `exited after the exit keys`, `exited after SIGTERM`, `exited after SIGKILL`, `exited before SIGTERM` (or SIGKILL: the signal found no process), or `not stopped: <why>`. A signal that fails (other than no such process) ends the target as `failed`. A check that fails during the stop ends the target as `failed` ("nothing more was sent"). After a hub stop during the stop, the next step checks again and continues; keys or a signal can then be sent again to the same checked process. A restart keeps its in-flight slot from its stop through its resume.

Test: `lazyrex-hub test -file agent_restart.lua tests/harness`; `TestRestartCheck`, `TestRestartCheckRefusesAShell`, `TestProcGone` (Go). The fake agent CLI is a program of its own (`lazyrex-hub testagent cli`, `testcli.go`), run through lab symlinks named `codex` and `claude`. Cases: a restart by kind; one Ctrl-C (Codex-like), two (Claude-like), SIGTERM, SIGKILL, each with its `stop` in the final reply and on a retry; no keys when another process is in the foreground; a symlink moved after the record (also with a later report while the live exec path is null); a stale report (an older reporter that reports after a newer one) after the symlink moved; a forged argv0 (refused, the process stays); a pane shell (refused); an exec to another path in the same pid (refused); a check that fails during the stop (`failed`, `not stopped: ...`); no record and a null exec path (refused, then allowed when the symlink resolves again); `busy` and a key reused for a resume.

### Restart form

The palette entries "LazyRex: Restart Codex agents…" and "LazyRex: Restart Claude agents…" call `hub.admin.restart.form {kind, ctx}` (`hub/cmd/hub/restartform.go`). The admin guard applies, and `ctx.client_id` must be the caller. The hub opens a form that it owns itself: the owner is `hub:restart`. No plugin can have that name: the manifest name rule (`^[a-z0-9_-]+$`) has no colon, and `hub.register` refuses it. The renderer, the view rows and the reconcile pass treat the form as any other form.

- **Open.** The hub reads the restart targets of the kind once (the same check as `hub.admin.restart`) and stores a snapshot in the form context: a revision (1), a new operation key, and for each target its identity, pane, spawn, pid, start time, executable, exec path and program. A target that the check refuses is stored with its reason.
- **Render.** The render only reads. It shows "To restart" (the snapshot targets) and "Not restarted" (the refused targets, with the reason). It reads the targets again and marks each listed target that changed: another agent, refused now, moved to another pane, another spawn, process changed, executable changed. While a target is marked, the restart action is disabled.
- **Restart.** The action name is `restart@<revision>`. The renderer sends nothing else. Another revision than the stored one is refused (`stale`). The hub then runs `hub.admin.restart` with the stored key and kind, through the same admission code. Admission reads the targets again and compares each with the snapshot. One difference refuses the whole batch (`conflict`, "nothing was restarted"). A lookup by key comes first, so a retry after a lost reply returns the same operation and never reads the targets again.
- **Refresh.** Refresh is an action (`refresh@<revision>`), never part of a render. It takes a new snapshot, with the next revision and a new key, and records its view operation key. Another revision than the stored one is refused (`stale`). A retry of the same operation key (a lost reply, or a crash after the context write) changes nothing: no new snapshot, no new key.
- **Queued operations.** An operation waits in `view_ops` until the form's one worker runs it, and a Refresh before it can change the form. So each operation reads the view row's context when it runs, never a copy from its admission. For this owner, `viewAdmit` keeps the context out of the operation's arguments and fingerprint, so a retry of a key still matches after a Refresh. Plugin views do not change.
- **After admission** the form is frozen: Refresh is refused (`invalid_state`), and the render shows only the state ("Restarting…", "Done.") and the outcome of each target (phase, then `stop`). A new restart needs a new form.

Residual risk: the restart action reaches the hub through the form's renderer pane, not through `hub.admin.*`. The admin guard checks the palette call that opens the form, not the keys that select the action. A process that can send keys into the renderer pane can press Restart. It can restart only the stored snapshot, and only while no target changed. The same holds for every user action of a view (see "The admin guard" under "The work session").

Tests: `TestRestartTokenChanged`, `TestRestartFormStaleRevision`, `TestRestartFormReadsTheStoredContext`, `TestRestartFormQueuedOperations` (two queued operations through `viewAdmit` and `runOps`, a same-key Refresh retry, a crash before the record), `TestRestartFormFrozenAfterAdmission`, `TestRestartFormCallErrors`, `TestRestartFormOwnerIsReserved`, `TestPaletteHasTheRestartEntries`. Lab: `lazyrex-hub test -file restart_form.lua tests/harness` (open, a UI pass, restart, a lost-reply retry, a change behind the form, Refresh with a same-key retry, stale revisions, a hub crash before and after the admission with the renderer attaching again, an agent pane refused).

## Process log

The hub writes one line for each answered call (`Hub.answer`, `hub/cmd/hub/calllog.go`): `<action> from <client> pid <pid> -> <error or <nil>> in <ms>`. One exception: a fast, successful routine read gets no line.

- The routine reads are `hub.store.get`, `hub.store.scan` and `hub.sql.query`. Before Messaging f60a990 (2026-10-21), the live Messaging plugin made tens of thousands of them a minute, and a line for each made the log grow by about 1 GB. After f60a990, it makes about 500 to 1,000 a minute.
- A routine read keeps its line when it fails, when its reply was not delivered, or when it takes 250 ms or longer.
- Every other action keeps its line, also an action that is added later. A call that changes state for another party (admin, register, spawn, acting as the user) needs its line.
- The hub counts the skipped calls. The first call one minute or more after the count started writes one summary line before its own line, then the count starts again: `routine calls without a line since 15:00:00: hub.store.get 3 (max 12.0ms), hub.store.scan 1 (max 40.0ms)`. A minute without skipped calls writes no summary. No goroutine writes the summary: the first call after the minute writes it. After a burst of reads with no later call, the counts wait for the next call. So a missing summary line does not mean that no reads occurred.
- `events.jsonl` does not change: the hub events stay in that file.
- Where the lines go: under `-restart`, to `<state>/hub.log`, size-bounded (see "Restart wrapper", "Log"). With dev.sh, to its redirect `<state>/dev/hub.log`, which has no size bound. A crash trace goes to the same place as the lines.

Tests: `TestCallLogKeepsEveryLineButFastRoutineReads` (the 250 ms boundary included), `TestCallLogSummary`.

## Spawn directories and path locks

Status: lab (Claude, Rex 1203, 2026-10-16). Contract agreed with Codex for the worktrees plugin.

- `hub.spawn {cwd}`: an absolute, existing directory. It replaces the profile's cwd. When given, the text as given is part of the retry identity: a retry returns the original result also after the directory is removed. Observed in the lab.
- The hub always records an explicit effective cwd (the argument, else the profile's cwd, else the home directory), with symlinks resolved (`filepath.EvalSymlinks`), and passes it to Rex. `hub.workers` shows it as `cwd`. Observed in the lab: a spawn with cwd `/tmp/…` ran in `/private/tmp/…`.
- `hub.cwd.lock {path, key}` (registered plugins): one operation (plugin + key) locks a resolved path. It answers `{locked: false, workers}` while a pending or open worker has a cwd in the path, or an unknown cwd (a row from before cwd was recorded). It also asks Rex for the live cwd of every terminal pane in every session (`com.superlogical.terminal.process`, child and foreground process, from the OS). It answers `panes` for each pane in the path, also a user's shell that ran `cd` into it. This check is a snapshot: it cannot stop a shell from changing to the path after the lock. Observed in the lab (Rex 1203): a shell that ran `cd` into the tree refused the lock until its pane closed. A retry with the same key succeeds; another key gets a conflict.
- While a lock exists, `hub.spawn` refuses a cwd in the path. The lock check, a second check of the resolved directory, and the INSERT share one transaction. The store has one connection, so transactions run one at a time: a removal (lock, remove, unlock) either refuses the spawn or sees the admitted worker and refuses the lock.
- A lock refuses a path that overlaps (is a parent or a descendant of) another operation's lock. Observed in the lab: a spawn into a locked directory got "conflict: … is locked by worktrees".
- `hub.cwd.unlock {path, key}`: only the owning operation; idempotent; works after the path is removed. `hub.admin.unlock {path}` is for manual recovery. Locks are durable: the plugin keeps a lock until it can prove its operation finished or was refused.

## Removed: app mode

Status: removed (the user's decision, 2026-10-19: "remove all app mode stuff including state"). Every plugin's tabs open in the one LazyRex session (decision "One LazyRex session; app mode retired").

An older build had app mode. Opening an app hid the user's own sessions (it detached their blocks), and "Exit LazyRex" (`hub.ui.exit`) rebuilt them. Each plugin also had its own app session (`app:<plugin>` in `ui_sessions`, labeled "LazyRex: <name>"). The hub has none of this now. It has no `hub.ui.exit`, no `hub.ui.mode`, no `hub.changed {what: "mode"}`, and no `session` argument for `hub.ui.tabs`.

At startup, the hub removes the state of app mode (`hub/cmd/hub/appmodestate.go`). It changes only hub records, and it makes no Rex call:
- It drops `ui_cycles`, `ui_hidden`, `ui_exit_keys` and `ui_apps`.
- It deletes the restore placeholders of app mode (`ui_restores` rows whose view ID starts with `hidden:`) and `ui_state` `active`.
- It keeps `ui_sessions` rows, views and intent records.
- When it removed something, it emits one `app_mode_state_removed` event with the counts.

Effects on an old database:
- The hub restores nothing and closes no pane. A pane that an old active or restoring cycle hid stays detached, and its process keeps running. The user can show it again with Rex (`rex move`). This is intended: it is not a recovery.
- An old late restore placeholder stays open.
- A view in an old session (an app session, or a session of an older hub) is legacy. It retires, and its tab opens again in the LazyRex session.

Legacy session cleanup (`settleLegacySession`, on every UI pass, for each session with a legacy `ui_sessions` row):
- It waits until every view row in the session is closed. The reconcile loop retires the rows of loaded plugins (an open view retires; a prepared, sending or exited row closes). A row of a plugin that is not loaded keeps the cleanup pending.
- Rex has no such session: the hub forgets it.
- The session has blocks (for example user panes): the hub deletes its legacy rows and stops managing it. The session and its panes stay with the user (event `legacy_session_released`).
- The session has no blocks: the hub destroys it, then forgets it. A failed destroy keeps the rows, and the next pass tries again (event `legacy_cleanup_failed`).

Lab (observed on Rex 0.0.1792158932-g88fd41): `TestAppModeStateRemovedAtStartupLab` (a detached block of an old cycle and an old app session with a user pane: no layout change) and `TestSettleLegacySessionLab`.

## Navigation to an agent

- `hub.navigate.agent {agent}` shows the tab of a live agent (an identity or a legacy agent) on the apps, like `hub.navigate.worker`. It refuses an agent with no open tab or a detached tab. Agents cannot call it. The messaging thread overview calls it when the user clicks an agent. `hub.agents` reports `activity` (the agent's program status, for example `working` or `idle`) for a live agent. Lab (2026-10-18): a click on an agent row in the overview made the agent's tab the active window and closed the overview. Untested: a jump to another session on a real app.

## One LazyRex session (mount and detach)

Status: superseded: every plugin's tabs are mounted in the LazyRex session now (see "Removed: app mode"). The home tab is gone too (see "No LazyRex tab"). Lab (Claude, Rex 1203, 2026-10-16).

All plugin views live in one Rex session, labeled `lazyrex`. The home plugin (config `home`, default `lazyrex`) is always mounted, first. Its tab is the menu. One other plugin is **active**, and its tabs are mounted after the home tab. The views of every other plugin are **detached** with `session.detach_block`: their processes and unsaved input stay.

- `hub.ui.navigate {plugin, tab}` commits the active plugin (table `ui_state`) before any layout change, then the worker reconciles. Navigation to the home plugin does not change the active plugin.
- One worker reconciles the whole session, serially. It reads placement from `session.list_blocks`, and it derives each view's window from Rex. It does not trust the stored value.
- Restore: Rex reattaches a detached block only beside a visible anchor. The hub opens a tab with a placeholder pane, calls `session.move_block`, and closes the placeholder. A placeholder's label and its process arguments both name the view. The cleanup closes a leftover placeholder only when both match.
- The user cannot close the home: it opens again. User suppression and the crash count of other tabs are unchanged.
- Views in sessions of an older hub (one session per plugin) are **legacy**. The hub asks each one to retire (`view.retire`, per attachment, repeated every 10 s while refused). The renderer uses the reload gate: it saves composer drafts, waits while a form is dirty or a draft is in conflict, and shows "Retirement waits". Then it exits. The hub closes that exact pane, then the row. The tab's new view opens only after the legacy view is gone. A recorded legacy session with no blocks left is destroyed. A legacy exit counts no crash.
- Durable intent: `ui_state` holds `active` and `nav` (written together). The nav intent goes only after Rex confirms the focus. A restore records its nonce (`ui_restores`) before the placeholder exists. The record goes only when that placeholder is confirmed closed. More than one session labeled `lazyrex` is a refusal.
- The placeholder runs `/bin/sh -c "while :; do sleep 600; done" lazyrex-placeholder <nonce>`. With a plain `sleep 600` the shell execs sleep, and the nonce leaves the process arguments (observed in the lab).

Lab results (observed):
- Mount: the LazyRex tab, then Messages' 9 tabs, with the navigation target active.
- Swap to a test plugin: Messages' 9 views were detached, and the test plugin's 3 tabs were mounted after LazyRex.
- Swap back: the same 9 renderer PIDs. Unsaved composer text was visible, and typing worked after the restore. The 9 restores took 0.21 s.
- A placeholder with the right label and arguments was closed. A pane with only the label was not closed.
- A hub restart during a swap converged: 13 blocks, no duplicates, no placeholders. The in-memory focus target was lost, so the restart kept the plugin mounted but did not focus the target tab.
- A user close of the home tab: the home came back, first.

- A crash before the move (a recorded placeholder, the view still detached): the placeholder was closed. A crash after the move (the view placed beside a recorded placeholder): the placeholder was closed, and the view was detached again (its plugin was not active).
- Migration from an older hub, with an old-hub lab run: 9 Messages views and the home in per-plugin sessions. The Claude thread had unsaved composer text, and the home had an unsaved Launch title. With the new hub, the 9 Messages views retired and their session was destroyed. The composer text appeared in the new Claude view. The old home showed "Retirement waits" and stayed until the title was cleared; then it retired, its session was destroyed, and the new home opened first.

- Resize after a swap out and back: the restored Claude view redrew at 60 and at 110 columns (its composer border followed the width), and the unsaved draft stayed.

Untested: app selection (no app in the lab). A draft conflict during retirement has renderer test coverage only.

## No LazyRex tab (2026-10-22)

Status: code with tests; next hub release (coord-parallel-push-1, item 2; decisions Q1 A and Q2,
2026-10-22). The home plugin has no tab. Every LazyRex command comes from the palette and its
popups.

- Any plugin may declare no tabs; the home plugin then has only its panel and forms (`hub.ui.tabs
  {tabs = {}}`; the hub no longer refuses it from the home plugin).
- The home plugin's palette entries:
  - "Agents…" (panel `/panels/agents`, the landing). It lists the agents (Message, only while the
    messaging plugin is registered) and the workers and accessories. An active worker has
    "Manage…": the menu form `/forms/worker/<spawn_id>` with Open (when it has a window), Stop and
    Close. A failed worker has Dismiss. Another worker with a window has Open.
  - "Stop Worker…" (`/forms/stop`): a select of the active workers.
  - "Launch Profile…" (`/forms/launch`): one launch form per profile. The plugin registers it only
    while no other registered plugin offers a launch: a palette command whose action is `hub.spawn`,
    as Profiles' Start Agent… and Start Accessory… have. The capability decides, not a plugin name.
    The plugin registers again on `hub.changed` only when its command set changes.
- The home tab's plugin list is gone: each plugin has its own palette entry, and Hub Plugins has
  "Plugins…".
- The LazyRex session stays. The hub still creates it, and it can be empty. Thread tabs with no
  known home still open there (phase 2 decides the session's creation and the thread fallback).
- Upgrade: a home view whose tab the home plugin no longer declares retires through the legacy gate
  (`view.retire`: the renderer leaves when nothing is unsaved; its exit closes the pane). The hub
  never closes it as undeclared. The missing declaration is durable, so the hub asks again after a
  restart. A home view in a legacy session retires as before. The home plugin refuses route `/`
  ("Unknown home route or user"), so an old view shows the hub's error tree until it retires.
  The release runbook for this hub release closes every open home view before the upgrade
  (coordinator, coord-c3-gate), so no view with unsaved input meets the refusal.
- Reconciliation covers every plugin with a declared tab or a live tab view (`uiPlugins`). A
  non-home plugin that declares no tabs gets a pass, so its open tab view closes as undeclared.

Tests: `tests/harness/tests/no_tabs.lua`, `tests/harness/tests/home_retire.lua` (the fixture as
home: a view retires, a view with unsaved text waits, a restart resumes the retirement),
`TestUIPluginsIncludesLiveTabViews`, `TestHomeRouteRefusedLab` (lab: a home view at `/` in the
LazyRex session gets the refused render, then retires, closes once and its pane goes), and
`plugins/lazyrex/tests` (README coverage table).

### Worktrees plugin

The form submit contract also supports long operations. A `view.action` result with
`status: "running"` keeps the stored operation pending. The hub retries the same
key and arguments in admission order. It runs no view effect until the operation
returns a final result. A definite plugin error fails the operation as before.

`hub.cwd.check {path}` gives registered plugins read-only removal advice:
`{path, available, workers, panes}`. It uses the same live pane and worker checks
as `hub.cwd.lock`, but reserves nothing and writes no lock. A removal must still
call `hub.cwd.lock`; advice can become stale before submit.

Observed on Rex `0.0.1792158932-g88fd41` in the Worktrees Codex lab: a running action
stayed pending, blocked later actions, and resumed with identical input through a
fresh hub object. Final success and refusal ended the operations. Directory advice
found an unmanaged terminal and a worker with an unknown directory, and created
no lock. Run `tests/worktrees-contract-lab.sh` with `WORKTREE_LAB_SOCKET` set to an
isolated `/tmp/rexlab-worktrees-<name>/rex.sock`. Set `GO_BIN` if Go is not on PATH.

`plugins/worktrees.lua` exports these agent tools:

- `worktree_create {repo, branch, base?, key}` creates a linked Git worktree.
- `worktree_list {repo?}` returns branches, paths, status, and workers with a matching launch directory.
- `worktree_remove {path, key}` removes a clean managed worktree. It preserves the branch.

The default root is `~/.lazyrex/worktrees/<repo directory name>/<branch>`.
The repository and removal paths must be absolute. A branch can contain `/`.
`base` applies only when the branch does not exist. Its default is `HEAD`.
A matching existing worktree returns `created: false`.
Two repositories with the same directory name can collide. The plugin refuses a conflicting path.
It does not change the requested directory scheme or replace a worktree.
A lab can pass `worktrees_root` to the Lua script. Production uses the default root.

Choose a unique key for each mutation. Preserve the key and arguments after an unknown result.
A result with `status: "running"` means the caller must repeat the same request to get the final result.
The hub stores completed results. A retry returns the stored result after a plugin or hub restart.
A recorded refusal also stays fixed. Use a new key after you remove the cause of that refusal.

Git owns worktree creation, branch selection, and removal. Rex owns the Git mutation process.
Each mutation uses an unplaced terminal block with a unique operation label and `on_completion: false`.
The plugin records the intent before it sends `session.new_block`.
After an unknown reply, it finds the original block by its label. It does not start a second process.
An absent label in a live session does not resolve an unknown spawn. The original request can still create a block.
A confirmed missing recorded block, or a destroyed session, permits reconciliation from Git.
Create recovery requires a matching clean tree. Remove recovery requires an absent path and an absent Git registration.
It reads completion from `com.superlogical.terminal.process.last_exit`.
It closes the block only after it commits the result. Startup completes pending block cleanup and lock release.

Removal first acquires `hub.cwd.lock`. The hub checks pending workers, open workers, and live Rex terminal directories.
The live directory check is a snapshot. It cannot stop an existing shell from changing directory after the check.
The durable lock prevents new hub workers from starting inside the tree while removal is pending.
The plugin refuses tracked changes, untracked files, and ignored files. It never passes `--force` to Git.
The ignored-file check precedes removal. Git can delete an ignored file created between that check and removal.
The plugin cannot prevent another process from writing into the tree during removal.
Only linked worktrees beneath the managed root can be removed. The main repository cannot be removed.

Worktrees has no tab. Its palette commands are **Worktrees…**, **New Worktree…**,
and **Remove Worktree…**. The landing panel (`/panels/worktrees`) shows branches,
paths, status, workers, and pending operations. A row opens its action menu.
Forms share the side slot. When a form ends, the hub restores the panel.

The default repository comes from the stored source pane's `terminal.pwd`.
When the pane has no reported directory, the plugin uses the foreground process
directory, then the child process directory. Recorded repositories are the fallback.
For a new branch, an omitted base means HEAD of that selected worktree.
Create opens a plain shell in the new worktree. Open and Start Agent use the hub's
profile and spawn services. Their retries retain the original spawn key.
An unknown launch result stays pending; it does not invite a second launch.

Removal advice runs asynchronously and reserves nothing. The confirmation form
names one worktree, so its advice and submit refer to the same path. Submit repeats
the authoritative guards. Worker and directory events invalidate cached advice.

Read-only status scans run asynchronously. The panel renders a cached snapshot.
Refresh reuses a scan that is still running. It requests one new scan after the current scan finishes.
The plugin has no idle scan timer. It refreshes on view focus, worker changes, terminal exits, and relevant directory or block events.
The Worktrees manifest sets `"events": "all"`. The hub passes `plugin_session` because Rex cannot combine `--all` with `-s`.
An explicit query starts a fresh scan. Retries receive the current scan and its completed result.
A one-shot timer collects results only while a status scan runs. Rex exposes no completion callback for `rex.exec.start`.
Completed scans call `hub.ui.invalidate`. Content acknowledgements do not start another scan.
Metadata reads remain synchronous: the catalog, worker list, and `git worktree list` run before each scan.
An exit in a pane with an unknown directory also refreshes the snapshot. Known unrelated panes do not trigger an exit refresh.
File edits alone have no Rex event. Use view focus or an explicit query to refresh after such edits.
`checking` and `refreshing: true` identify an incomplete scan. `unavailable` identifies a failed scan.
Worker names refer to launch directories. The removal check also examines current Rex terminal directories.

An unresolved operation retains its intent and any directory lock. The panel shows its path and phase.
If the Rex block still exists, inspect that block and repeat the original request.
If Git has a partial result or the operation block is missing, inspect Git and Rex before any manual repair.
`git worktree list --porcelain` shows Git's registered paths. `git worktree prune --dry-run` previews stale registrations.
Do not prune a live worktree or unlock a path while its Git process can still run.
`hub.admin.unlock {path}` releases a directory lock after operator review. It does not resolve the plugin's stored intent.
The panel has a user-only **Discard stopped operation** action. Agent tools cannot discard an intent.
Discard records the operation as discarded and releases its lock. It does not remove files or branches.
Discard refuses a running or starting Git process.
For an unknown spawn without a recorded block, discard requires a confirmed destroyed session.
The plugin does not use a timeout as proof that a process cannot start.

Tests use an isolated Rex server:

```sh
REX_SERVER=unix:///tmp/rexlab/rex.sock node spikes/03-ui-framework/tests/worktrees.mjs
HUB_BIN=/tmp/lazyrex-hub VIEW_BIN=/tmp/lazyrex-view REX_SERVER=unix:///tmp/rexlab/rex.sock \
  node spikes/03-ui-framework/tests/worktrees-hub.mjs
```

The first test uses real Rex Lua and Git with a controlled hub.
The second test uses the real hub, a real agent pane, and SQLite storage.
Set `GIT_SOURCE` for the second test to use an owned shared clone of an existing repository.
The test does not add a worktree or branch to that source repository.

`tests/worktrees-contract-lab.sh` checks the hub's pending-operation consumer and directory guards.
Run it with `sh` and set `WORKTREE_LAB_SOCKET` to `/tmp/rexlab-worktrees-<name>/rex.sock`.
Set `GO_BIN` if `go` is not on `PATH`.
`tests/worktrees-panel-lab.mjs` checks the palette, panel, forms, shell launches, and agent launch through the renderer.
Its header gives the lab setup commands.

Observed on Rex `0.0.1792158932-g88fd41`: the controlled-hub test passed with source-directory defaults,
source HEAD selection, form retries, launch retries, removal advice, and four crash recovery cases.
The three focused hub checks also passed on that version.

Observed on Rex `0.0.1792158932-g88fd41`: the real-hub test passed with an owned shared clone of restty.
Creation completed in 657 ms in that run. This measurement is not a performance guarantee.

## Notices (notify ops)

A plugin with a `notice` in its manifest sets a pending notice for an agent with a `notify` op in
`hub.store.batch`. The hub writes the notice text into the agent's pane when the agent is ready (see
"Profiles and workers"). The ops:

- `{notify: {agent, pending: true}}`: a pending notice with a new revision. Each one gives one
  nudge (one pane write), also while an earlier nudge is unread.
- `{notify: {agent, pending: true, once: true}}`: one nudge until a read (the user's rule,
  2026-10-22). The revision is new only if the row is not pending, or the agent read since the last
  revision. Otherwise the row stays as it is and no nudge follows.
- `{notify: {agent, read: true}}`: the agent read. The next `once` op gives a nudge again. The
  read also ends the nudge of the current revision: one not written yet is not written, and the
  repeat (below) is cancelled. `pending` does not change. A read for an agent with no row does
  nothing. `read` takes no `pending` and no `once`.
- `{notify: {agent, pending: false}}`: no pending notice.

`once` is a boolean, and only with `pending: true`. Any other form is a malformed request, and
nothing in the batch is written. A hub before this change ignores `once`, and refuses
`{agent, read: true}` because it has no `pending`. So a plugin sends the new forms only after the
hub release that has them.

For every op form, the hub stores each nudge that it writes (`notice_deliveries`: the revision, the
time, and the conversation: the block, the child pid and its start time). A hub restart, a shim
reconnect or a rebind does not write it again. Another child process in the pane (a new
conversation, a spawn, a resume or a restart) gets it again: that conversation never saw it. A
nudge with no read and no new revision for 30 minutes is written once more (`notice_written` with
`repeat: true`), then not again until a read or a new revision. The loop arms its own wake for the
repeat when it writes the first nudge, so the repeat needs no other event. In test mode,
`hub.test.notice_renudge {ms}` sets the repeat time for the hub run. A failed store write of the delivery
leaves the hub run's memory record of that delivery: the same fields as the stored row (revision,
first-write time, conversation, repeated), so the repeat and its time stay right. Only a restart can
give the nudge once more: never fewer nudges.
Tests: `TestNoticeOnceUntilRead`, `TestNoticeOpForms`, `TestNoticeDue`, `TestNoticeDeliveryStored`,
`tests/harness/tests/notice_once.lua`.

## Reply size

Rex closes a client's connection when one frame from that client is larger than the welcome's
`max_message_bytes` (1 MiB on the Unix socket; rex-protocol.md, "Rex Lua runtime"). One large
result, for example a `hub.store.get` of many records, closed the hub's connection for every caller.

The hub measures every `client.reply` frame before it sends it, on the plugin face (`Hub.answer`)
and on the agent face (`agentAnswer`). The limit is `max_message_bytes` minus 1 KiB (1 047 552
bytes). A welcome without limits gets the reference value, 64 KiB. Over the limit, the hub sends
this error and stays connected:

- Code `reply_too_large`. The message names the action, says whether its handler returned a
  result or an error, and gives the attempted frame size.
- The guard does not know the outcome: a result can be a pending or failed operation, and an
  error can follow partial changes. The error does not undo changes and does not tell the final
  state. Before a caller retries a change, it keeps the operation key and checks the status.
  If the action stored its result under that key and the stored result is still too large, a
  replay gives the same error. The guard adds no replay of its own: a pending operation's status
  can change, and some actions (for example `hub.stop`) ignore the key.

The margin: Rex's response to the caller is larger than the reply frame (16 to 32 bytes in the
lab), and a caller with a 1 MiB limit refuses a larger response with `too_large`. 1 KiB covers
that difference and the length of the request ID.

What a Lua plugin sends is still limited to 64 KiB (its replies, view trees and writes). The guard
does not change that limit.

Tests: `hub/cmd/hub/replyguard_test.go` (unit), `replyguard_lab_test.go` (`REPLY_LAB_SOCKET`: the
server's limits, both faces), and `tests/harness/tests/replyguard.lua` (the plugin face through
the real hub; the hub client and process stay the same).

## Agent actions through Rex

The hub owns a second Rex client labeled `lazyrex`. This client exposes agent actions and never attaches a session.
Rex removes this client when its connection closes. A restarted hub can use the same label without a lost client collision.
The internal hub client keeps the label `lazyrex-hub` for plugins, hooks, and existing MCP adapters.

Discover actions with `rex -S "$REX_SERVER" --autostart=false -C lazyrex actions`.
Call an action with `rex -S "$REX_SERVER" --autostart=false -C lazyrex do messaging.read`.
Rex supplies descriptions, schemas, argument validation, and the caller PID.
The hub forwards registered plugin actions through `hub.forward`.
A plugin accepts that assertion only from its registered hub client.
Direct plugin calls and MCP tools keep their existing caller attribution.

A conversation identity replaces a legacy name when its hook first reports from that pane.
Messaging migrates mutable records in one batch before it handles the next request.
Historical messages and saved operation results stay unchanged. Readers show canonical sender and recipient IDs.
A permanent legacy alias follows its first conversation, including after the pane starts another conversation.

Spawns follow the alias at lookup (the coordinator, 2026-10-19; replaces a row rewrite at alias time that missed work made after the alias). A spawn row keeps the ID that made it; `hub.workers` reports that recorded `parent`. The agent of an ID is its identity plus each legacy ID that the identity took over (`identity_aliases`, read at each call; a legacy ID belongs only to its first identity). `hub.spawn` and `hub.spawn_window` replay a key made by any ID of the caller's agent, and `hub.stop` accepts any ID of the parent's agent. A key that two IDs of one agent used before they became one is refused (`conflict`), even for equal requests: a replay cannot tell which request it repeats. Each of those workers stays stoppable by its spawn ID. The admission transaction checks the alias set again before it inserts: the hub's database has one connection, so the check and the insert are atomic with every other admission and every alias write. A second ID of one agent that admits the same key at the same time gets `conflict`; a retry with that key replays the first row. Observed before the fix (lab, `tests/harness/tests/spawn_migration.lua` and `spawn_resume.lua`): the same key from the identity created a second worker.
A conflicting pair of nonempty drafts stops migration with an error and preserves both drafts.

See `../skills/lazyrex/SKILL.md` for agent instructions.
The skill is stored in this repository. Installing it in an agent's configuration is a separate step.

### Thread titles from Rex tabs

The hub sends `view.rename {route,user,title}` when the user renames a messaging
thread tab. The plugin stores the title for that user and thread. The title does
not change the participant's name or identity. The plugin rejects empty titles
and routes that do not identify a thread. The inbox, thread view, and declared
tabs use the stored title. Unread counts remain a separate tab suffix.

An unnamed Claude or Codex identity uses its kind and the first eight characters
of its suffix for display. Routes, recipients, and store keys retain the full ID.
Alias migration moves thread titles to canonical keys. If both keys have titles,
the title with the latest store revision wins.

Verified with the real Lua plugin and a controlled hub on Rex
`0.0.1792158932-g88fd41`: trusted rename, invalid rename refusal, title persistence,
alias migration, and short identity display. The hub owns the Rex event path and
must suppress events from its own label writes. Combined event delivery still
requires the hub integration test.

### Palette forms and thread menus

Status: candidate in the lab. The palette contains global commands only.
Messaging declares `new_thread` at `/forms/new-thread`. The form lists agents in
the source session. A local toggle shows agents from other sessions. Selected
agents remain selected when the toggle hides them.

The hub owns command registration, generated Rex config, floating form panes,
and their lifetime. The plugin owns form contents and thread changes. The
renderer owns popup menus and local field visibility. Thread menus open through
right-click or Ctrl+O. Menu items use the existing view action protocol.

Where forms and panels open (an experiment, the user, 2026-10-22). The hub config key `ui_slot`
is `"split"` (the default: a right split of the window, ratio 0.55) or `"layer"` (a frontmost
floating layer in the same window, bounds `[0.15, 0.15, 0.7, 0.6]`, with focus). Any other value
stops the config load. Both modes use the same block, label, rows and recovery; only the Rex call
differs (`slotPane`). A replacement in layer mode opens a new layer, then closes the old pane, and
its layer goes with it. A transcript pair inside a layer is checked in that layer (`wrapPair`).
The key goes away when the user decides. Tests: `TestUISlotConfig`, `TestSlotPaneLayer`,
`TestSlotLayerLab` (`PANELS_LAB_SOCKET`).

The return of a closed form in layer mode (contract v2.1, 2026-10-22). When a form closes without
an action (Esc, its exit, the user's close of its pane, a hub close), the hub opens the panel that
the form replaced again. In layer mode an unfocused layer would cover the user's pane, and a
focused one must not take the focus from another place. So:

- When a slot's first layer opens, the hub records the window's focused block, if that block is in
  the window's tiled layer (`under_block_id` in the form context; `""` otherwise). A replacement
  (panel to form, form to form, form to panel) keeps the replaced view's value and never reads it
  again. The value is stored before `session.new_layer`.
- The return opens, with the focus, only when the session's active window is the return window and
  that window's focused block is exactly the recorded tiled block. The hub reads this under the
  window lock, just before `session.new_layer`.
- Otherwise no layer opens. The return's replacement row ends in the terminal state `skipped`, and
  the hub emits `form_return_skipped {view_id, replacement_id?, why}`. `why` is
  `window_not_active`, `focus_moved` (another block or layer has the focus), `under_unknown` (no
  recorded block, also for a form from an older hub) or `read_failed`. The user opens the panel
  again from the palette.
- `replacement_id` is absent in one case: `read_failed` when the hub cannot read the session at
  the form's close. Then no replacement row exists. Every other skip has its row. A consumer must
  not read a missing `replacement_id` as an error.
- Limit: the read and `session.new_layer` are two calls. The focus can move between them, and a
  move away and back before the read cannot be seen. Rex has no conditional focus operation.

Split mode and the return after an action (the return replaces the form) do not change. Tests:
`TestSlotLayerReturn*Lab` (`panels_return_lab_test.go`).

The hub stores `{session_id, window_id, source_view_id, source_route,
source_block_id, source_pwd, source_pwd_error}` as form context. Every form
render and action receives this object in `args.context`. The plugin does not
inspect later focus to find the source thread. The context also carries hub-internal
fields (`return`, `panel`, `restart`, `under_block_id`). A plugin must not read or depend on them;
they can change in any release.

The hub reads the source pane's working directory once, at the open (a palette
command, or a form or panel effect), and freezes it in the context:
`source_pwd` is the raw `terminal.pwd` value (for example an OSC 7
`file://host/path` URI; the plugin decodes it), and `source_pwd_error` says why
the read gave no value. Neither field is set when the open had no source pane.
A form that a form opens keeps its source's value. A refresh, a reattach and a
hub restart give the same value. A render only reads: values from the opening
moment come from this context, never from a write in a render (decision "view
render never writes; freeze open state in the form context").

Action results can contain `{form:'/forms/…'}` or `{close_form:true}`. Opening a
form preserves the source view. The hub commits the operation result before it
closes the form. A successful navigation from a form closes that form afterward.
Escape and Cancel discard unsubmitted form values. Cancellation waits while an
operation is unresolved. The source view and its draft remain in another pane.

Acceptance: test palette entry, session filtering, form submit, cancel, restart,
stale source context, and preservation of dirty source input in an isolated Rex
server. Test thread menu actions through the renderer and hub. Membership changes
must preserve delivered mail and must not backfill unread mail. New members can read full history through `messaging.history` (default 20, maximum
50 messages per page). Removed members cannot page group history.

Membership verification uses the real Lua plugin with a controlled hub on Rex
`0.0.1792158932-g88fd41`. New messages store their recipient list. Before the first
membership change, the plugin saves delivery lists for older messages in separate
records. It does not rewrite message records or revision cursors. A send checks the
group revision in the same batch as delivery. A membership change checks both the
group and thread revisions. A conflicting send retries with current membership.

Adding a member creates one new notice. It does not make earlier messages unread.
The notice points to `messaging.history`. History pages use stable revision cursors
and return newest messages first. Tests cover removal with unread mail, original
message revisions, operation replay, history pagination, and a send that races a
membership change. Full palette, renderer, and hub integration remains a separate
acceptance check.

## Thread home sessions: plugin candidate (2026-10-17)

The messaging plugin stores the first home session in a `thread-meta/` record.
The record uses the same canonical participant pair or group identity as history.
The creation transaction writes the home and the saved operation result together.
A concurrent first home wins. Replays do not move the thread. Alias migration
preserves the earlier home record.

`New Thread…` supplies the form session. Direct-thread expansion supplies the
form session for the new group. `Threads…` lists the current session's threads,
including archived threads with a Restore action. The plugin uses
`hub.ui.describe.default_session_id` for threads without a home.

Tab declarations include optional `session_id` and a `revision` string. The newest
message ID is the revision; an empty thread uses an empty string. The hub owns
manual-close suppression, reopening on a new revision, and navigation focus.

A tab home must be a Rex session ID: `session:` and a UUID. `hub.ui.tabs` checks only the
`session:` prefix. The reconcile treats any other stored home as gone, without a Rex call: its tabs
use the app session, and the hub logs it and emits `ui_home_invalid` once per hub run. Observed on
Rex `0.0.1792158932-g88fd41`: `session.attach` of `session:kv-home` fails with status 500, code
`internal`, not `session_not_found`. Before this rule, that error failed every reconcile pass, so
no view opened (`TestInvalidHomeGoesToTheAppSession`).

Controlled Lua tests passed on Rex `0.0.1792158932-g88fd41`. Tests cover first-home
preservation, concurrent creation, alias migration, old operation replay, session
filtering, direct expansion, and revision stability across rename and acknowledgment.
Combined hub tests remain pending. This candidate is not deployed. The session
list follows `home_redirects` when the hub recreates a missing home session.

## One-tab migration: renderer and plugins (2026-10-17)

The hub places plugin tabs without an explicit home in the LazyRex session.
App mode is removed: the hub restores nothing (see "Removed: app mode"). The hub
releases an old app session that holds user panes. Existing home sessions,
closed-tab revisions, and drafts must survive migration and restart.

Claude owns hub placement, restoration, and migration tests. Codex owns the
renderer and plugin status changes. The renderer reads only `snapshot.status`.
The hub lifts a plugin's `tree.status` into that field when present. Otherwise,
the hub uses the status from `hub.ui.tabs`. The renderer sends OSC 7501 with
`app=rex`. Each report includes all applicable fields. An absent status clears
the record. OSC 2 continues to use the view title.

Messages declares `done` with an unread count for inbox and thread tabs. Open
forms report `blocked` with `kind=question`. Worktrees reports `working` during
its existing scan and `error` when refresh fails. The home removes app-mode
controls. This phase does not add progress or a separate status title.

Verification: the renderer Go suite and the controlled messaging plugin suite
pass. Combined hub migration and real renderer status verification remain pending.

### Session choice and thread moves

New Thread lists open sessions and defaults to the form's source session.
`New session…` requires a name. The agent action accepts `session_id` or
`new_session`. Without either field, it uses the source session from the hub's
caller identity. Reopening an existing direct thread preserves its home.

The thread menu opens in a side pane. `Move to session…` changes the stored home
and navigates to the thread. The hub retires the old renderer through its draft
save gate and opens the replacement. Rex cannot move panes between sessions.
A move preserves the message revision, history, and saved draft.

The plugin records session intent in the operation fingerprint. A replay keeps
the original result when the caller moves to another session. The hub's
`hub.session.ensure` uses the scoped operation key for new sessions. An old move
replay does not undo a later move. The plugin refuses a closed target session.

Controlled Lua tests pass for defaults, explicit and new sessions, replays,
menu forms, and draft and revision preservation. The combined hub migration
and source-session checks remain pending. Native scrollback for the message
screen is a separate renderer phase.

### Profiles directory helper

`plugins/profiles_directories.lua` supplies read-only helpers for the Start form.
Load it by absolute path. `source_directory(context, profile_cwd)` returns a local
OSC 7 directory, the profile directory, or nil when the hub must use its default.
RPC failures stay errors. `list_directory(context, path, prefix)` uses the original
source pane and returns Rex's status, home, directory entries, and truncation flag.
It requests at most 200 entries per prefix. The browser must let the user narrow
the prefix when the result is truncated. No plugin uses `list_directory` since the Start form
lost its in-form browser (2026-10-18); only its lab test calls it. `worktrees(path)` returns the current
root and Git's worktrees. Bare repository entries are not selectable.

Observed on Rex `0.0.1792158932-g88fd41`: all 12 checks in
`tests/profiles-directories-lab.mjs` pass. These cover absolute helper loading,
foreign-session directory reads, URI decoding, source errors, unusual directory
names, prefix narrowing, worktree discovery, and inherited Git variables.
This verifies the helper. The combined Start form is tested below.

### Start Agent… and Start Accessory…

`plugins/profiles.lua` declares two palette commands. Each opens a form in the tab's side slot
(`/forms/start-agent`, `/forms/start-accessory`). The form lists the profiles of its kind.

- **Directory.** The default is the source pane's `terminal.pwd`, frozen at the open
  (`source_pwd` in the form context). Without one, each profile uses its own directory. In a Git repository with more than
  one worktree, the form lists the worktrees. The form has no folder list of its own (removed
  2026-10-18, the maintainer: the native browser replaces it).
- **Directory field.** The form has a text field `dir` with `browse = "native"`: the renderer
  offers the app's directory browser (`pane.insert_path` through `hub.view.app_action`), which types
  the chosen path into the field. The plugin fills the field with the draft's raw path, escaped once
  (each `\` doubled). Submit decodes the field text as data, with no shell and no trim. A `\`
  escapes the next character; a fully single-quoted value gives its inner text; a leading `~` or
  `~/` is `$HOME`; trailing `/` is removed. The result must be absolute. An empty field means the
  directory that the form showed, so a cancelled pick changes nothing. The profile's own directory
  is chosen with "Use each profile's own directory". When the app refuses Browse… (`no_app`,
  `ambiguous_app`), the user types the path.
- **Explicit directory choice.** A worktree and the profile default are explicit choices. Each one increments the draft's `dir_rev`, which the plugin sends as the field's
  integer `rev`. When `rev` changes, the renderer replaces the editor text, also after an edit, so
  the field shows and submits the choice. An ordinary render keeps `rev`. The choice also replaces
  the directory in the values that a failed start saved, and keeps the other saved values. Observed
  on Rex 1203: Rex Lua is 5.1 and encodes the count as a JSON integer.
- **Draft.** The directory choice is a draft keyed by `view_id`
  (`draft/<view_id>`). Each click writes the draft with an operation record, so a replayed action
  changes nothing again. The hub renders the form again after each action, and the renderer keeps
  the values that the user typed (lab). The first render records the default directory, so the
  start uses the directory that the form showed. Drafts are deleted at a successful start and
  expire after a day. A draft from before the removal can hold `browse` and `prefix`; the plugin
  ignores them.
- **Start.** Fields: profile, the profiles' args, tab title, and placement (new tab in the source
  session, or a split right of the source pane). One field per arg name; it starts empty, and an
  empty field is not sent, so the chosen profile's own default applies. The start sends only the
  args that the chosen profile declares. The plugin saves the exact `hub.spawn` request under the
  operation key before it calls the hub, so a replay sends the same request. The request has
  `placement.focus = {source_window_id}`: the hub focuses the new pane only while the form's tab is
  still active.
- **Pending start.** A `pending` result completes the view operation and records the start in the
  draft. An operation that stays unknown would hold every later operation of the plugin in the
  hub's queue (`runOps`), and an accessory with a lost answer stays pending. The form then hides
  Start and shows "Check again", which sends the saved request with the same key. Open closes the
  form. Failed shows the error and brings Start back with the submitted values (the start saves them
  with its request; the renderer drops the fields while the pending view shows). Cancel closes the form; the spawn stays
  pending in the hub. The pending view renders before the profiles load, so it works when the
  profiles file broke since.
- A broken profiles file shows its error in the form. Before, the render failed as `unavailable`
  and the hub retried it, so the form showed Loading.

Observed on Rex `0.0.1792158932-g88fd41`: `tests/profiles-lab.mjs` passes (agent in a chosen
worktree in a new tab with its task, typed text kept across a directory choice, accessory from the
folder browser in a split, focus, draft removal, broken profiles file, Ctrl+Q), and
`tests/profiles-plugin.mjs` passes (pending, replay, Check again, failure, shared args).

## Tunnels

### Pair contract (2026-10-17)

Status: proposed lifecycle contract. Port discovery passed its isolated lab test on
Rex `0.0.1792158932-g88fd41`. Full plugin and panel verification is pending.

The Tunnels plugin owns discovery, tunnel intents, and forward records. Rex owns
the tunnel processes and their session lifetime. The hub provides its existing
plugin storage and panel services. No new hub port service is needed.

`plugins/tunnels_ports.lua` returns a module with `scan(current_session)`. It
returns `{ports, warnings}`. Each port has `session_id`, `window_id`, `block_id`,
`tab`, `pid`, `command`, `port`, and `address`. The current session sorts first.
Detached panes have empty `window_id` and `tab`. Tunnel blocks and LazyRex hub,
plugin, renderer, form, and placeholder panes are excluded by their known labels.
This filter affects choices only; labels grant no lifecycle authority.
The command is a process name,
not its arguments. IPv4 and IPv6 listeners remain separate. Rex encodes empty
Lua lists as objects; consumers must accept both. Display scan warnings beside
the result. A failed scan is not proof that no ports exist.

The scan lists terminal child PIDs, reads process parents with `ps`, and selects
only those roots and descendants for `lsof -nP -a -p ... -iTCP -sTCP:LISTEN`.
It uses argv commands and parses lsof fields. It never runs lsof with an empty
PID selection. It runs on explicit form discovery, not in a background poll.
This is a best-effort snapshot: a process or port can change after discovery.
It does not discover Docker processes outside the pane ancestry or prove port
ownership at submit time. Manual entry remains available.

Lifecycle invariants:

- Save a separate intent under the submit operation key before `session.new_block`.
  Save its arguments or fingerprint, source session, source block, port, profile,
  and phase. Reusing a key with different arguments is a conflict.
- Mark the intent as sending before the create. Send the create once. A timeout,
  disconnect, or restart with a sending intent leaves its outcome unknown. An
  empty block listing does not permit a second create. A label is only a hint;
  it cannot authorize adopting or closing an unrelated block.
- Save the returned block ID and a forward record keyed by that ID. Preserve the
  source session and source block separately. Commit the saved operation result
  with the forward record. A repeat returns the recorded result.
- Reconcile records at start and on Rex lifecycle events. A successful block
  listing that omits a known block, or a successful session listing that omits
  its session, establishes a stopped tunnel. A failed query leaves state unknown.
  A kept-open block needs its child process and last exit inspected; existence
  alone does not mean running. Keep the last URL as history, not as evidence of
  a live connection.
- Show Logs places the known tunnel block beside its original source block in
  the same session. Refuse if the source block is gone or detached. Hide Logs
  detaches the tunnel. Stop closes only the recorded tunnel block. Closing the
  session ends its tunnel. Neither Stop nor recovery creates a replacement.
- A lost reply to Show, Hide, or Stop requires a layout or block-state check
  before retry. A restart preserves records and never spawns a second tunnel.
- The plugin panel shows starting, running, stopped, failed, and unknown outcomes
  clearly. Creation and connection readiness are distinct. A printed URL alone
  does not prove the process is still running.

Verification: `tests/tunnels-ports-lab.mjs` uses two lab sessions and local TCP
listeners. It checks descendant attribution, session order, exclusion of a
listener outside the pane trees, detached panes, and closed panes.
`tests/tunnels-ports-unit.lua` checks parsers and failed scans through fake APIs.
The partner owns full form, panel, log placement, stop, session-close, and restart
tests. Both agents review the combined result before completion.

Public tunnel provisioning, installation, and internet tests require coordinator
direction and the maintainer's approval. Until then, use the local stand-in. Do not deploy
from either pair worktree.

### Panel renderer and plugin checks

The panel renderer freezes input before it answers `view.release`.
Unsaved input shows **Keep editing** and **Discard and replace**.
An unresolved mutation or draft conflict disables discard.
A completed source action releases automatically only when all input is safe.
Input typed after submission still needs confirmation.
A matching `view.resume` restores input. Attachment recovery reads the durable replacement state.

A view that no renderer ever attached holds no input. A replacement claims it without
`view.release` (`views.attach_state`: never, attached, claimed, unknown).
- The claim and the move to opening are one transaction. Only one replacement can claim a view.
- A renderer that attaches after the claim gets the replacement in its attach reply and freezes.
  It then reads `hub.view.replacement` once.
- A view that attached before and has no renderer now is a conflict: "try again in a moment".
  A row from an older database is `unknown` and counts as attached before.
- The claim belongs to the view, not to one replacement. A replacement that ends undoes the claim
  only when no replacement of that view is still active. While one is active, the claim stays,
  and a new replacement gets the conflict. Hub start undoes the claims that no active
  replacement holds.
Esc and Ctrl+Q close a panel through `hub.view.close`.

Threads and Worktrees declare landing panels. Home returns a declared panel effect.
The Threads panel lists all threads. It puts current-session threads first within each list.
Conversation tabs remain declared.

Observed on Rex 0.0.1792158932-g88fd41 in an isolated lab:
`tests/messaging.mjs` passes with the global panel and session-order checks.
The home panel checks (panel effects, retained tab navigation) are now `plugins/lazyrex/tests/navigation.lua` and `render.lua`.
Renderer tests pass with the Go race detector, including the release reply order,
attachment checks, replacement recovery, and input typed after submission.
These checks do not establish combined hub replacement or production deployment.

Direct-thread tabs now require a message or an explicit user open.
New Thread and the thread list record durable open intent.
Identity migration preserves that intent. Archive still hides the tab.
An explicit navigation can call the plugin's `view.open` once to declare a missing tab.
Restore and reconciliation do not call `view.open`.
The direct-thread checks pass in the controlled Lua test on Rex 0.0.1792158932-g88fd41.
The hub unit checks cover declaration, a lost answer, unsupported hooks, and missing tabs.

The user's Threads panel also lists agent-only threads in a separate section.
Agents still list and read only their own threads.
Observed groups and direct threads get tabs only after an explicit user open.
Observed tabs have no unread status or message token that can reopen a closed tab.
Agent-to-agent direct threads use a stable `d-` ID derived from the stored participant pair.
The user can read those direct threads but cannot save a draft or send into them.

The user's first send to an observed group adds the user and writes the message in one transaction.
A failed send does not add the user. A retry does not send twice.
Old messages keep their delivery lists and do not become unread for the user.
Older messages expands a thread's visible history once per action key.
It uses the existing history control; native scrollback remains separate.

The controlled Lua suite passes on Rex 0.0.1792158932-g88fd41 for observer lists,
read-only directs, agent access, group joins, revision conflicts, legacy delivery,
unread isolation, and older-history retries. Combined hub and renderer checks remain separate.

## Session templates (2026-10-17)

Status: lab (Templates pair, Rex 0.0.1792158932-g88fd41). Not deployed. Decision: "Session templates".

- Plugin `templates` (`plugins/templates.lua`). Palette commands: "Start Session…" (`/forms/start`), "Save as Template…" (`/forms/save`), "Templates…" (panel `/panels/templates`, the landing). The engine `plugins/templates_run.lua` must sit beside it.
- Hub actions: `hub.spawn_window {key, session?, title?, layout}` starts one tab of profiles with one Rex request; `hub.spawn.spec {block_id}` gives the launch spec of a spawned pane. Contract: the header of `hub/cmd/hub/spawnwindow.go`. The spec has `title` (the resolved title, a default when the caller gave none), `requested_title` (the title as the caller gave it; absent when no explicit title is known: none was given, or an old row cannot tell; `title_source` tells which) and `title_source` (`request` when the saved request has a title string, also an empty one; `unknown` for an old or malformed request, which cannot tell). A pane rename after the spawn changes neither.
- Hub config: add `{"source": "<the Templates directory>"}` to `plugins` (its manifest must set `"events": "all"`; on 2026-10-20 the Templates manifest has no `events`). The profiles file needs no change; `shell` is built in.

Lab test (observed, real hub and renderer):

```sh
spikes/lab/server.sh /tmp/rexlab-templates-<name>
# run the hub on that server with plugins lazyrex and templates, state in $LAB/hubstate,
# and a profiles file with "server" (arg port, default 3000) and "watcher"
TEMPLATES_LAB=/tmp/rexlab-templates-<name> node spikes/03-ui-framework/tests/templates-lab.mjs
SPAWN_WINDOW_LAB_SOCKET=/tmp/rexlab-templates-<name>/rex.sock go test ./cmd/hub -run TestSpawnWindow   # from hub/
```

Passed: save (an undecided pane is refused; Leave out removes the pane and its split; a profile choice), start from a pane in another repository into a new session (exact ratios, directories under the new root, the profile's default arg), the panel. `TEMPLATES_RESTART=1` kills the hub after a start is admitted: the run stays pending, as decided (see "Not in v1" in the decision).

Limits: the lab pane is short, so the test reads lower form rows only after a focus change scrolls them in. Floating panes are not captured.

### App switch (lab, 2026-10-19)

A start into a new session switches the app to it (decision: "App switch" in "Session templates"). Hub action: `hub.app.select_session {view_id, session_id, window_id}`, for a plugin's own open form (`hub/cmd/hub/appaction.go`). Plugin: `switch_app` in `plugins/templates.lua`.

Tests (observed, Rex 0.0.1792158932-g88fd41, lab):

- `go test ./cmd/hub -run 'AppAction|AppSelect'`: one app, no app, two apps, not focused, an existing-session target, a window of another session, a closed form, another plugin's form, a caller that is not a plugin.
- `tests/templates-plugin.mjs` F1–F6 (the real plugin, a controlled hub): one switch and no switch on replay; none for an existing session; a refusal gives the hint and is not asked again; an earlier `attempted` reads as unknown with no call; the wait times out and a late first tab does not switch; the waiting form shows "Starting…".
- `tests/templates-lab.mjs` 2, 5 and 6 (real hub and renderer, a fake app client): the app gets one `session.select` with the new session and its first tab; none for a start into the current session; two apps give `ambiguous_app`, no call, and the hint in the panel. Two full runs with `TEMPLATES_RESTART=1` passed.

Not tested: the real app. A lab has no app, so what `session.select` does on screen is unknown until the maintainer approves a check.

### Remote Control (observed)

The Rex app refuses client actions until the user turns on Remote Control (Rex app: Settings, Rex Server). The hub never asks the app to find out (the user, 2026-10-22: no probe). It records the result of the app actions that it runs anyway (`hub/cmd/hub/remotecontrol.go`):

- The calls: `client.perform` to the app from `hub.view.app_action` (`pane.insert_path`), `hub.app.select_session` and the navigation step (`session.select` on each app). The `session.view`, `client.list` and `client.inspect` calls around them record nothing.
- The rule: a result without error is `on`. A `refused` error whose message starts with `Rex.app is not accepting actions` is `off`. Any other result (a timeout, another refusal) keeps the earlier observation and its time. The refusal form is observed by the CLI on Rex 1203 ("Driving the Rex app" in rex-protocol.md); the form that the hub gets is untested until an ordinary app action shows it on a real app.
- Memory only, for each app client ID: a hub restart forgets it. A client that Rex keeps while it reconnects (the same ID) keeps its observation and its time; an observation never moves to another ID. A read deletes the observations of clients that `client.list` no longer names, and only those that it saw before its `client.list` call.

`hub.app.remote_control` (no arguments; any client; a read) returns `{"apps": [{"client_id", "label", "state", "action"?, "at"?}]}`: every connected app of `client.list` (`info.kind` `app`). `label` is the client's label, else its name, else empty. `state` is `on` or `off` (the last observed result, with `action` and `at`, RFC 3339 UTC) or `unknown` (no retained observation; no `action`, no `at`). An empty list means no connected app. A failed `client.list` is an error, never an empty list. Hub Status shows it ("Stop and status").

Setup prints one paragraph about Remote Control: what needs it and where to turn it on. It does not ask the app either.

Tests: `TestRemoteControlState` (the rule, also with the error wrapped as `rex.Call` wraps it), `TestRemoteControlRecordSites` (only the `client.perform` result counts; a timeout keeps the observation), `TestRemoteControlRead` (connected apps, unobserved apps, a reconnecting app, an absent client, the same ID back, no app, a failed list), `TestRemoteControlReadKeepsARecordDuringTheCall`. Lab: `tests/harness/tests/remote_control.lua` (a fake app answers the refusal text: a form action and a navigation each give `off`, then `on`, and Hub Status shows the lines). The fake app proves the transport and the recording, not the real app's setting.

## File watches (hub.watch)

Design approved by the hub features reviewer, 2026-10-20 (design v3 with amendment 1). The user
decision of 2026-10-20 approves fsnotify (`github.com/fsnotify/fsnotify`). Rex has no file-watch
API, and plugins are Lua inside Rex, so the hub owns the watch. The first user is the Automations
plugin: a new file in the vault inbox starts the knowledge worker, or messages it if it runs.

Status: built. The rules and the feed are in `hub/cmd/hub/watch.go`. The actions, the loop of each
watch and the nudges are in `watchsvc.go`. Tests:

- Unit tests: `watch_test.go` and `watchsvc_test.go`.
- Lab tests (observed on Rex `0.0.1792158932-g88fd41`, 2026-10-20): `lazyrex-hub test tests/watch`.
  `tests/watch/README.md` lists what they cover and what no test covers.

### Model

The hub watches one directory for a plugin. It keeps a feed with one row per file name: the latest
settled state of that name. fsnotify events only start a settled check. A settled change gives the
row a new `seq`, from the database-wide revision counter, so no watch reuses a `seq`. A removed file
stays as a tombstone until the plugin acknowledges it.

The feed reports the latest state of each name, not every change. A plugin must accept collapsed
intermediate changes.

### Actions

The caller is the registered plugin client. The namespace is the plugin name, as for `hub.store`.
Sequence values and cursors are decimal strings.

- `hub.watch.ensure {key, path, glob?, settle_ms?, emit_existing?}` returns
  `{key, watch_id, state, cursor, watermark}`.
  - `key` matches `[a-z0-9_.-]{1,64}`. `path` is absolute and clean; the hub does not expand `~`.
    `glob` matches the base name (`filepath.Match`, default `*`). `settle_ms` is 200 to 60000
    (default 1000).
  - The same key and the same spec return the same `watch_id`. Another spec for the key is refused
    (`watch_conflict`). To change a spec, remove the watch, then ensure it again.
  - `ensure` is fast. It checks the grant and the path, lists the directory once (bounded, see
    "Limits"), and commits the watch in one transaction with state `initializing`. It does not
    wait for any file to settle. If the check fails, it stores nothing and returns the error.
- `hub.watch.read {key, watch_id, after?, limit?}` returns `{rows, next, watermark, more, state}`.
  - `after` omitted: the stored cursor. `after = "0"`: from the start.
  - `limit` default 100, maximum 500. A page also stops at 256 KiB of encoded rows.
  - A `watch_id` that is not the current watch of `key` is refused (`stale_watch`).
- `hub.watch.ack {key, watch_id, through}` returns `{cursor}`. See "Acknowledgement".
- `hub.watch.remove {key, watch_id}` returns `{removed}`. It deletes the watch, its rows and its
  pending names. Another `watch_id` changes nothing (`removed: false`), so a late remove cannot
  delete a replacement. A missing key succeeds (a retry after a lost reply).
- `hub.watch.list {}`: each watch of the caller with `key`, `watch_id`, `path`, `glob`, `state`,
  `last_error`, `cursor` and `watermark`.
- Nudge: the hub calls the plugin's `hub.changed` action with `{what: "watch", key, watch_id}`.

### Rows and kinds

Each row has `name`, `path`, `kind`, `seq`, `event_key`, `appearance_key`, and, for a present
file, `size` and `mtime_ns`. The hub derives `kind` from the row and from the plugin's
acknowledgements (`acked_present`), never from the page cursor:

| Row | Acknowledged as present | kind |
| --- | --- | --- |
| present | no | `created` |
| present | yes | `changed` |
| tombstone | yes | `removed` |
| tombstone | no | `created_removed`: it came and went before an ack covered it |

- When a tombstone becomes present again, the row gets a new `created_seq` and is not acknowledged
  as present. So a recreated file reads as `created`. A replacement that never settles absent (an
  atomic save) reads as `changed`.
- `event_key` is `watch_id:seq`: one delivery of one row state.
- `appearance_key` is `watch_id:created_seq`: one appearance of a file. A plugin uses it only for
  start-once work (Automations starts or messages the knowledge worker once per appearance). Other
  work from a row uses `event_key`. Example: a file is read as `created`, the plugin commits its
  start-once work, the ack is lost, and the file is removed. The next read returns
  `created_removed` with a new `event_key` and the same `appearance_key`. The plugin does not start
  the work again, and it handles the removal.
- Delivery is at least once. A row that changes after a read moves to a higher `seq`, past the
  next ack, and comes again.

### Acknowledgement and paging

The consumer loop:

1. `read {key, watch_id}`.
2. Process each row.
3. `ack {key, watch_id, through: next}`. Use `next`, not `watermark`.
4. If `more` is true, go to step 1.

- `next` is the highest `seq` that the read scanned before the first visible row that it did not
  return. It never passes a visible row that was not returned. Hidden rows (baseline, below) count
  as scanned, so an ack can pass them.
- A row that changes between pages gets a higher `seq` and comes on a later page.
- `ack` with `through` above the watermark is refused (`bad_cursor`) and changes nothing.
  `through` at or below the stored cursor changes nothing and returns the cursor.
- Otherwise, every row with `seq <= through` is recorded as acknowledged in its current state.
  Tombstones with `seq <= through` are deleted. The cursor becomes `through`.

### Initialization

- At `ensure`, the hub stores each eligible name of the listing as a pending initial name.
  Initialization ends when no pending name remains: each one committed, or dropped because it
  settled absent. Then the state is `active`. An empty listing is `active` at once.
- `emit_existing = false` (default): the pending names are the baseline. A baseline file that
  settles present gets a row that is acknowledged and hidden. Its next settled change shows it
  (`changed`, or `removed`). A file in the listing that is still being written is baseline, and
  its later content is not reported.
- `emit_existing = true`: the pending names commit as normal rows and read as `created`.
- A name that is not in the `ensure` listing is a later arrival. It reads as `created`, also
  during initialization.
- `emit_existing` is a creation option. A restart does not replay it. The pending names are
  durable, so a hub restart during initialization resumes it.
- A slow writer keeps its name pending. It does not block `ensure`, reads or other hub requests.

### Settling

One rule applies to every source of a difference: an fsnotify event (Create, Write, Remove,
Rename; Chmod is ignored), the start scan, the 5-minute scan, the scan after a watcher error, and
the initial listing.

1. The first observation is an `Lstat` of the name when the source reports it, or "absent".
2. The name's timer starts, or starts again (`settle_ms`).
3. When the timer expires, the hub makes a new `Lstat`. If it differs from the last observation,
   the timer starts again.
4. If it is equal, the name settled. The hub compares it with the row and commits a change only
   if they differ.

- The hub uses `Lstat` only, never follows a symlink, and never opens a file. Only regular files
  are eligible. Names that start with `.` are not tracked.
- A settled observation that is absent or not eligible becomes a tombstone if the name has a row.
  With no row, it changes nothing.
- A scan commits nothing directly. Each name that differs from its row, and each row whose name is
  missing from the listing, becomes a first observation.
- A settle timer commits only while the watch is `initializing` or `active`. When a watch leaves
  those states, its timers are cancelled. One goroutine owns the watcher and every timer of a
  watch. Closing the watch forgets every pending name, so a timer from an older open finds
  nothing to commit. This replaces the epoch counter of the design.
- Before a commit, the hub checks the watched directory's `(dev, ino)` against the open. A
  directory renamed away makes no tombstones.

Effects: an atomic save (a temp file renamed over `x`) gives one `created` or `changed` for `x`.
The temp file is hidden or comes and goes inside the settle time. Many writes give one row
change. Duplicate events and Spotlight changes give nothing when the stat is unchanged. A writer
that pauses longer than `settle_ms` gives `created`, then `changed`.

### States and errors

| State | Meaning | Retry |
| --- | --- | --- |
| `initializing` | pending initial names remain | none needed |
| `active` | watching | none |
| `missing` | the directory is gone (`ENOENT`, `ENOTDIR`); no tombstones | every 30 s |
| `error` | a listing error (`EACCES`, `EIO`, a partial listing), a backend error, `EMFILE`; no commits, rows stay | every 30 s |
| `too_large` | over an entry limit | each 5-minute scan; reopen at 90% of the limit or less |
| `not_granted` | the resolved path is outside the plugin's `watch_roots` | at hub start |
| `backlog_full` | 20000 rows; no commits | the next scan after an ack |
| `inactive` | the config omits the plugin; watches stay stored | at hub start |

- An `Lstat` error for one name, other than "not found", skips that name in this pass. Its row
  stays.
- Each watch has its own `fsnotify.Watcher` (its own kqueue). A backend failure closes only that
  watch.
- When a watch becomes `active` again, a settled scan compares the directory with the rows.

### Grants and paths

- The hub config entry of a plugin lists `watch_roots`. A watch path must resolve to a root or to
  a directory inside one. The manifest cannot grant this. Config changes take effect at hub
  start.
- On `ensure` and on every open and scan, `EvalSymlinks(path)` must equal the stored resolved path
  and be inside a resolved root. Otherwise the state is `not_granted`. This catches a watched
  directory that was replaced with a symlink to a path outside the root.
- Before and after `fsnotify.Add`, the hub compares the directory's `(dev, ino)`. A difference
  closes the watch, and the hub retries after 30 s.
- This enforces the user's declared intent. It is not an OS security boundary: plugins run as
  the same user. The kqueue backend opens a symlink entry's target for events (`O_EVTONLY`). The
  hub reads no data from it and commits nothing for it.

### Limits

- No recursion: one directory per watch.
- The kqueue backend (fsnotify v1.10.1, macOS) opens one descriptor for every directory entry
  except sockets and named pipes. This includes ignored names, subdirectories and symlinks. The
  budget counts all of them, the directory, and 3 descriptors
  that each watcher opens for itself (a kqueue and a close pipe with two ends).
  - Per watch: 1000 counted entries.
  - All watches together: 4000 descriptors, and at most half of the soft `RLIMIT_NOFILE`.
  - 8 watches per plugin.
- The `ensure` check and every scan read the directory in chunks of 256. They stop as soon as the
  number of listed entries passes 1000. This limit counts every entry, sockets and named pipes
  too: they need no descriptor, but they still cost work in the hub. A directory over it is
  `too_large`.
- The count after each Create event and each scan detects growth. It is not an instantaneous
  ceiling: the backend opens descriptors for new entries before the hub handles the event.
- Local file systems only. fsnotify does not support NFS or SMB.
- Not recoverable:
  - A change made while the hub is down, or while a watch is not active, and undone before the
    next scan.
  - A file created and removed in that time.
  - A file removed and created again with the same name in that time reads as `changed`, because
    its inode differs.

### Nudges

- After a commit, the hub sends `hub.changed` in a goroutine, outside action handlers, `h.mu` and
  database transactions.
- While the cursor is below the watermark, the hub repeats the nudge after 5 s, after 30 s, then
  every 5 minutes. It stops when an ack reaches the watermark or the plugin is not registered.
  `hub.register` starts the schedule again.
- A plugin also reads at start and on its own `rex.every(300)`.

### Plugin lifecycle

- Watches belong to the plugin name, not to an instance. A plugin restart keeps them. The hub
  nudges after `hub.register` when rows are waiting.
- A stopped plugin's watches stay active and collect rows up to the row limit.
- A plugin that the config omits: its watches become `inactive` and stay stored. When it comes
  back, a scan compares the directory with the rows. `hub.watch.remove`, or the plugin manager's
  remove, deletes them.

### Timers

A plugin can use `rex.every` and `rex.after` (rex-protocol.md, "Plugin timers"). A daily job uses
a short tick and a stored `last_run`, not a hub schedule.

## Digests (hub.digest)

`hub.digest {texts}` gives the SHA-256 of each text, for a plugin that cannot hash in Lua. A Lua
plugin cannot send a body over 64 KiB in one request. So it digests each chunk, then the joined
chunk digests (at most 7 x 64 bytes). One rule covers every body; a small body is one chunk.
A chunk boundary must not split a UTF-8 sequence. Lua `string.sub` counts bytes, so a cut inside
a multibyte character makes two chunks that are not valid UTF-8, and Rex replaces their bytes
(below): the digest no longer matches the body. Move the cut back to the start of the character
(a byte that is not `10xxxxxx`).

- Caller: the registered current instance of a configured plugin. Other callers get code
  `forbidden`. No state, no grant.
- Input: `texts`, a list of 1 to 16 strings. An empty string is a valid text. A missing field, an
  empty list, more than 16 items, an item that is not a string, or another field: code
  `invalid_input`.
- Limits: each text at most 65536 bytes, all texts at most 262144 bytes (UTF-8). Over a limit:
  code `too_large`, for example `texts[3] is 70000 bytes, over the 65536-byte limit` or `the
  texts are 300000 bytes, over the 262144-byte limit`.
- Output: `{digests}`, in the same order. Each digest is the lowercase hex SHA-256 of the text's
  UTF-8 bytes (64 characters).
- Bytes that are not UTF-8 (observed on Rex 0.0.1792158932-g88fd41, `TestDigestLab`): Rex
  replaces each such byte with U+FFFD before the hub gets the text. The hub digests the replaced
  text: `a\255b` from Lua gives the digest of `a` U+FFFD `b`. A plugin that digests a body before
  it sends it gets the same value only for valid UTF-8 (rex-protocol.md, "Client RPC").

## Agent prompts and acting as the user (Telegram hub support)

Design approved by the hub features reviewer, 2026-10-21 (design v2 with amendments 1-3, and the
retention rule of H1d). The first user is the Telegram plugin: it shows an agent's open prompt on
the user's phone, sends the user's answer to the agent's pane, and reads and sends the user's
messages.

Status: part 1 (prompts and answers) is in progress. Part 2 (acting as the user) waits for the
Messaging change.

### Grants

Two fields of a plugin entry in the user's config grant this access. Both are booleans, and both
are false when absent.

- `prompts`: the plugin gets the prompt events of every agent, reads `hub.prompts` and calls
  `hub.prompt.answer`. A prompt can show secrets from the pane, so other plugins get nothing.
- `user_messaging`: the plugin calls `hub.user.call`, and Messaging acts for the user. At most one
  entry sets it. Two entries are a malformed config: `entries 2 and 4 both set user_messaging`.

Only the user's config sets them (user decision, 2026-10-19). A manifest that names them is
refused by the unknown-field rule, and so is a managed entry (plugin-sources.md, section 13). An
agent can change plugin settings through the plugin manager; it must not be able to give a plugin
the user's identity or the pane prompts. The grant belongs to the entry: the hub checks it for the
current registered instance of that entry (the caller's `client_id` is the plugin's registered
client), not for a name in a request. The grant also needs a config entry: a kept record (its entry
was removed, for example by a reload) has no grant, also while its old instance runs. A grant
added to an entry applies when the instance starts again (the instance's run carries it).

### Prompts

An open prompt is an agent's configured program status record (the same record that notices use)
in state `blocked`, for a live binding. The hub keeps prompts in memory (`hub/cmd/hub/prompts.go`).

- A prompt opens when the record becomes `blocked`. A blocked record without `kind` and `msg`
  (Codex 0.163.0) is a prompt too.
- A blocked report with the same `kind`, `msg`, `app` and `effective_app` keeps the prompt. A
  change of any of them closes it (reason `replaced`) and opens a new one. The hub compares the full
  `msg` (by its SHA-256), so a change after the 8 KiB cut also replaces the prompt.
- A prompt closes when the state is not `blocked` (reason `state`), when the record is removed or
  its state is unknown (`removed`), or when the binding changes or the process exits (`gone`).
- A pane has at most one open prompt: the prompt of its current owner. The owner is the current
  identity of the pane, or the legacy agent of the pane when no identity took it over. A legacy
  agent with an alias never has a prompt again: at the first identity report, its open prompt
  closes (reason `alias`), and the identity opens the same prompt with a new `prompt_id` and a
  higher `rev`. A superseded or detached identity has none: after a `/clear`, the old
  conversation's prompt closes (`gone`) and the new one opens. Each close comes before the open,
  in one pass, so a pane never has two prompts.
- Each prompt has a `prompt_id` (64 random bits, hex) and a `generation` (a counter per agent).
- A hub restart closes every prompt: ids and generations are not stored.
- `msg` is cut to 8 KiB on a UTF-8 boundary, with `msg_truncated: true`.
- `rev` increases at each open and close. It is the order of prompt changes within one
  `hub_generation` (the hub run generation that `hub.register` returns).

**Limit.** The hub sees only status reports. Two different prompts with the same `kind`, `msg` and
`app`, with no report of another state between them, get one `prompt_id`. For Codex, which sends
no `kind` and no `msg`, this happens for two approvals in a row without a state report between
them. The hub cannot detect it.

**`kind` does not tell how the agent reads input** (rex-protocol.md, "Blocked records of real
agents"). The hub never chooses the input form from `kind`.

`hub.prompts` (plugins with the `prompts` grant) returns `{hub_generation, rev, prompts}`. Each
prompt has `prompt_id`, `generation`, `agent`, `session_id`, `block_id`, `kind`, `msg`,
`msg_truncated`, `app` and `opened_at`. The hub reads the list and `rev` under one lock, so `rev`
describes exactly that list. `hub.agents` gives `block_id` for each live agent.

### Prompt pushes

The hub calls the action `hub.prompt` on each registered plugin with the `prompts` grant:

```json
{"hub_generation": "7", "rev": "12", "agent": "alice",
 "prompt": {"prompt_id": "…", "generation": 3, "agent": "alice", "session_id": "…", "block_id": "…",
            "kind": "permission", "msg": "…", "msg_truncated": false, "app": "claude-code", "opened_at": "…"},
 "closed": {"prompt_id": "…", "reason": "replaced"}}
```

A push gives the **current** state of one agent. `prompt: null` means that the agent has no open
prompt. `closed` names the agent's last closed prompt, for the plugin's text only. `rev` is the rev
of the agent's last change.

Delivery (`hub/cmd/hub/promptpush.go`):

- Each registration of the plugin (instance and client) has one sender with a set of dirty agents.
  A prompt change marks the agent dirty for every sender.
- The sender sends one agent at a time, with the state current at the send. It has at most one call
  outstanding (5 s, with `timeout_ms` 3000).
- A failed call, a timeout or a lost reply leaves the agent dirty. The next call sends the state
  current then, after 1 s, 5 s, then every 30 s. A change during the wait only marks its agent dirty.
- So a lost reply never hides a close, and the hub keeps no event history. A plugin that never
  answers costs one goroutine and one call at a time.
- A new registration starts with every agent dirty that had a prompt in this hub run. The sender of
  the old registration ends after its outstanding call, and its result changes nothing.

**The plugin's apply rule.** The plugin keeps `applied[agent]` (the rev of the last state it
applied) and one `floor` (the rev of the last snapshot it applied). The effective rev of an agent
is `max(applied[agent], floor)`.

1. A push with rev `r` for agent `a` applies only if `r` is higher than the effective rev of `a`.
   Then `applied[a] = r`.
2. A snapshot (`hub.prompts`) with rev `R`: for each agent in the list or in local state, if its
   effective rev is lower than `R`, take its state from the snapshot (absence means no open prompt)
   and set `applied[a] = R`. Then `floor = max(floor, R)`.
3. The plugin takes `hub_generation` only from its own accepted `hub.register` reply
   (`generation`). A new generation drops every saved prompt, `applied` and `floor`. A push or a
   snapshot with another `hub_generation` is ignored. A message never changes the generation.

So a push and a snapshot can arrive in any order: the older one is ignored.

### Prompt answers

`hub.prompt.answer {prompt_id, input, key}` (plugins with the `prompts` grant) writes the user's
answer to the pane of an open prompt (`hub/cmd/hub/promptanswer.go`). `input` is exactly one of:

- `{keys: "<text>"}`: one terminal write of these bytes, with no Enter. 1 to 64 bytes. For a menu.
- `{text: "<text>", submit: true}`: a bracketed paste of the text (newlines become spaces, as in
  notices), then Enter (`\r`). 1 to 4096 bytes.

The plugin chooses the form. The hub never infers it from `kind`.

Each answer has a durable row in `prompt_answers`, keyed by `(plugin, key)`, with a fingerprint
(SHA-256 of `{prompt_id, input}`). The steps:

1. **Key check.** A row with the same key and fingerprint returns its recorded `{phase, result}`
   and writes nothing. A row still writing returns `{phase: "in_progress"}`, which is not final:
   repeat later. The same key with another fingerprint is `operation_conflict`. The key check comes
   first, so after a hub restart a repeat with the old `prompt_id` returns the recorded result
   (H1d), not `prompt_closed`.
2. **Binding check.** The prompt must be the open prompt of its agent, for the same binding.
   Otherwise the row is `refused` with `{reason: "prompt_closed"}`, and no byte is written.
3. The row is inserted in phase `writing` before any byte. Answers for one agent run one at a
   time.
4. The binding is checked again right before the write.
5. `keys`: one write. `text`: the paste, then phase `text_written`, then the binding is checked
   again (the paste can change the agent's state), then Enter.

Results:

| Phase | Meaning |
|---|---|
| `done` | Rex answered every write with every byte written. |
| `refused` | The prompt is not open. This call wrote zero bytes. |
| `failed` | Rex refused the first write with `not_found` or `session_not_found`. Zero bytes. |
| `partial` | The text was written; Enter was not sent (`enter: "not_sent"`, the prompt closed) or Rex refused it (`enter: "failed"`, with `code`). |
| `unknown` | The first write had another error, a timeout, a lost reply or a short `bytes_written`. Some bytes may be written. |
| `unknown_after_text` | The text was written; the Enter write's outcome is unknown. |

Only `not_found` and `session_not_found` count as proof of zero bytes (lab, Rex
0.0.1792158932-g88fd41: a write to a missing block or session wrote nothing). `bad_request` is
`unknown`: the lab showed it only for a missing `data` field, and the hub always sends `data`.

The hub never repeats a write by itself. A hub start turns a row still `writing` into `unknown` and
a row in `text_written` into `unknown_after_text`.

**`prompt_closed` means "this call wrote zero bytes".** While the key's row exists, a repeat
returns the recorded result. After the row is deleted, `prompt_closed` does not prove that an
earlier call with the same key wrote nothing.

**Retention.** A row stays while its prompt is open, then 7 days after the close. `closed_at` is
set by an hourly sweep that sees the prompt closed (so it can be up to one hour late; retention
only grows). The sweep closes every row whose prompt is not open, also a row inserted just after
its prompt closed. A prompt id never opens again, so a closed prompt stays closed. A refused row is closed at once. A hub start closes every row of an earlier run (a
restart closes every prompt). The Telegram plugin repeats a key at most 6 days after its intake, so
its repeats always find the row.

**Race.** The hub checks the binding against the last status event it processed. It cannot prove
that the agent is still at the prompt at the write: a change can be on its way. Rex has no atomic
write-if-state.

**The request queue.** Rex delivers requests to the hub one at a time. The writes are Rex server
calls (each with a 5 s limit), so an answer holds the hub's request queue for at most the time of
its writes, as `hub.spawn`'s create does. No answer waits for a plugin.
