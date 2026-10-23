# LazyRex view renderer

This renderer implements the version 1 contract in the parent README.
It runs only in a lab Rex pane. The hub owns its route and pane identity.

## Build

```sh
cd spikes/03-ui-framework/view
GOMODCACHE=/tmp/lazyrex-view-mod GOCACHE=/tmp/lazyrex-view-build go build -o /tmp/lazyrex-view-spike3 .
```

The hub starts the binary with these arguments:

```text
/tmp/lazyrex-view-spike3 -view-id <id> <plugin> <route>
```

The renderer reads `REX_SERVER`, `REX_SESSION`, and `REX_BLOCK` from its pane.
It accepts only Unix sockets under `/tmp`, including the macOS `/private/tmp` alias.
It refuses a symlink that resolves outside those directories.

The Go module proxy reported these latest versions on 2026-10-16:

- Bubble Tea `v2.1.0`.
- Lip Gloss `v2.0.6`.
- Bubbles `v2.2.1`.

The renderer uses the existing project's WebSocket dependency version, `v1.8.15`.

## Interaction

- Press Tab or Shift+Tab to move focus.
- Press Enter in a composer to send its current text.
- Press Shift+Enter to insert a newline when the terminal reports distinct keys.
- Press Ctrl+J to insert a newline without key negotiation.
- Click Back, Send, a list item, or a form control to use it.
- Use the mouse wheel or Page Up and Page Down to scroll.
- Press Ctrl+T to turn off mouse capture for native text selection.
- Press Ctrl+T again to restore mouse controls.
- Press Ctrl+Q to exit the renderer.

Enter fixes the submitted text before a pending draft save completes.
Later edits remain local until the send has a known result.
The renderer saves those edits against the revision returned by that send.

The composer hides routine save notices. Errors and conflicts remain visible.
A draft conflict preserves local text and offers Keep my text and Use saved text.
Removed dirty controls keep their text in a recovery section.
Native close can lose text that storage has not acknowledged.

## Modes

The renderer decides its mode once per process (thread view v1.2, item 1; view/mode.go).

- Neutral: before the first tree with content. Loading and error nodes do not decide.
  The renderer draws inline and prints nothing. The first frame is "Connecting to the hub…".
- Thread: the first tree with a history node, no list or form node, and a layout other than `transcript`.
  The renderer stays on the primary screen for the life of the process, without mouse reporting.
  The terminal owns the scrollback and the pointer, so Page Up, Page Down, Ctrl+T and mouse events do nothing.
  This applies to every inline frame: neutral, thread, and the wait before alt-on.
- Alt: the first tree with any other content, a history node beside a list or form, or a paired composer
  (layout `transcript`). The renderer uses the alt screen as before.

No route decides the mode. A form route is neutral until its first tree.

Two defect cases show a fixed status note once per process:
"view defect: history beside a list or form" and "view defect: history in alt mode".

The live region is the composer and the other nodes, then one status line.
They get at most the pane height minus 2 rows, and at most 8 rows beside a history node.
So every inline frame is at most the pane height minus 1 rows. Alt mode keeps its full-pane layout.
When the rows do not fit, the renderer cuts text rows only, and ends each cut with "… <n> more lines".
When the controls alone do not fit, the live region is a window that always holds the focused control,
with "… <n> more above" and "… <n> more below" rows.
Tab and Shift+Tab reach every control, and the window follows the focus.
A composer's editor and Send are one focus span, so a short window holds the editor row and Send together.

The last frame before the exit is empty, on the screen that is active, also during a panel replacement.

### The clear at process start

