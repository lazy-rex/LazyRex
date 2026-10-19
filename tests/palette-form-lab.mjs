// Lab: palette commands and forms (forms.go). Lab only: /tmp/rexlab-pal, server started with
// LAB_CONFIG=/tmp/rexlab-pal/init.lua (one line: pcall(require, "lazyrex_palette")), hub on
// /tmp/rexlab-pal/int.json with the plugins lazyrex and tests/formprobe.lua, palette_lua set.
// Needs a renderer that focuses a form's first control (Codex's palette renderer, c35416a or later).
// Checks: the generated palette action exists; a palette run opens a focused, zoomed form split in the
// window of ctx.block_id and returns session.select for the app; the form gets the stored context;
// {form} chains forms, {close_form} closes, Ctrl+Q exits, a user close ends the form; a form opened
// from a tab records that view as its source; forms are not tabs (no retirement, no crash count);
// bad palette calls fail clearly.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.PALETTE_LAB || '/tmp/rexlab-pal', SOCK = `unix://${LAB}/rex.sock`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
await c.call('client.set_label', {label: 'palette-form-lab'});
const me = (await c.call('client.list')).clients.find(x => x.info?.label === 'palette-form-lab' && x.connection_state === 'connected').client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const hubErr = async (action, args) => { try { await hub(action, args); return ''; } catch (e) { return String(e.message ?? e); } };
const waitFor = async (what, f, ms = 10000) => { for (let t = 0; t < ms; t += 100) { const v = await f(); if (v) return v; await sleep(100); } assert.fail(`timed out: ${what}`); };
const rex = (...a) => execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', ...a], {encoding: 'utf8'});
const EV = `${LAB}/int/events.jsonl`;
const allEvents = name => fs.readFileSync(EV, 'utf8').split('\n').filter(l => l.includes(`"event":"${name}"`)).map(l => JSON.parse(l));
const startAt = new Date().toISOString();
const events = name => allEvents(name).filter(e => e.at >= startAt); // only this run's events
const shell = label => ({block: {flavor: 'com.superlogical.terminal.shell', label, options: {command: ['/bin/sleep', '600'], login: false}}});
const layersOf = async (sess, win) => (await c.call('session.view', {session_id: sess})).windows.find(w => w.window_id === win);
const formBlock = async (sess, win, id) => (await layersOf(sess, win))?.layers?.flatMap(l => l.blocks).find(b => b.label === `lazyrex-form:${id}`);
// The renderer wraps long text, also inside words: compare with all whitespace removed.
const screen = (sess, block) => { try { return rex('capture', '-s', sess, '-b', block).replace(/\s+/g, ''); } catch { return ''; } };
const shows = (sess, block, text) => screen(sess, block).includes(text.replace(/\s+/g, ''));
const viewRow = id => execFileSync('sqlite3', ['-readonly', `${LAB}/int/hub.db`, `SELECT status || '|' || closed_by FROM views WHERE view_id = '${id}'`], {encoding: 'utf8'}).trim();
const press = (sess, block, ...keys) => rex('send-key', '-s', sess, '-b', block, ...keys);

assert.match(rex('actions'), /lazyrex_formprobe_probe\s+LazyRex: Probe Form…/, 'the generated palette action is loaded');

