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

Launch: `lazyrex-view -view-id <id> <plugin> <route>`.
The hub passes the view ID as an argument. The pane supplies `REX_SERVER`.
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
The hub renders every open view of a plugin after a store commit in its namespace.
A plugin calls `hub.view.invalidate {routes}` for changes outside storage.

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

**Profiles file.** The config key `profiles` names a JSON file `{"profiles": [...]}`. Each profile has `id` (`^[a-z][a-z0-9_]{0,31}$`), `name`, `kind` (`agent` or `accessory`), `command` (argv), optional absolute `cwd` and optional `env`. Env names that start with `LAZYREX_` are refused. Optional `args` declares named values, for example `{"name": "task", "label": "Task"}` with `"command": ["claude", "${task}"]`. A spawn passes `args: {"task": "..."}`. Each `${name}` must be a whole command argument (not the program), and the hub replaces it with the value as one argument: no shell runs, so any text is safe. An arg with no value and no default is left out, so plain `claude` runs. A `required` arg must have a value; an undeclared arg is refused. Each declared arg must appear in the command. An unknown field or an invalid profile makes the whole file invalid, so a typo never removes a profile silently. The hub reads the file at each request and tells the plugins when it changes. Profiles are global now. Later, spawn point files come after the global file, and a profile in a later file replaces the profile with the same `id` (`loadFrom` already applies this order).

**Built-in shell.** The profile `shell` (kind `accessory`) runs the login shell (`$SHELL -l`, or `/bin/zsh` when `$SHELL` is not absolute). It is first in `hub.profiles`. A profile file can replace it with its own `shell` profile. `hub.profiles` entries also carry `cwd` (empty: the home directory).

**Environment.** Every pane, also an accessory, starts through `/usr/bin/env -u LAZYREX_AGENT -u <agent variables>` (`agentEnvUnset`): the user can start an agent in a shell, and it must not inherit another agent's identity. Only an agent spawn sets `LAZYREX_AGENT` again. Lab: `tests/spawn-env-lab.mjs` (Rex 1203).

**Agent profiles.** An agent profile also creates a hub agent with a new ID `<profile>-<n>` and name `<Name> <n>`. IDs are never reused. The pane gets `LAZYREX_AGENT=<id>` through `/usr/bin/env`, because Rex has no env option on a spawn. The MCP shim uses `LAZYREX_AGENT` before its `-agent` flag, so a shared MCP config (`-agent claude`) still joins as the new ID. Messaging adds a thread tab for the new agent without a restart.

**Hub tools** (also MCP tools for agents): `hub.profiles`, `hub.spawn {profile, key, title?, args?, placement?}`, `hub.workers`, `hub.stop {spawn_id, key?}`. The home plugin acts for the user with `for: "user"`; only a registered plugin may do that.

**Placement.** Omit `placement` to open a new tab in the work session. Set `placement.session` to another session ID to use that session. The hub attaches the session before admission. Managed LazyRex app sessions are refused by their recorded ownership, even if their labels change.

- New tab: `placement: {session: "session:..."}`.
- Split: `placement: {session: "session:...", beside: "block:...", direction: "right", ratio: 0.3}`. The anchor must be placed in that session. Direction is `right`, `left`, `above`, or `below`; the default is `right`. Ratio is the fraction for the new pane, greater than 0 and less than 1; the default is `0.5`. Direction and ratio require `beside`.
- Detached: `placement: {session: "session:...", detached: true}`. The worker has no tab. It still has a block ID and can be stopped. `detached: true` cannot be combined with `beside`.

Spawn results and worker listings include `session_id`. A retry must preserve the placement fields. The hub never resolves a retry against a replacement work session. Stop closes only the worker's block, including a split pane or a detached block.

Verification (Codex, Rex 1203): `workers_placement_test.go` exercises four split directions and their ratios, default tab and split placement, another session, detached placement, stop, and retry identity. It also checks that invalid targets admit no spawn intent. The tests call worker entry points with a real isolated Rex connection. Full cross-session agent handshake and restart verification belongs to the agent-session integration.

**Lifecycle.** The spawn row is the source of truth:

