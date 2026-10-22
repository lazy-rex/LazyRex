# LazyRex guided session: UX checklist (final)

Author: claude-48 (Hub UX). Reviewer: codex-25. For: claude-43 (onboarding C) and the coordinator.
Final text: applies codex-25's reviews of drafts 1, 2 and 3.

## How to use this checklist

Results: each item gets exactly one of PASS, FAIL, NOT RUN, BLOCKED (say by what), N/A (say why),
and an artifact path for every FAIL or BLOCKED (screenshot, pane text, or log).

Three columns per item, kept apart:
- **Expected (source)**: what the code at the frozen revisions is meant to do. It is not evidence.
- **Observed**: what the user saw on the named device. Only this column is evidence.
- **Preference**: the user's opinion, when the item asks for one. Not a pass or fail.

One action per item. When an item fails, its ID names the failed action.

## A. Frozen combination (filled in before the first item; the coordinator selects it)

| Part | Value | How it was read |
|---|---|---|
| Hub binary revision | | the hub's build info or `hub.info` |
| Renderer binary (lazyrex-view) revision and vcs.modified | | `go version -m <binary>` (vcs.revision, vcs.modified), where `<binary>` is the executable of the running renderer process of the test pane (its process ID from the pane, then that process's executable path), not an arbitrary lazyrex-view file |
| SDK revision | | |
| Each plugin revision (Messaging, Profiles, Templates, Tunnels, Worktrees, Automations) | | |
| Rex build (app and server) | | `rex whoami --json` rex_version |
| Config and state target (hub dir, server) | | |
| Device(s): Mac Rex app, phone Rex app | | |
| Time of freeze | | |

Source revision main 8228cbe is the renderer the expectations below describe. It proves nothing
about what the live renderer runs: row A records that.

## B. Actors, environment and temporary resources

- The coordinator owns the live hub: release, start, stop, and the environment for the session.
  The guide does not start a second hub and does not regenerate live config or palette files.
- Agent tests run in isolated labs under the lab rules in docs/testing.md. This checklist is for the user-guided live
  session in the coordinator's environment.
- B.1 Effective plugins: the guide records the registered plugins from the running hub (the
  plugin list the hub reports, not a single config file: managed entries can come from
  state/plugins.json) and the palette entries the Rex app actually shows. Both with a time.
  Observation for context (16:20Z, 2026-10-22, files only, on the machine used to draft this
  checklist): `<hub-dir>/hub-live.json` listed only the lazyrex plugin, and
  `<rex-config-dir>/lazyrex_palette.lua` had 15 entries and no Restart entries.
  These file observations do not prove what the runtime loads.
- B.2 One rule for every action that changes state (any submit, any list or button action other
  than open/close/refresh, every palette entry that is not a pure open, and Set Up): before the
  item runs, the coordinator names the session resource it acts on (temporary agents, threads,
  templates, sessions, worktrees, automations created disabled or for this session,
  reservations, ports) or the coordinator-selected install/config target for Set Up. The item
  records that target in Observed. An item without a named target is NOT RUN.
- B.3 Cleanup removes only the resources created for this session, listed in B.2.

## 1. Controls (once each, on a form that has the control)

Yes/No (for example New Thread "Show agents in other sessions")
- 1.1 Title above two buttons Yes and No. Expected: the chosen one has the primary fill; the other
  has the secondary (muted) fill.
- 1.2 Click Yes. Expected: Yes takes the primary fill.
- 1.3 Click No. Expected: No takes the primary fill.
- 1.4 Press on Yes, release on No. Expected: no change.
- 1.5 Press on Yes, release outside both. Expected: no change.
- 1.6 Hover each button. Expected: only the hovered button changes color.
- 1.7 Tab from the field before. Expected: focus on Yes, then No, then the next field.
- 1.8 Enter on the focused button. Expected: chooses it. 1.9 Space: same.
- 1.10 visible_when (New Thread, agents in other sessions): Yes shows them. 1.11 No hides them,
  and a member choice made while visible is kept.

Select (for example Start Agent "Profile")
- 1.12 Closed: value and "▾". Left/Right change the value without opening.
- 1.13 Enter opens the list. 1.14 Space opens it. 1.15 A click opens it.
- 1.16 Open list: "✓" on the current value, "> " on the highlighted row; Up/Down move it.
- 1.17 Enter picks the highlighted row. 1.18 Esc closes without a change.
- 1.19 Typing filters, case-insensitive. 1.20 No match: "No matching options".
  1.21 First Esc clears the filter; the second closes.
- 1.22 Hover moves the highlight. 1.23 A click picks. 1.24 A click outside closes and does not
  activate the control under it. 1.25 The wheel scrolls a long list; "N more above/below" is right.

Text input (for example New Worktree "Branch")
- 1.26 One row with the "> " prompt; a bar on the left while focused or hovered.
- 1.27 Typing. 1.28 Paste. 1.29 Backspace. 1.30 Ctrl+U deletes the text before the cursor.
  1.31 Left/Right/Home/End move the cursor.
- 1.32 Long text scrolls sideways; nothing is lost.
- 1.33 Enter moves focus to the next control and does not submit.
- 1.34 Paste text with a newline. Expected: the newline becomes a space (documented change).
- 1.35 A click on the row focuses it. 1.36 The wheel over it scrolls the form.

Folder picker, Browse… (Start Agent "Directory"; New Automation "path", "cwd")
- 1.37 Browse… beside the input (under it in a narrow pane).
- 1.38 Click Browse…. Expected: status "Choose a folder in the browser…"; the Rex browser opens.
- 1.39 Pick a folder. Expected: the path replaces the field value.
- 1.40 Cancel the browser. Expected: the old value stays.
- 1.41 Focus move while waiting: click Browse…; when the browser opens, dismiss nothing, return
  keyboard focus to the renderer pane explicitly (click the pane), press Tab to reach Browse…,
  then go back to the browser and pick. Expected: the path reaches the field. If the native
  browser keeps input until it closes, record N/A with that reason.
- 1.42 Click Browse…, then type in the field. Expected: the wait ends; the typing edits the value.

Multiline (Start Agent "task", New Automation "instruction")
- 1.43 Grows up to its cap. 1.44 Shift+Enter adds a line. 1.45 Ctrl+J adds a line.
  1.46 The wheel scrolls it without moving the cursor.

Buttons and lists
- 1.47 Buttons are one row; Submit has the primary fill. 1.48 Hover changes the fill.
  1.49 Press and release on the button activates. 1.50 Press, drag off, release: nothing.
- 1.51 Visible disabled state, Send: in a thread composer with no text. Expected: Send shows
  stripes and ignores hover and click. Prerequisite: an empty composer.
- 1.52 Visible disabled state, Browse…: while an operation of the view is pending or the hub is
  not ready. Expected: stripes; hover and press do not change it. A click opens no browser and
  does not change the value; it may focus Browse… and show the status "The folder browser is
  unavailable while the view waits for the hub." (native_browse.go startBrowse). Prerequisite:
  record how the pending state was produced. (A disabled Submit is not drawn at all.)
- 1.53 Submit and Cancel share a row. 1.54 In a narrow pane they stack with a blank row.
- 1.55 List rows (panels): hover shades the row. 1.56 A click on any row of an item activates it.

## 2. Palette entries (one item per entry)

Process and report panes (not forms: no Esc-closes check)
- 2.1 Set Up: opens a window running setup; the pane stays open with the report.
- 2.2 Start Hub (coordinator only; skip in the user session unless the coordinator directs it).
- 2.3 Hub Status: reports the state of the coordinator's hub.
- 2.4 Stop Hub (coordinator only; same rule as 2.2).
- 2.5 Resume detached agents: runs at once; expected: no result in the palette. Preference: is
  the silence acceptable?
- 2.6 Restart Codex agents…: the restart form opens; record the targets listed (B.2).
- 2.7 Restart Claude agents…: same.

Forms and panels (each: opens the right surface; Esc closes it)
- 2.8 New Thread…  2.9 Threads…  2.10 Start Agent…  2.11 Start Accessory…  2.12 Start Session…
  2.13 Save as Template…  2.14 Templates…  2.15 Tunnels  2.16 Forward Port…  2.17 Worktrees…
  2.18 New Worktree…  2.19 Remove Worktree…  2.20 Automations…  2.21 New Automation…
  2.22 Agents… (the home panel)  2.23 Stop Worker…
  "Launch Profile…" shows only when the Profiles plugin is not registered; with Profiles, use
  Start Agent… and Start Accessory…
  Each is its own result.

## 3. Forms (submit and cancel are separate items)

- 3.1 New Thread: submit. 3.2 New Thread: Cancel with a changed value discards it.
- 3.3 Thread menu: Ctrl+O opens it in the thread tab. 3.4 Right-click opens it. 3.5 Ctrl+O closes.
- 3.6 Rename: submit. 3.7 Rename: cancel. 3.8 Move: submit. 3.9 Members: submit.
- 3.10 Start Agent: submit. 3.11 Start Agent: Cancel. The "Worktree <name>" rows fill Directory.
- 3.12 Start Session: submit (target per B.2). 3.13 Start Session: Cancel.
- 3.14 Save as Template: submit. 3.15 Save as Template: Cancel.
- 3.16 Template menu, Delete…: Cancel. 3.17 Delete: submit (the session's template only, B.2).
- 3.18 Forward Port: submit (the session's port, B.2). 3.19 Forward Port: close with Esc
  (it has no Cancel button; preference in section 7).
- 3.20 Release reservation, Yes chosen: submit releases (the session's reservation, B.2).
- 3.21 Release reservation, No chosen: submit. Expected: the renderer sends false (a boolean
  false is a supplied value); the Tunnels plugin refuses it with invalid_input:
  'Check "I accept this risk" to release the reservation…'. This is plugin risk acceptance, not
  renderer validation.
- 3.22 New Worktree: submit (B.2). 3.23 New Worktree: Cancel. 3.24 Remove Worktree: the picker,
  then the locked confirm form, then submit (the session's worktree, B.2).
- 3.25 New Automation: profile step Next. 3.26 Fields step: submit. 3.27 Cancel.
- 3.28 Required text left empty (for example Rename title): submit. Expected: status
  "Complete <label>"; nothing is sent. (Renderer validation applies to nil and empty text only.)
- 3.29 Esc with changed values: closes and discards. 3.30 Esc while an operation runs: waits.
- 3.31 Ctrl+N within one renderer: in a view whose tree holds both a composer and a form, Ctrl+N
  focuses the form's first field. It cannot move focus to a form in another pane.
- 3.32 Stop Worker: submit (a worker of the session, B.2); the form closes. 3.33 Stop Worker: Cancel.
- 3.34 Worker menu (Agents…, an active worker, Manage…): it opens over the panel. Open shows the
  worker and the panel comes back. 3.35 Stop (a worker of the session, B.2): the panel comes back
  without it. 3.36 Close and Esc: the panel comes back.

## 4. Panels

- 4.1 Threads 4.2 Templates 4.3 Tunnels 4.4 Worktrees 4.5 Automations 4.8 Agents: each list action and
  button, empty state, refresh. One result per panel; list failures by action.
- 4.6 Open a panel over a form with changes. Expected: "Replace this panel with X?" with Keep
  editing / Discard and replace. 4.7 Keep editing keeps the values.

## 5. Narrow and short panes, theme, input devices

- 5.1 About 38 columns: no word cut except one over-wide footer hint, which ends in "…"; the
  footer drops whole hints from the end.
- 5.2 Yes/No: narrow the pane until the Yes/No row no longer fits (it still fits at 38 columns;
  Yes + gap + No = 14 cells of content width). Expected: then they stack with a blank row.
- 5.3 Submit/Cancel: same method, narrowed until that row no longer fits. Expected: they stack.
- 5.4 Browse…: narrowed until the input would be under 20 cells. Expected: Browse… goes under it.
- 5.5 The select list stays in the pane at 12+ columns; long and Unicode labels end in "…".
- 5.6 About 12 rows: the form scrolls.
- 5.7 Select placement: record where it opened. Expected: below the field when it fits there;
  above when it does not fit below but fits above; when it fits on neither side, a reduced list
  on the side with more room.
- 5.8 Keyboard only: Tab / Shift+Tab reach every control once, in screen order.
- 5.9 Ctrl+T switches to text selection; the footer says so. 5.10 Ctrl+T switches back.
- 5.11 Phone: tap each control kind of section 1; each is hit on the first try. Phone folder
  picker: N/A unless the phone app has a browser (record which).
- 5.12 Light theme, 5.13 dark theme: titles, primary fill, chosen Yes/No, select "> " and "✓",
  input bar and prompt, disabled stripes are readable.
- 5.14 Switch the theme with a form open: it recolors; text, cursor and focus stay.
- 5.15 First draw of a form: record whether it drew in the right theme at once. Expected: the
  renderer waits up to 300 ms for the terminal's theme reply; if the reply is later, it draws
  with a fallback theme and recolors when the reply arrives (by design). A brief wrong theme is
  PASS when the late reply recolors it; record which case occurred.

## 6. Recovery paths (controlled fixtures; not every form)

The coordinator or claude-43 picks one fixture form for these.
- 6.1 Type in a text input, move the cursor to the middle; a snapshot arrives (another field
  changes). Expected: value and cursor stay.
- 6.2 Same, with a theme switch. Expected: value and cursor stay.
- 6.3 Click Browse…, press Enter in the field (focus moves to Browse…), then paste a path.
  Expected: the path replaces the value and the wait ends.
- 6.4 Focus the last No of a stack of Yes/No fields, then narrow the pane until they stack.
  Expected: the focused button stays visible.
- 6.5 Open a select over a button and click outside the list on that button. Expected: the list
  closes; the button does not activate.
- 6.6 Form close gate: in a /forms/ fixture with a submitted operation still unresolved, press
  Ctrl+Q. Expected (cancelForm runs before the general close gate): the form stays open, values
  remain, and the status is "Wait for the submitted operation before closing this form." After
  the result settles, close normally. Local unsaved values alone do not block form
  cancellation; item 3.29 covers their discard.
- 6.6b Replacement gate: with unresolved work, open another panel over the form. Expected
  (panels.go): "Replace this panel with X?"; "Discard and replace (unavailable)" while it is
  blocked; Keep editing keeps the values. The renderer has no general guard against a second
  submit; this checklist does not test or promise one.
- 6.7 A draft conflict in a composer: both texts are kept and the choice is explained.

## 7. Questions for the user (preferences, not results)

- Resume detached agents shows no result (2.5).
- "LazyRex: Tunnels" lacks the "…" of the other panel entries (2.15).
- Start Session root and New Worktree repo path have no Browse… (3.12, 3.22).
- Forward Port has no Cancel button (3.19).
- Automation trigger fields are always shown, not conditional on the trigger (3.26).
- Tunnels' refusal says 'Check "I accept this risk"', but the field is now a Yes/No pair (3.21).
  The wording belongs to the Tunnels plugin.