const S = (await c.call('session.create', {label: 'palette-work', initial_windows: [{window_label: 'one', layout: shell('one')}, {window_label: 'two', layout: shell('two')}]})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
try {
  const v0 = await c.call('session.view', {session_id: S});
  const one = v0.windows.find(w => w.label === 'one'), two = v0.windows.find(w => w.label === 'two');
  // Window two has two panes: two (focused) and two-b on its right.
  await c.call('session.new_split', {session_id: S, anchor_block_id: two.focused_block_id, direction: 'horizontal', side: 'after', focus: false, layout: shell('two-b')});
  const twoLayout = async () => JSON.stringify((await layersOf(S, two.window_id)).layers[0].layout);
  const twoBefore = await twoLayout();
  const palette = async win => {
    const before = events('form_opened').length;
    const r = await c.call('client.action', {action: 'lazyrex_formprobe_probe', origin: 'palette', session_id: S, block_id: win.focused_block_id});
    const opened = await waitFor('form_opened', () => events('form_opened')[before]);
    return {r, opened};
  };

  // 1. A palette run in window 'one' (not the active window) opens a focused form there.
  const {r, opened} = await palette(one);
  assert.deepEqual(r.client_actions?.map(a => a.action), ['session.select'], 'the action queues session.select: ' + JSON.stringify(r));
  assert.deepEqual(r.client_actions[0].args, {session_id: S, window_id: one.window_id});
  assert.equal(opened.window_id, one.window_id);
  assert.equal(opened.source_view_id, '', 'a shell pane is no source view');
  const f1 = await waitFor('form pane', () => formBlock(S, one.window_id, opened.view_id));
  assert.equal((await layersOf(S, one.window_id)).focused_block_id, f1.block_id, 'the form has focus');
  // The app draws floating layers translucent: a form is a split on the right of the source pane.
  const w1 = await layersOf(S, one.window_id);
  assert.equal(w1.layers.length, 1, 'no floating layer');
  assert.deepEqual(w1.layers[0].layout?.split && [w1.layers[0].layout.split.direction, w1.layers[0].layout.split.before.block_id, w1.layers[0].layout.split.after.block_id],
    ['horizontal', one.focused_block_id, f1.block_id], 'the form is on the right of the source pane: ' + JSON.stringify(w1.layers[0].layout));
  assert(!w1.zoomed_block_id, 'no zoom');
  // The app gives focus back to the palette's pane (inferred): the hub focuses the form again.
  await c.call('session.focus_block', {session_id: S, block_id: one.focused_block_id});
  await waitFor('form focused again', async () => (await layersOf(S, one.window_id)).focused_block_id === f1.block_id, 3000);
  await waitFor('form renders its context', () => shows(S, f1.block_id, `probe-form-1 context: session=${S} window=${one.window_id} source= route=`));
  await sleep(1500);
  assert.equal(viewRow(opened.view_id), 'open|', 'the tab reconcile does not retire a form');

  // 2. {form} from a form: the second form opens with the same context; the first closes.
  press(S, f1.block_id, 'enter'); // a form opens with focus on its first control
  const second = await waitFor('second form', () => events('form_opened').find(e => e.route === '/forms/second'));
  assert.equal(second.window_id, one.window_id);
  await waitFor('first form closed', () => viewRow(opened.view_id).startsWith('closed|hub'));
  assert(!(await formBlock(S, one.window_id, opened.view_id)), 'the first form pane is gone');
  const f2 = await waitFor('second form pane', () => formBlock(S, one.window_id, second.view_id));
  await waitFor('second form renders the same context', () => shows(S, f2.block_id, `probe-form-2 context: session=${S} window=${one.window_id} source= route=`));

  // 3. {close_form}: the form closes; the window keeps its shell.
  press(S, f2.block_id, 'enter');
  await waitFor('second form closed', () => viewRow(second.view_id).startsWith('closed|hub'));
  await waitFor('second form pane gone', async () => !(await formBlock(S, one.window_id, second.view_id)));
  const w3 = await layersOf(S, one.window_id);
  assert.deepEqual(w3.layers[0].blocks.map(b => b.label), ['one'], 'the window has only its shell again');

  // 3b. Effects that fail leave the form open and record the failure for the renderer.
  const ops = id => execFileSync('sqlite3', ['-readonly', `${LAB}/int/hub.db`, `SELECT status || '|' || effect_state || '|' || effect_error FROM view_ops WHERE view_id = '${id}' ORDER BY rowid`], {encoding: 'utf8'}).trim().split('\n').filter(Boolean);
  const x = await palette(two);
  const fx = await waitFor('form for failures', () => formBlock(S, two.window_id, x.opened.view_id));
  await waitFor('rendered', () => shows(S, fx.block_id, 'bad navigation'));
  press(S, fx.block_id, 'tab', 'enter');
  await waitFor('bad form route fails', () => ops(x.opened.view_id).some(o => /^succeeded\|failed\|.*must start with \/forms\//.test(o)));
  press(S, fx.block_id, 'tab', 'enter');
  await waitFor('bad navigation fails', () => ops(x.opened.view_id).some(o => /^succeeded\|failed\|navigate no-such-tab/.test(o)));
  await sleep(500);
  assert.equal(viewRow(x.opened.view_id), 'open|', 'a failed effect keeps the form open');
  press(S, fx.block_id, 'ctrl+q');
  await waitFor('failure form exits', () => viewRow(x.opened.view_id).startsWith('closed|'));
  // Chained and closed forms recorded their effects as done.
  assert(ops(opened.view_id).every(o => o.startsWith('succeeded|done|')), 'chain effects done: ' + ops(opened.view_id));

  // 4. Ctrl+Q exits the renderer: the pane closes itself; no crash count.
  const q = await palette(two);
  const fq = await waitFor('form in two', () => formBlock(S, two.window_id, q.opened.view_id));
  await waitFor('rendered', () => shows(S, fq.block_id, 'probe-form-1'));
  press(S, fq.block_id, 'ctrl+q');
  await waitFor('exit closes the form', () => /^closed\|(exited|user)$/.test(viewRow(q.opened.view_id)));
  await waitFor('pane gone after exit', async () => !(await formBlock(S, two.window_id, q.opened.view_id)));
  assert(!events('view_failed').some(e => e.tab === '@form'), 'no crash accounting for forms');

  // 4b. A form in a window with two panes: the user moves to the other pane; the hub leaves it there.
  const m = await palette(two);
  const fm = await waitFor('form in two', () => formBlock(S, two.window_id, m.opened.view_id));
  const twoB = (await layersOf(S, two.window_id)).layers[0].blocks.find(b => b.label === 'two-b').block_id;
  await c.call('session.focus_block', {session_id: S, block_id: twoB});
  await sleep(2000);
  assert.equal((await layersOf(S, two.window_id)).focused_block_id, twoB, 'the hub does not take focus from another pane');
  press(S, fm.block_id, 'ctrl+q');
  await waitFor('form in two gone', async () => !(await formBlock(S, two.window_id, m.opened.view_id)));
  assert.equal(await twoLayout(), twoBefore, 'window two has its two panes as before');

  // 4b2. The source pane has focus again, but the user is on another tab: the hub leaves them there.
  const t = await palette(one);
  const fo = await waitFor('form in one', () => formBlock(S, one.window_id, t.opened.view_id));
  await c.call('session.focus_block', {session_id: S, block_id: one.focused_block_id});
  await c.call('session.focus_window', {session_id: S, window_id: two.window_id});
  await sleep(2500);
  assert.equal((await c.call('session.view', {session_id: S})).active_window_id, two.window_id, 'the hub does not pull the user back to the form tab');
  press(S, fo.block_id, 'ctrl+q');
  await waitFor('form in one gone', async () => !(await formBlock(S, one.window_id, t.opened.view_id)));

  // 4c. A zoomed source pane: the form must not open hidden behind the zoom.
  await c.call('session.zoom_block', {session_id: S, block_id: one.focused_block_id, zoom: true});
  const z = await palette(one);
  const fz = await waitFor('form from a zoomed pane', () => formBlock(S, one.window_id, z.opened.view_id));
  const wz = await layersOf(S, one.window_id);
  console.log('zoomed source: after the open, zoomed_block_id =', wz.zoomed_block_id ?? 'none', '(source', one.focused_block_id + ', form', fz.block_id + ')');
  assert.notEqual(wz.zoomed_block_id, one.focused_block_id, 'the form is not hidden behind the zoomed source');
  assert.equal(wz.focused_block_id, fz.block_id, 'the form has focus');
  press(S, fz.block_id, 'ctrl+q');
  await waitFor('zoomed-source form gone', async () => !(await formBlock(S, one.window_id, z.opened.view_id)));
  console.log('zoomed source: after the close, zoomed_block_id =', (await layersOf(S, one.window_id)).zoomed_block_id ?? 'none');

  // 5. The user closes the form pane.
  const u = await palette(two);
  const fu = await waitFor('form for user close', () => formBlock(S, two.window_id, u.opened.view_id));
  await c.call('block.close', {session_id: S, block_id: fu.block_id});
  await waitFor('user close ends the form', () => viewRow(u.opened.view_id) === 'closed|user');

  // 6. A form opened from a tab: the tab's view is the source.
  await hub('hub.ui.navigate', {plugin: 'formprobe', tab: 'home'});
  const home = await waitFor('home view', async () => {
    const d = await hub('hub.ui.describe', {plugin: 'formprobe'});
    const v = d.views?.find(v => v.tab === 'home' && v.status === 'open' && v.window_id && v.block_id);
    return v && {...v, session_id: d.session_id};
  });
  await c.call('session.attach', {session_id: home.session_id, event_scope: 'session'});
  await waitFor('home rendered', () => shows(home.session_id, home.block_id, 'probe-home'));
  const n = events('form_opened').length;
  press(home.session_id, home.block_id, 'tab', 'enter');
  const fromTab = await waitFor('form from a tab', () => events('form_opened')[n]);
  assert.equal(fromTab.source_view_id, home.view_id, 'the tab view is the source');
  // Compare with the window that Rex shows the source pane in now.
  const liveWin = (await c.call('session.view', {session_id: home.session_id})).windows
    .find(w => w.layers.some(l => l.blocks.some(b => b.block_id === home.block_id)))?.window_id;
  assert.equal(fromTab.window_id, liveWin, 'the form opens in the source pane\'s window');
  home.window_id = liveWin;
  const ft = await waitFor('form layer in the tab', () => formBlock(home.session_id, home.window_id, fromTab.view_id));
  await waitFor('context names the source', () => shows(home.session_id, ft.block_id, `source=${home.view_id} route=/`))
    .catch(e => { console.log('screen:', screen(home.session_id, ft.block_id), JSON.stringify(home)); throw e; });
  // A palette run while a form has focus is refused: one form at a time per pane.
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.probe', ctx: {client_id: me, session_id: home.session_id, block_id: ft.block_id}}), /close the open form/);
  press(home.session_id, ft.block_id, 'ctrl+q');
  await waitFor('tab form exits', () => viewRow(fromTab.view_id).startsWith('closed|'));
  assert.equal((await hub('hub.ui.describe', {plugin: 'formprobe'})).views.find(v => v.view_id === home.view_id)?.status, 'open', 'the source tab stays open');

  // 7. Bad calls.
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.nope', ctx: {client_id: me, session_id: S}}), /unknown command/);
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.probe', ctx: {client_id: me}}), /needs a session/);
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.probe', ctx: {client_id: me, session_id: S, block_id: home.block_id}}), /not shown in session/);
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.probe', ctx: {client_id: hubId, session_id: S}}), /not the calling client/);
  assert.match(await hubErr('hub.palette.run', {command: 'formprobe.probe', ctx: {session_id: S}}), /not the calling client/);
  console.log('PASS: palette forms');
} finally {
  await c.call('session.destroy', {session_id: S});
  c.close();
}