- `hub.spawn` commits the spawn row (status `sending`) and the new agent before it calls Rex to create the worker. A retry with the same key returns the same spawn. A retry with different content (the exact `{profile, title, args, placement}` request) is refused.
- Rex answers: `open` (the spawn row and the agent binding commit in one transaction) or `failed` (Rex refused).
- No answer (hub crash or disconnect), or a success answer without exactly one nonempty block ID: the spawn stays `sending`. Rex said yes, so a pane may exist (`TestSpawnMalformedSuccessLab`). A label is only a hint, so the hub never adopts or closes a pane by label. An agent spawn opens when its agent joins from the one pane with the spawn's label. An accessory spawn stays pending in the worker list, with "outcome unknown", and `hub.stop` refuses it.
- Join admission for a spawned agent: `sending` gives `not_ready` (or adoption, above), `open` accepts only the recorded block, `failed` and `closed` refuse (the ID is retired).
- `hub.stop` is idempotent by `spawn_id`. The user may stop any worker; an agent only the workers it started. Stop on a failed spawn dismisses it from the list.
- The hub tells the plugins (`hub.changed`) when workers, agents, profiles or plugins change, including a worker exit and a program status change.

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
- A spawn intent for a session that is gone is abandoned. An intent for a live session is searched for in that session, and stays unresolved until found. A legacy intent is searched for in every session.

Observed in the lab (Rex 1203): plugins with blocks in a gone session restarted in a new LazyRex session; destroying the work session while the hub ran restarted all three plugins in a new session within about 0.5 s; closing one plugin pane stopped it; a legacy row found its live block and replaced it; a normal restart replaced each instance in place.

Agents may run in any session (the maintainer, 2026-10-16). `hub.join` accepts a caller whose process descends from a pane's child process. It searches the work session first, then every other session, and attaches each one first. The binding records the session (`bindings.session_id`); notices, readiness and `hub.admin.rebind` use it. A dynamic agent must still join from its spawn's block. Observed in the lab (Rex 1203): codex and claude joined from a session other than the work session; a message from claude wrote the notice into codex's pane, also while that pane was detached. `hub.admin.rebind` now reads the pane's current status, as join does; before, a rebound agent stayed "not ready" until its program reported a new status. Start Codex with `--no-daemon` (for example `codex --no-daemon resume <id>`). Without it, Codex 0.163.0 starts MCP servers under its `codex app-server` daemon, not under the pane, and the hub refuses the join ("caller pid … is not in a pane of the hub's session"). Observed 2026-10-16 on the real server.

The config field `rex` is optional. Without it, the hub runs `rex` from `PATH`, which follows the installed Rex build.

## Spawn directories and path locks

Status: lab (Claude, Rex 1203, 2026-10-16). Contract agreed with Codex for the worktrees plugin.