Every renderer process clears its pane once (cursor home, ESC[2J, ESC[3J), before Bubble Tea writes anything.
This applies to thread mode and alt mode, because the mode is not decided at start (lead-tv-clear2).
A renderer restarted in the same pane (a reload, or a respawn after an output failure) would otherwise print the thread history a second time below the old copy.
In a new pane, the clear removes nothing.
If the clear's write fails, the process exits with status 1 before it starts Bubble Tea or connects to the hub.
The `-transcript` printer keeps its own clear at its first rebuild.

### Printed messages (thread mode)

Thread mode prints each message of the current history page once, above the live region, oldest first (view/print.go).
The terminal owns the printed lines. The renderer never rewrites them.

- The ledger of printed message IDs belongs to the process and its pane. A reattach or a new tree does not clear it.
- One print runs at a time. Later batches wait in order.
- A print commits its IDs after its write returns. A failed write commits nothing.
- A message is late when a printed message comes after it in the page.
  The renderer prints it at the bottom, with "(late)" after the author.
- The first print waits for the terminal's theme reply, or for the theme timeout (300 ms).

### The history fetch (thread mode)

The renderer gets older pages through `hub.view.history` (view/fetch.go).
The model owns all fetch state. One page call runs at a time.

The open runs once for each process:

- When the first history page has `has_more` and holds fewer than 500 messages, the renderer fetches older pages until it holds 500 or more, or until `has_more` is false.
- The renderer prints the newest 500 as one batch. In full mode (`Thread Settings… > Show full history`), it fetches every page and prints all messages.
- A count line comes first: "N earlier messages. Thread Settings… > Show full history", where N is `total` minus the printed count. Without `total`, the line is "Earlier messages exist. …". A thread of 500 or fewer messages has no count line.
- During the open, the notice row shows "Loading history…".

The excluded set (lead-tv-excl):

- The excluded set holds every message ID that the open saw but did not print. Full mode has no excluded IDs.
- The set belongs to the process. A reattach does not clear it, and it is separate from the printed ledger.
- A later frame never prints an excluded ID.
- A new ID that is neither printed nor excluded prints at the bottom. It is late when a printed message comes after it.

The gap fill:

- A gap is a page with `has_more` that holds no printed, queued or excluded ID (for example, after a reattach that missed many messages).
- The renderer fetches older pages until a page holds a printed ID. It then prints the messages after the last printed ID in page order (the anchor), oldest first, not late. The fill never prints a fetched message before the anchor.
- Excluded IDs are never an anchor.
- When no page holds a printed ID, the fill has fetched the whole thread. It prints every fetched message that is not printed or excluded, oldest first, not late (lead-tv-noanchor).

Page rules:

- A new current page during a fetch joins the fetch when its oldest ID is in the fetched run. Any other page, or a new attachment, restarts the fetch from that page.
- A joined page replaces the run from its oldest ID on: the page gives the thread order for its range, also for a new message inside it.
- The opening exclusions stay across a restart. A result of an old fetch or an old attachment is dropped.
- A failed page, or a page with `has_more` and no new message, is asked again after 2 s.
- A reattach, a reload, a close, an output failure or a lost connection stops the fetch and its page call.
- A close (Ctrl+Q or the hub), a reload or a retirement stops the fetch when it is requested, also while it waits for drafts or operation results. Until it ends, no tree starts a fetch. A failed close ends it: the view reads its current tree again and fetches again.
- Seen stays closed while a fetch runs.

### Keys (thread mode)

The composer's keys are declared once (view/keys.go).

- Enter sends the composer's text (send_action).
- Shift+Enter, Alt+Enter and Ctrl+J insert a newline. Alt+Enter is a renderer binding: the keys that a Mac or a hardware iOS keyboard sends for it are not observed.
- The footer hint names Shift+Enter, the first newline key: "Enter send · Shift+Enter newline", or below 60 columns "Enter send" and "Shift+Enter ↵".
- The iOS probe recorded no modified Return from the app's on-screen keyboard (docs/rex-protocol.md). Multi-line input there is an accepted limit. A hardware keyboard on iOS is not tested.
- Ctrl+O opens the menu, Ctrl+Q closes the view, and Tab and Shift+Tab move the focus. Thread mode has no mouse.

### The seen gate

The renderer sends no `hub.view.seen` until the mode is decided.

- Alt mode sends one seen at the decision, then uses the focus rules below.
- Thread mode sends seen only after a print that holds the current tree's newest message ID.
- A tree without a history page, or with an unprinted newest message, sends no seen.
- A new attachment sends no seen until its first thread frame. A print receipt that comes before that frame (a print of the old attachment's tree) opens nothing.

### Output failures

The first failed or short write to the pane is fatal (view/output.go):

1. The renderer prints nothing more, commits no print, and sends no seen.
2. The renderer saves the current text of each dirty composer (also while a send waits) with one `hub.view.draft.save` call and a new operation key.
   All saves share one 5 s deadline. There is no retry.
3. The process exits with status 1. It writes nothing to stderr, because stderr is the same pane.

A timed-out save has an unknown outcome. The next view reads what the plugin committed.

Known limit: the respawn after this exit makes a new view, and a new view has no `open_args`.
So full mode ("Show full history") ends, and the new renderer prints the newest 500 messages.

### Known limit: the clear before the alt screen

Before alt-on, the renderer draws an empty inline frame and waits `altClearDelay` (50 ms).
This is best effort: Bubble Tea v2.1.0 enters the alt screen without erasing the inline rows,
and it has no frame-written signal.
On a slow flush the neutral rows ("Connecting…", or an error drawn while neutral) can stay
on the primary screen after the alt screen ends.
Those rows are cosmetic and never hold a printed message.
Do not use this timing pattern for anything that holds a message.

At exit and before a reload, the last frame is empty, so no live row stays on the primary screen.

## Verification

```sh
GOMODCACHE=/tmp/lazyrex-view-mod GOCACHE=/tmp/lazyrex-view-build go test -race ./...
```

The protocol tests bind temporary Unix sockets under `/tmp`.
They require permission to bind those sockets when the sandbox blocks it.

The tests exercise the real renderer connection code against a controlled protocol fixture.
They check startup retry, lost hub candidates, subscription, completion notices, and recovery after a committed operation loses its reply.
The fixture is not a Rex server. These results do not establish real hub integration.

State tests cover draft conflicts, local cursor and text preservation, removed controls,
newer text during sends, stale snapshots after sends, and input actions.
Schema tests reject unknown versions and oversized history pages.

## Lab results

Observed on Rex `0.0.1792158932-g88fd41`, 2026-10-16, in `/tmp/rexlab-s3ui`.
The tests used the real renderer, hub, and messaging plugin.

- Back and inbox Open produced successful navigation results in `view_ops`.
- Two fresh views edited one draft concurrently. One saved; the other retained its text and showed a conflict.
- Keep my text saved the second draft against the current revision.
- The hub replaced renderer PID 2875 after a verified crash. The replacement restored the acknowledged draft and showed Saved.
- An idle renderer reattached after a hub restart without terminal input. Its view, block, and renderer client IDs stayed the same.
- Kitty Shift+Enter input inserted a newline and saved the multiline draft.
- An SGR mouse press and release on Send created one message and cleared the matching draft.

The first complete idle recovery took 6.8 seconds after hub start.
After the renderer added recovery on hub discovery events, attachment took 173 ms.
That faster run exposed a hub race: the plugin had not registered, and the screen stayed on an error view.
The hub fix keeps a loading state for transient failures and refreshes views when the plugin registers.
A subsequent restart passed without terminal input: the same renderer showed the thread, history, and saved composer.
In that run, hub generation 6 started at `20:51:45.653074Z`; the renderer reattached at `20:51:45.781217Z` (about 128 ms).
The attachment time does not measure the time to the completed screen render.

Evidence: `/tmp/rexlab-s3ui/int.log`, `/tmp/rexlab-s3ui/int/events.jsonl`, and the read-only inspection of `int/hub.db`.
The 18 renderer tests pass with the race detector. Raw output: `/tmp/lazyrex-view-tests.json`.
The protocol fixture verifies recovery of a committed operation after its reply is lost.
It does not replace the full lab test of a hub restart during a send.

Physical mouse behavior, iOS touch, and two real apps with different sizes remain unverified here.

### No hub

The renderer finds the hub with `target.resolve` (label `lazyrex-hub`) and `client.inspect` for each candidate.
It needs exactly one connected hub. The status text depends on the result:

- No connected hub: "No hub: run LazyRex: Start Hub. If the palette has no such entry, run lazyrex-hub setup and the Rex config reload that it prints. This pane retries the connection automatically."
- More than one: "N LazyRex hubs are connected. This pane needs exactly one."
- Any other failure: "Hub unavailable: " and the error. A failed `client.inspect`, an inspection without a connection state, and an unexpected or incomplete `target.resolve` reply (no result, an unknown status, a resolved target or a candidate without a client ID, `ambiguous` without candidates, `not_found` with candidates) are errors, not a count. A client in any other state (for example `lost`) is not a hub.

The footer notice is one row, cut at the pane's width, so the text starts with the action: "No hub: run LazyRex: Start Hub." (31 columns) shows whole in a 38-column pane.
The renderer maps control characters in the status text to "?" (`paneSafe`, a copy of the hub's).
It retries with the same backoff and jitter in every case.

Tests: `TestNoHubSaysHowToStartOne`, `TestTwoHubsSayThePaneNeedsOne`, `TestDiscoveryFailuresAreNotACount`, `TestConnectionStatusHasNoControls`, `TestPaneSafe` and `TestNoHubNoticeInANarrowPane` (the protocol fixture and the model).
Lab: `HUBUNAVAIL_LAB=1 go test -run TestNoHubTextLab .` starts its own lab server and hub, and opens the hub's restart form (a real renderer pane) at 38×14.

Lab: `CHUNK_LAB=1 go test -run 'TestReplyParts(Hold|Timing)Lab' .` starts its own lab server and hub
(`-test`) for each test. TestReplyPartsHoldLab: the plugin `testdata/holdparts` stops a render of more
than 150 KiB before its last reply part. The tree fits on one screen. During the hold the pane shows
the first and the last node of the old tree; after the release it shows those of the new tree. TestReplyPartsTimingLab: through the real SDK (`tests/sdk-parts`), the median
`view.render` call of a 150 KiB panel in parts takes at most 50 ms more than a 40 KiB panel inline.
It stops the hub, waits for "No hub: run LazyRex: Start Hub." on the screen with no row wider than 38 columns, starts the hub again, and waits for the attached form without the text.
Observed on Rex `0.0.1792158932-g88fd41`, macOS, 2026-10-22: pass. Rex answers `not_found` with no candidates when no client has the label.

### Renderer reload

The renderer declares `view.reload`. It accepts requests only from the attached hub,
with the current view ID and attachment. It replies before it starts reload work.
The reply confirms admission. A new attachment confirms completion.

The renderer saves local composer text before reload. It waits for all operation
results. It keeps the current process when a draft conflict, unsaved form, or
removed control contains local values. The status line explains the condition.
After a successful form submission, the renderer records the submitted values as
saved. New edits still prevent reload.

When these checks pass, Bubble Tea restores the terminal. The renderer then calls
`syscall.Exec` with the executable path captured at startup, the same arguments,
and the same environment. This permits an atomic binary replacement at that path.
A reload does not preserve cursor position, focus, or scroll position.

Observed on Rex `0.0.1792158932-g88fd41`: `hub.ui.reload` admitted the request for
`view-096218f1a922e7e5`. The renderer kept PID `46018` and the same pane. It attached
with a new Rex client at `2026-10-16T20:57:38.516623Z`. The composer restored
`reload preserved draft` and showed `Saved`. Two older renderers returned
`not_supported`, as expected. They did not restart.

Tests verify that reload waits for a save result, a generic action result, and
connection recovery. Tests also verify that conflicts, removed controls, and dirty
text or choice fields prevent reload. These tests pass with the Go race detector.

### Renderer retirement

The renderer declares `view.retire` for migration between UI sessions. The request
must name the current view and attachment and come from the attached hub. The
renderer replies `{status: "accepted"}` before it starts retirement.

Retirement uses the reload safety checks. It saves composer drafts and waits for
operation results and a hub connection. Conflicts, unsaved forms, and recovery
text prevent exit. The status line identifies the blocking condition.

After these checks pass, the renderer restores the terminal, clears its program
status, and exits. It does not execute the binary again. Retirement takes priority
when reload is also pending. The hub must observe process exit before it creates
the replacement view. An admission reply does not prove retirement is complete.

Model tests cover blocked retirement, draft saves, pending actions, and reload
priority. Protocol fixture tests cover admission and hub, view, and attachment
checks. Full migration with the real hub remains untested by these tests.

### Layout and read notices

The renderer uses Lip Gloss for layout and colors, Bubbles for the textarea and
viewport, and Glamour `v2.0.1` for message Markdown. The Go module proxy reported
that Glamour release as current on 2026-10-16. The renderer uses the pane width. Each message uses at most 70 percent of that
width, with a maximum of 100 terminal columns. It adapts colors to the reported terminal background.
It respects `NO_COLOR`; the test shell sets that variable, so the lab launcher
removes it for color checks.

A single composer stays at the bottom in panes with at least 18 rows. Smaller
panes and views with multiple composers keep the composers in the scrolling body.
Send remains a mouse control. The hint beside Send does not activate it. The
renderer follows `tree.back`; a plugin can omit Back. `Ctrl+N` moves focus to the
first form field. The header sets the terminal title through Bubble Tea.

History starts at the latest message. New messages keep the history at the bottom
if the user was already there. Otherwise, the renderer preserves the row anchor.
The renderer displays compact local times. It groups consecutive messages from
the same author only within two minutes. Each message has its own border and gap. Optional message field `own: true` adds a `You` label and aligns the message right. The plugin
supplies the author's display name.

The renderer enables terminal focus reporting. It sends `hub.view.seen` on
focus-in, and when a history node's last message ID changes while focused. Draft
changes alone do not send a read notice. The serialized client loop sends these
notices without operation keys. Blur prevents queued notices from sending if
the client has not already started the call. The renderer starts with unknown
focus, treated as not focused. Rex app forwarding of focus events, including
initial focus after reload, remains unverified. Hub navigation handles its own
visibility evidence.

Observed on Rex `0.0.1792158932-g88fd41` in `/tmp/rexlab-codexui`: the renderer sent
multiline Markdown through the messaging plugin and rendered the saved message.
The renderer's New thread form created `Design lab` with Claude and Codex selected.
The hub declared the new group tab and attached its renderer. These checks used
an isolated copy of the hub and plugin. No app connected to this lab, so these
checks do not prove app navigation or physical touch behavior.

Tests pass with the Go race detector. They cover 38×24, 80×24, and 140×40 layouts
in light and dark colors; fixed composer position; scroll anchors; Markdown;
form focus; mouse bounds; small panes; multiple composers; and focus-based read
notices. Existing draft, conflict, send, reconnect, and reload tests also pass.
Visual fixtures come from the model's ANSI output. They are not Rex app screenshots.

### Pointer controls and growing editors

Lip Gloss compositor bounds use the same rows as the visible controls. Hover does
not move keyboard focus. A button activates on release inside the pressed button.
Send stays inside the composer. Its adjacent hint does not activate it.

Bubbles grows each multiline textarea with its content. A separate viewport caps
the visible editor at ten rows, or fewer in a short pane. Wheel input over an
editor changes its viewport without moving its cursor or changing its text.
Typing returns the viewport to the cursor. Wheel input over the body scrolls the
body. Routine save and send notices do not change the layout.

A scrollbar occupies the right margin when the body exceeds its viewport. Drag
the thumb to scroll. Click the track to move the thumb to that position.
Hover over the scrollbar to widen it. The thumb highlights under the pointer
and during dragging. Hover uses the last frame's geometry. Motion within the
same scrollbar part reuses the frame.
Ctrl+T disables mouse capture for native text selection.

Tests verify hover, canceled button presses, exact Send bounds, wheel routing,
long draft preservation, scrollbar dragging, track clicks, and stable status
height. The existing state and protocol tests pass with the race detector.
Model fixtures cover light and dark layouts at 38×24, 80×24, and 140×40.
These fixtures do not establish physical mouse behavior in a Rex app.

### Performance regression and correction

The first interaction pass repeatedly laid out all history messages during pointer
motion. It also passed all history rows to a new viewport on each render. The
small layout fixtures did not expose this cost. The live renderer was reverted.

The corrected renderer caches complete message rows. The cache checks message
content, author, ownership, time grouping, pane width, and theme. The viewport
receives only the visible rows, which already have terminal-width wrapping.
Pointer motion uses the last visible controls. If the hovered control does not
change, the renderer reuses its last frame. Scrollbar dragging still updates the
layout. Other model updates invalidate frame reuse.

Measured on an Apple M2 Max with Go benchmarks at 200×60: the synthetic fixture
contains 30 long Markdown messages. Before correction, View took 240.5 ms and a
motion update plus View took 608.2 ms. After correction, View took 4.08 ms and an
unchanged-hover motion update plus View took 12.2 microseconds.

With the 32-message Team fixture, View took 1.77 ms. A key insertion and a
backspace, each followed by a frame, took 7.95 ms together. A burst of 256 motion
events followed by one key and frame took 6.2 ms in the regression test.
These model measurements exclude Rex input transport and app presentation.
The regression test allows 250 ms for the burst, including race instrumentation.

Run the benchmarks with `go test -run '^$' -bench BenchmarkLargeConversation`.
Set `LAZYREX_PERF_FIXTURE` to a JSON file of `{from, body}` messages to test another
history. The default synthetic fixture does not depend on private message data.

### Padded controls

A button is one row, in the shape of huh's ThemeCharm: the label with two cells of
padding on each side, and the padding belongs to the button. Adjacent buttons are one cell
apart. Buttons that do not fit side by side stack, with a blank row between them. Keyboard
focus underlines the label without changing dimensions. List targets contain four rows: top padding, name, detail, and bottom padding. Long list labels
and details use an ellipsis. Adjacent targets share a boundary without overlap.

Send is a one-row, ten-column button on the first footer row of the composer. Its hint
remains outside that region. Narrow panes show the newline hint on a second footer row.
Browse… is on the row of its text input. Send and form submission use the primary style. Other buttons use a muted
style. Keyboard focus and pointer feedback remain visible.

These changes apply the ui.sh button and form guidance in terminal cells. The
renderer does not know the physical size of a cell, so these dimensions do not
prove a 48×48 pixel touch target. The scrollbar remains two columns wide during
hover. A physical touch check in the Rex phone app remains necessary.

### Message wrapping

Lip Gloss includes borders and padding in a style's width. The bubble width is
its Markdown text width plus two border columns and two padding columns. The
renderer preserves the Markdown renderer's line breaks inside that bubble.
A regression test compares every paragraph line across adjacent pane widths.
It prevents a second wrap from placing the last words on extra short lines.

### Control gallery

Set `LAZYREX_GALLERY` to an existing directory, then run
`go test -run 'TestControlGallery|TestMessagesGallery'` to export ANSI fixtures.
The gallery uses the production controls at 38 and 100 columns in both themes.
It shows supported normal, hover, keyboard focus, pressed, chosen, and
unavailable-action states. The schema does not define disabled form fields or
selected list items. The gallery does not invent those states.

Lists and forms use at most 72 columns. List hover shades the text and equal
padding rows above and below it. An accent bar marks keyboard focus. A boolean
field is a title and a row of two ordinary buttons, Yes and No. The chosen value has the
primary style and the other the secondary style. Each button is one Tab stop; Enter, Space or a
click chooses it. Button hover and press change the button fill. Primary buttons use the
highlight role, and its hover and press roles keep readable label contrast.
Disabled buttons show diagonal stripes beside the label. Disabled buttons do not respond to hover or press.

PNG fixtures use approximate font metrics. They do not prove glyph joins or
physical touch dimensions in a Rex app. The live composer screenshot shows
joined rounded borders.

Widget text wraps at word boundaries, with hard wrapping only for words longer
than the available width. A model test verifies that Ctrl+N moves from the
composer to the first form field and routes subsequent typing to that field.
The lab must also check the terminal's key encoding.

## Menus and form fields

A closed select shows its current value and `▾`. Left and Right cycle its options.
Enter, Space, or a click opens a floating list. The list shows up to eight options.
Lip Gloss composes the bordered list over the form. Opening it does not move form rows.
The border contains two cells of horizontal padding and blank rows above and below the options.
The highlighted option has huh's `> ` selector and label in the highlight role; the current value has a `✓` in the selected role. The border uses the control role.
The list width uses the wider of the field control and the widest option with its check-mark column.
Padding and borders add six cells. The pane edge caps the result.
Typing replaces the top blank row with `Filter: <text>`. Clearing the filter restores the blank row.
The list uses its unfiltered options for geometry, so filtering keeps its origin and width.
The list opens below the field when it fits. Otherwise, it opens above the field.
If neither side fits, the list uses the side with more room and reduces its option window to at least three rows.
A pane too short for that minimum can make the list overlap its field. The canvas clips the list to the pane.
The count line shows only options outside the window. The line disappears when all options fit. Narrow lists use arrows and counts.
The check mark identifies the current value. The selector identifies the candidate value.
Up, Down, Home, End, and the mouse wheel move the highlight.
Pointer motion over an option also moves the highlight. Leaving the list keeps that highlight. Typing filters labels
by a case-insensitive substring. Backspace edits the filter. Enter or a click picks
an option and closes the list. Escape clears a nonempty filter first. Otherwise,
Escape closes the list without a change. Moving focus away also closes without a change.
Clicks and wheel events reach the list before covered controls. A click outside closes
the list without a change and does not activate the control underneath. Scrolling the
form closes the list. Resizing keeps the focused field visible and recalculates the list position.
Empty selects do not open. Option labels truncate instead of wrapping.

Model tests cover filtering, picks, cancellation, tree updates, scrolling, and narrow labels.
They also check unchanged form rows, upward placement, reduced windows, and clicks over other controls.
`select_test.go` covers the overlay; Profiles `tests/start.lua` covers the Start form end to end.
Observed on Rex `0.0.1792158932-g88fd41`: keyboard and mouse picks submit the selected
profile through the existing action. The highlight stays visible at 32 columns and 20 rows.
The lab also checks unchanged rows outside the layer and Placement above its field.
It captures the list at 90 columns and 65 rows, 32 columns and 20 rows, and 32 columns and 16 rows.
The lab moves the pointer to a new option, leaves the list, and uses Enter to pick that option.
The lab helpers use the current-value check mark to detect an open list.
Use `tests/profiles-lab-setup.sh` for the isolated server and hub configuration.
Replace its agent profiles with `Agent 01` through `Agent 12` before starting the hub.
Each command must print `selected-agent-N`, then sleep. Set `PROFILES_LAB` to the lab path.

Pointer motion keeps the existing all-motion mode. Messages retain their cached-frame path.
An open list also reuses its frame when the candidate does not change.
Observed on an Apple M2 Max, 2026-10-18: the 84-message synthetic fixture matches a live thread's message count and text lengths.
`BenchmarkLargeConversationMotionFrame` took a median 10.950 µs before this change and 11.060 µs after it.
Both runs used 1577 bytes and 18 allocations per event. Unchanged select motion took about 18 ns with zero allocations.
These benchmarks measure model updates and frames. They do not measure terminal input latency.

Full history in the transcript pane: a view tree from a Lua plugin must fit in one 64 KiB Rex
control message (rex-protocol.md, "Rex Lua runtime"). The messaging plugin sends the newest
messages within that budget and sets `has_more` when older ones exist. The transcript pane then
calls `hub.view.history {before}` with the ID of its oldest message, page by page, until
`has_more` is false. It prints the whole thread once, into native scrollback. Later snapshots
append to the merged thread. When a snapshot's oldest message is unknown (a burst of messages),
the pane fetches the pages again. A failed fetch leaves the newest messages and the older marker.
`hub.view.history` answers `{status: "pending"}` until the page is ready: a hub handler never
waits for a plugin. The pane asks again every 100 ms, for at most 20 s. Lab (2026-10-18): a
287-message thread loaded in two pages (105 and 78 messages) within one second.

A `menu` node has a label and an action. The renderer shows no button.
Ctrl+O or right-click invokes the first menu's action. The footer shows `Ctrl+O menu`.
The plugin returns a form route. The hub opens that menu in a side pane.
Menu choices use a list node and the normal view action protocol.
A form tree with `menu: true` belongs to a menu. In that form, Ctrl+O closes the form, like the
Cancel button, and the footer shows `Ctrl+O close`. In other forms, Ctrl+O does nothing.

A list whose items all have no `detail` is a compact list. Each item uses one row: an optional
one-cell `icon`, the label, and an optional `status` at the right edge. An enabled item ends in
`›` and gets the hover and focus highlight. A compact list shows its label as a dimmed uppercase
heading. A list with details keeps the four-row targets.

Form layout:
- Each field has a title row and one blank row after it.
- A field of kind `text` is one row in the shape of huh's Input: a bar while the input has
  focus or the pointer, then the `> ` prompt (highlight role while focused) and the Bubbles
  textinput. The input scrolls long text sideways and has no length limit. Newlines and tabs in
  a value or a paste become spaces; a value normalized this way is not an unsaved change.
  Enter moves to the next control, as Tab does. It never submits. The wheel over the input
  scrolls the form. A `multiline` field keeps the growing textarea in a box.
- Browse… sits to the right of its text input, on the same row. The input is narrower by
  the button width plus one cell. When the input would be narrower than 20 cells, Browse…
  goes under the input.
- The submit button and the plain button nodes after the form (a `widget` with no text and an
  action, such as Cancel) share one row, two cells apart. They stack when the row does not fit.
- A row can hold more than one control (`row.Extra`). Each control has its own `Left` and
  `HitWidth`. Tab order goes from left to right.
- The footer names PgUp/PgDn history only when the view has a history node.
- When the footer help does not fit, it drops whole hints from the end. Only a single hint that
  is wider than the pane is cut, and it ends in an ellipsis.

Ctrl+Q in a tab waits for saved drafts and unresolved operations. It calls
`hub.view.close` before it quits. A failed close leaves the view open and shows
a retry message. Forms retain their cancel behavior.

A form field can set `visible_when: {field, value}`. `field` names a boolean field
in the same form. `value` is the required boolean value. The renderer evaluates
this condition locally. Hidden fields retain their values and remain in the
submitted values. A condition cannot refer to the same field.

A text field can set `browse: "native"`. The renderer shows Browse beside its label.
Browse focuses the editor and calls `hub.view.app_action` with `pane.insert_path`.
The renderer keeps the value until a paste arrives. The paste replaces the value
and ends the wait. A key press with text ends the wait and edits the value normally.
This also applies to a manual paste while the picker waits.
Escape, another Browse activation, or a change of editor focus ends the wait.
Focus on the field's own Browse… (by Tab or Enter) keeps the wait. A paste then goes to the
field: it replaces the value, ends the wait and focuses the field.
These actions preserve the value. App blur does not end the wait.
A refusal shows an error and preserves the value. The renderer does not retry
the app action automatically. The plugin can keep its folder list as a fallback.

When a new tree changes a field value, a clean editor takes that value.
An editor with unsaved changes keeps its local value. An unchanged tree value
does not reset a submitted value. A field that waits for native input also keeps
its value. Model and protocol tests verify these rules. The combined picker path
still requires a test with the Rex app and the plugin.

A field can set an integer `rev` for an explicit replacement choice. A present
revision that differs from the previous tree replaces the editor value, including
unsaved edits. It also ends that field's native picker wait. Revision `0` differs
from an omitted revision. An unchanged or omitted revision uses the clean-field
rule. Plugins change `rev` for explicit choices, not for ordinary renders.

A route that starts with `/forms/` identifies a form in a side pane. Escape and an action
named `$cancel` exit that renderer. The renderer waits if a mutation is unresolved.
Cancel discards only local form values. The hub keeps the source view separate.

A saved action can have a pending UI step, such as navigation or opening a form.
The renderer keeps the operation unresolved while `effects` is `pending`. If that
step fails, the renderer shows the error and a Retry button. Retry calls
`hub.view.effect.retry` with the original operation key. It does not submit the
plugin mutation again. Submitted form values remain saved after a UI step fails.

Forms (`/forms/`) use the terminal's normal background. A form waits for the first
scheme or background reply before drawing its controls. After 300 milliseconds,
it draws with the fallback palette if no reply has arrived. Later replies still
update the palette. Ordinary thread views do not wait.