- `hub.spawn {cwd}`: an absolute, existing directory. It replaces the profile's cwd. When given, the text as given is part of the retry identity: a retry returns the original result also after the directory is removed. Observed in the lab.
- The hub always records an explicit effective cwd (the argument, else the profile's cwd, else the home directory), with symlinks resolved (`filepath.EvalSymlinks`), and passes it to Rex. `hub.workers` shows it as `cwd`. Observed in the lab: a spawn with cwd `/tmp/…` ran in `/private/tmp/…`.
- `hub.cwd.lock {path, key}` (registered plugins): one operation (plugin + key) locks a resolved path. It answers `{locked: false, workers}` while a pending or open worker has a cwd in the path, or an unknown cwd (a row from before cwd was recorded). It also asks Rex for the live cwd of every terminal pane in every session (`com.superlogical.terminal.process`, child and foreground process, from the OS). It answers `panes` for each pane in the path, also a user's shell that ran `cd` into it. This check is a snapshot: it cannot stop a shell from changing to the path after the lock. Observed in the lab (Rex 1203): a shell that ran `cd` into the tree refused the lock until its pane closed. A retry with the same key succeeds; another key gets a conflict.
- While a lock exists, `hub.spawn` refuses a cwd in the path. The lock check, a second check of the resolved directory, and the INSERT share one transaction. The store has one connection, so transactions run one at a time: a removal (lock, remove, unlock) either refuses the spawn or sees the admitted worker and refuses the lock.
- A lock refuses a path that overlaps (is a parent or a descendant of) another operation's lock. Observed in the lab: a spawn into a locked directory got "conflict: … is locked by worktrees".
- `hub.cwd.unlock {path, key}`: only the owning operation; idempotent; works after the path is removed. `hub.admin.unlock {path}` is for manual recovery. Locks are durable: the plugin keeps a lock until it can prove its operation finished or was refused.

## App sessions and app mode

Status: lab (Claude, Rex 1203, 2026-10-16). Home Exit by Codex. Not deployed. It replaces the single swapping session below.

The maintainer's model: Home is an app launcher, and each plugin is an app. Opening an app hides the user's own sessions. "Exit LazyRex" brings them back.

App sessions:
- Each plugin has its own Rex session. Home's session is labeled "LazyRex". Another plugin's session is labeled "LazyRex: <name>". The plugin can set the name with `hub.ui.tabs {tabs, session}`; the default is its first tab title. The hub adds the prefix. "LazyRex" is reserved for Home. A name that another plugin uses gets the plugin ID added.
- The hub owns a session by its record in `ui_sessions` (key `*` for Home, `app:<plugin>` for an app), not by its label. It adopts an unrecorded session by label only when exactly one has that exact label (a crash between create and record).
- Apps are mounted only in app mode. Outside it (no cycle), the hub detaches every app view pane: the processes and unsaved input stay, and the session has no placed pane (the maintainer: "hide means detach the block"). Entering app mode restores them with the placeholder method. Home is always mounted. A pane the user put in an app session is not touched. Between apps in app mode nothing is detached.
- Migration: a view in any other session than its plugin's session is legacy. It retires with `view.retire`, and its tab opens again in the app session. The live state has only the `*` row, so the LazyRex session stays and keeps Home.

App mode (`hub/cmd/hub/appmode.go`):
- A cycle is one enter and exit. Phases: `capturing`, `hiding`, `active`, `restoring`, `complete`, or `refused`.
- Enter: `hub.ui.navigate` to a plugin other than Home, with no cycle open, records a new cycle in the same commit as the navigation.
- Capture: the worker reads `session.view` of every session that is not recorded in `ui_sessions`. It checks every field against the fields the rebuild is proven for (`rex-protocol.md`, "A whole session layout can be rebuilt exactly"). An unknown field, layer kind, split direction or ratio refuses the cycle: nothing is hidden, and the problem is on the cycle. Otherwise one commit records each window (`ui_hidden`), the counts, and the origin (the first recorded session in Rex order). Only then does the hide start.
- Hide: `session.detach_block` for every recorded block. It is idempotent, so a retry after a crash repeats it.
- Switching apps while active uses the same cycle. Nothing is captured again.
- Exit: `hub.ui.exit {for: "user", key}`. The key binds to the cycle it first exits; a retry with the same key reports that cycle, never a later one. With no cycle open, the result is phase `none`. An accepted exit returns phase `restoring`, not done. A navigation into an app is refused while a cycle restores.
- Restore, per window: a placeholder tab (its nonce recorded first, as for view restores), floating layers back to front, the tiled layer, the focused block, then zoom. The window's record commits only after Rex confirms the window. Per session: the recorded tabs go first in recorded order (`session.move_window`), tabs made while hidden stay after them, and the recorded active tab gets focus. At the end the hub selects the origin session on every app (the maintainer's choice: Rex does not report which session an app shows).
- A crash during a rebuild: the next pass finds the half-built window (the recorded window, or the window of a proven placeholder). It detaches the recorded blocks again, closes the placeholder, and rebuilds from the start.
- The hub never closes or replaces a recorded block. A block closed while hidden is a problem `closed`, and its split is pruned. A block moved to another tab is a problem `moved`, and it stays there. A block still in its original tab (an exit during the hide) is put back with its window.
- A restore that fails (not a known closed or moved pane) keeps its intent. The worker tries again with back-off (1 s, doubling, at most 30 s), and `hub.ui.mode` shows `retry {attempts, error}`. A failure count never makes a window lost (Codex's review). A failed tab order or focus also keeps the restore pending.
- A placeholder whose `new_window` outcome was unknown keeps its record after the cycle. Each pass closes such a placeholder when it appears with the nonce in its label and arguments, and drops the record when the session is gone. The hub stays attached to each such session, and a layout change there wakes the cleanup; a transient failure retries in 5 s. Lab: with the hub idle, a late placeholder closed 10 ms after it appeared, with no hub call.
- An exit key with no cycle open binds to "no cycle", so its retry never exits a later cycle.
- The session focus at the end uses `session.focus_window`, which keeps a restored zoom.
- `hub.ui.mode` reports `{mode, cycle, phase, problems, hidden, restored}` for the latest cycle. Each phase change and each restored window sends `hub.changed {what: "mode"}`.
- `hub.navigate.worker` refuses a worker whose tab app mode hides. A worker started in app mode opens visibly in the hub's work session, as before.
- `hub.navigate.agent {agent}` shows the tab of a live agent (an identity or a legacy agent) on the apps, like `hub.navigate.worker`. It refuses an agent with no open tab or a detached tab. Agents cannot call it. The messaging thread overview calls it when the user clicks an agent. `hub.agents` reports `activity` (the agent's program status, for example `working` or `idle`) for a live agent. Lab (2026-10-18): a click on an agent row in the overview made the agent's tab the active window and closed the overview. Untested: a jump to another session on a real app.
- Home (Codex): an "Exit LazyRex" widget first, with the phase, the restored counts and each problem. It stays usable when `hub.ui.mode` fails.

Lab results (observed, Rex 1203):
- `tests/appmode.mjs`: 21 windows and 29 blocks in every lab user session were hidden and rebuilt exactly in 622 to 738 ms. Same PIDs. Unsaved shell input survived. Switching apps kept the cycle. LazyRex sessions kept their tabs. Key rules held. A closed pane and a moved pane were reported; the closed pane's split was pruned with the other ratios kept; a tab made while hidden stayed after the recorded tabs.
- Crash: with `-lab-fault point=restore-window,mode=exit-mid` the hub stopped with a window half built (the placeholder and one block). A new hub reset that window and restored all 19 windows and 24 blocks exactly, with no placeholder left and no problems.
- Through the UI: a click on "Exit LazyRex" in the lab Home pane restored 18 windows and 20 panes, and Home showed "Workspace mode." with the counts.
- A detached agent pane takes notices and keeps running (`rex-protocol.md`, "A detached pane still works for agents").
- Migration: the 9 Messages views left the LazyRex session and opened in "LazyRex: Messages". LazyRex kept only Home.

- Review fixes (Codex), lab: a session closed while hidden is a `session_closed` problem; a zoomed active tab keeps its zoom; a key replay with no cycle does not exit a later cycle; a late proven placeholder closes while a label-only pane and its record stay (`tests/late-placeholder.mjs`); the crash test passes again.

Untested: app selection on a real app (no app in the lab), and a cycle refusal with a real unknown layout (unit tests cover the check).

## One LazyRex session (mount and detach)

Status: superseded by "App sessions and app mode" above. Lab (Claude, Rex 1203, 2026-10-16); deployed and live until app sessions replace it.

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
Set `plugins[].events` to `"all"` for worktrees. The hub passes `plugin_session` because Rex cannot combine `--all` with `-s`.
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
A conflicting pair of nonempty drafts stops migration with an error and preserves both drafts.

See `tests/README.md` for verification and `../../skills/lazyrex/SKILL.md` for agent instructions.
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

The hub stores `{session_id, window_id, source_view_id, source_route}` as form
context. Every form render and action receives this object in `args.context`.
The plugin does not inspect later focus to find the source thread.

Action results can contain `{form:'/forms/…'}` or `{close_form:true}`. Opening a
form preserves the source view. The hub commits the operation result before it
closes the form. A successful navigation from a form closes that form afterward.
Escape and Cancel discard unsubmitted form values. Cancellation waits while an
operation is unresolved. The source view and its draft remain in another pane.

Acceptance: test palette entry, session filtering, form submit, cancel, restart,
stale source context, and preservation of dirty source input in an isolated Rex
server. Test thread menu actions through the renderer and hub. Membership changes
must preserve delivered mail and must not backfill unread mail. New members can read full history through `messaging.history` (default 20, maximum
50 messages per page). Removed members cannot page group history. Remove app mode only after these checks pass.

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

Controlled Lua tests passed on Rex `0.0.1792158932-g88fd41`. Tests cover first-home
preservation, concurrent creation, alias migration, old operation replay, session
filtering, direct expansion, and revision stability across rename and acknowledgment.
Combined hub tests remain pending. This candidate is not deployed. The session
list follows `home_redirects` when the hub recreates a missing home session.

## One-tab migration: renderer and plugins (2026-10-17)

The hub places plugin tabs without an explicit home in the LazyRex session.
The hub restores an active app-mode cycle before migration. A failed restoration
stops migration. The hub retains old app sessions. Existing home sessions,
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

- **Directory.** The default is the source pane's `terminal.pwd` (`source_block_id` in the form
  context). Without one, each profile uses its own directory. In a Git repository with more than
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
Esc and Ctrl+Q close a panel through `hub.view.close`.

Threads and Worktrees declare landing panels. Home returns a declared panel effect.
The Threads panel lists all threads. It puts current-session threads first within each list.
Conversation tabs remain declared.

Observed on Rex 0.0.1792158932-g88fd41 in an isolated lab:
`tests/messaging.mjs` passes with the global panel and session-order checks.
`tests/home-panel.mjs` passes with panel effects and retained tab navigation.
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

Status: lab (Templates pair, Rex 0.0.1792158932-g88fd41). Not deployed. Decisions: `docs/decisions.md`, "Session templates".

- Plugin `templates` (`plugins/templates.lua`). Palette commands: "Start Session…" (`/forms/start`), "Save as Template…" (`/forms/save`), "Templates…" (panel `/panels/templates`, the landing). The engine `plugins/templates_run.lua` must sit beside it.
- Hub actions: `hub.spawn_window {key, session?, title?, layout}` starts one tab of profiles with one Rex request; `hub.spawn.spec {block_id}` gives the launch spec of a spawned pane. Contract: the header of `hub/cmd/hub/spawnwindow.go`.
- Hub config: add `{"name": "templates", "path": ".../plugins/templates.lua", "events": "all"}` to `plugins`. The profiles file needs no change; `shell` is built in.

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
