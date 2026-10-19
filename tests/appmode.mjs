// Lab integration test for app mode (hub appmode.go). Lab only: /tmp/rexlab-s3ui/rex.sock, with a hub
// whose config has the plugins messaging and uitest (dev.sh on a lab config).
// 1. Builds a user session with deep splits, floating layers, zoom, focus and unsaved input.
// 2. Enter (navigate to an app): every user session is hidden; the apps' panes come back (the same
//    blocks); Home is untouched. Outside app mode every app session has no placed pane.
// 3. Switch apps: same cycle, nothing recaptured.
// 4. Exit: every user session is rebuilt exactly; the same processes and unsaved input survive.
// 5. Key rules: a retry reports the same cycle; a new key with no app mode reports none.
// 6. Problems: a pane closed and a pane moved while hidden are reported, never replaced.
import {connectControl} from '../lib/rex-client.mjs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const RUN = 'run-' + Date.now();
const SOCK = '/tmp/rexlab-s3ui/rex.sock';
const c = await connectControl(SOCK);
const lr = (...a) => execFileSync('rex', ['-S', 'unix://' + SOCK, '--autostart=false', ...a], {encoding: 'utf8'});
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const until = async (what, f, ms = 15000) => {
  const t = Date.now();
  for (;;) { const v = await f(); if (v) return v; if (Date.now() - t > ms) throw new Error('timeout: ' + what); await sleep(100); }
};
const mode = () => hub('hub.ui.mode');

const sh = {block: {flavor: 'com.superlogical.terminal.shell', options: {command: ['/bin/sh'], login: false}}};
const H = (r, b, a) => ({split: {direction: 'horizontal', ratio: r, before: b, after: a}});
const V = (r, b, a) => ({split: {direction: 'vertical', ratio: r, before: b, after: a}});
const ours = new Set(['LazyRex']);
const isOurs = l => ours.has(l) || l.startsWith('LazyRex: ');
const isApp = l => l.startsWith('LazyRex: ');
const appBlocks = async () => {
  const out = {};
  for (const s of await sessions()) if (isApp(s.label)) out[s.label] = (await c.call('session.list_blocks', {session_id: s.session_id})).blocks.map(b => b.block_id).sort();
  return out;
};

const sessions = async () => (await c.call('session.list')).sessions;
const view = async id => { await c.call('session.attach', {session_id: id, event_scope: 'session'}); return c.call('session.view', {session_id: id}); };
const tree = n => n.split ? {d: n.split.direction, r: n.split.ratio, b: tree(n.split.before), a: tree(n.split.after)} : n.block_id;
const norm = v => ({active: (v.windows ?? []).findIndex(w => w.window_id === v.active_window_id), windows: (v.windows ?? []).map(w => ({
  label: w.label, focused: w.focused_block_id, zoomed: w.zoomed_block_id || null,
  layers: w.layers.map(l => ({kind: l.kind, bounds: l.bounds, layout: l.layout ? tree(l.layout) : null}))}))});
const snapshot = async () => {
  const out = {};
  for (const s of await sessions()) out[s.label + ' ' + s.session_id.slice(-6)] = norm(await view(s.session_id));
  return out;
};

// 1. A user session with every proven feature, and unsaved input in two panes.
const S = (await c.call('session.create', {label: 'am-user'})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
const call = (m, a = {}) => c.call(m, {session_id: S, ...a});
const w1 = await call('session.new_window', {window_label: 'deep', focus: false, layout: H(0.3, sh, V(0.6, sh, H(0.4, sh, sh)))});
const w2 = await call('session.new_window', {window_label: 'floats', focus: false, layout: H(0.5, sh, H(0.5, sh, sh))});
await call('session.float_block', {block_id: w2.block_ids[2], bounds: [0.1, 0.1, 0.5, 0.5]});
const w3 = await call('session.new_window', {window_label: 'zoom', focus: false, layout: V(0.6, sh, sh)});
await call('session.focus_block', {block_id: w1.block_ids[2]});
await call('session.focus_block', {block_id: w3.block_ids[1]});
await call('session.zoom_block', {block_id: w3.block_ids[1], zoom: true});
await call('session.focus_block', {block_id: w1.block_ids[2]});
// A second session whose active tab is zoomed: the session focus after the rebuild must keep the zoom.
const Z = (await c.call('session.create', {label: 'am-zoom-active'})).session_id;
await c.call('session.attach', {session_id: Z, event_scope: 'session'});
await c.call('session.new_window', {session_id: Z, window_label: 'other', focus: false, layout: H(0.5, sh, sh)});
const z = await c.call('session.new_window', {session_id: Z, window_label: 'zoomed', layout: V(0.4, sh, sh)});
await c.call('session.zoom_block', {session_id: Z, block_id: z.block_ids[0], zoom: true});
await sleep(500);
lr('send', '-s', S, '-b', w1.block_ids[0], 'unsaved-alpha');
lr('send', '-s', S, '-b', w2.block_ids[2], 'unsaved-beta');
const pid = async b => (await c.call('com.superlogical.terminal.process', {session_id: S, block_id: b, args: {}}).catch(() => null))?.child?.pid;
const pids0 = {};
for (const b of [...w1.block_ids, ...w2.block_ids, ...w3.block_ids]) pids0[b] = await pid(b);

try {
  assert.equal((await mode()).phase !== 'active' && (await mode()).mode, 'workspace', 'starts in workspace mode');
  const before = await snapshot();
  for (const [k, v] of Object.entries(before)) if (isApp(k.slice(0, -7))) assert.equal(v.windows.length, 0, `${k} has no placed pane outside app mode`);
  const apps0 = await appBlocks();

  // 2. Enter.
  await hub('hub.ui.navigate', {plugin: 'messaging', tab: 'home'});
  const m1 = await until('active', async () => { const m = await mode(); return m.phase === 'active' && m; });
  console.log('entered: cycle', m1.cycle, 'hidden', JSON.stringify(m1.hidden));
  for (const s of await sessions()) {
    const v = await view(s.session_id);
    // Navigation selects the app's tab, so only the active tab of a LazyRex session may change.
    if (s.label === 'LazyRex') assert.deepEqual(norm(v).windows, before[s.label + ' ' + s.session_id.slice(-6)].windows, 'Home untouched');
    else if (!isOurs(s.label)) assert.equal((v.windows ?? []).length, 0, `${s.label} hidden`);
  }
  for (const [b, p] of Object.entries(pids0)) assert.equal(await pid(b), p, 'processes survive the hide');
  await until('apps mounted', async () => {
    for (const s of await sessions()) if (isApp(s.label) && !((await view(s.session_id)).windows ?? []).length) return false;
    return true;
  });
  assert.deepEqual(await appBlocks(), apps0, 'the apps come back with the same view panes');

  // 3. Switch apps: same cycle.
  await hub('hub.ui.navigate', {plugin: 'uitest', tab: 'home'});
  await sleep(600);
  assert.equal((await mode()).cycle, m1.cycle, 'switching apps keeps the cycle');

  // 4. Exit.
  const t0 = Date.now();
  const ex = await hub('hub.ui.exit', {for: 'user', key: RUN + '-exit-1'});
  assert.equal(ex.cycle, m1.cycle);
  assert.equal(ex.phase, 'restoring', 'an accepted exit is not described as done');
  const m2 = await until('complete', async () => { const m = await mode(); return m.phase === 'complete' && m; });
  console.log(`restored in ${Date.now() - t0} ms:`, JSON.stringify(m2.restored), 'problems', m2.problems.length);
  assert.deepEqual(m2.problems, []);
  const zv = norm(await view(Z));
  assert.equal(zv.windows[zv.active].label, 'zoomed', 'the zoomed tab is active again');
  assert.equal(zv.windows[zv.active].zoomed, z.block_ids[0], 'the active tab keeps its zoom after the session focus');
  assert.deepEqual(m2.restored, m1.hidden);
  const after = await snapshot();
  for (const k of Object.keys(before)) {
    if (isApp(k.slice(0, -7))) continue; // checked below, once the next pass detaches them
    if (isOurs(k.slice(0, -7))) assert.deepEqual(after[k].windows, before[k].windows, k);
    else assert.deepEqual(after[k], before[k], `${k} is back exactly`);
  }
  for (const [b, p] of Object.entries(pids0)) assert.equal(await pid(b), p, 'same processes after the restore');
  await until('apps hidden again', async () => {
    for (const s of await sessions()) if (isApp(s.label) && ((await view(s.session_id)).windows ?? []).length) return false;
    return true;
  });
  assert.deepEqual(await appBlocks(), apps0, 'Exit detaches the same app panes; nothing closes');
  const screen = b => lr('capture', '-s', S, '-b', b);
  assert.match(screen(w1.block_ids[0]), /unsaved-alpha/);
  assert.match(screen(w2.block_ids[2]), /unsaved-beta/);

  // 5. Keys.
  assert.equal((await hub('hub.ui.exit', {for: 'user', key: RUN + '-exit-1'})).cycle, m1.cycle, 'a retry reports its own cycle');
  assert.equal((await hub('hub.ui.exit', {for: 'user', key: RUN + '-exit-2'})).phase, 'none', 'nothing to exit');
  await assert.rejects(hub('hub.ui.exit', {for: 'user'}), /key is required/);

  // 6. Problems: close one hidden pane; move another into a tab the user opened while hidden;
  // close a whole hidden session.
  const D = (await c.call('session.create', {label: 'am-doomed'})).session_id;
  await c.call('session.attach', {session_id: D, event_scope: 'session'});
  await c.call('session.new_window', {session_id: D, window_label: 'doomed', layout: sh});
  await hub('hub.ui.navigate', {plugin: 'messaging', tab: 'home'});
  const m3 = await until('active again', async () => { const m = await mode(); return m.phase === 'active' && m.cycle !== m1.cycle && m; });
  const replay = await hub('hub.ui.exit', {for: 'user', key: RUN + '-exit-2'});
  assert.equal(replay.phase, 'none', 'a key bound to no cycle does not exit a later cycle');
  assert.equal((await mode()).phase, 'active');
  await c.call('block.close', {session_id: S, block_id: w1.block_ids[3]});
  await c.call('session.destroy', {session_id: D});
  const extra = await call('session.new_window', {window_label: 'made while hidden', focus: false, layout: sh});
  await call('session.move_block', {block_id: w3.block_ids[0], anchor_block_id: extra.block_ids[0]});
  const ex3 = await hub('hub.ui.exit', {for: 'user', key: RUN + '-exit-3'});
  assert.equal(ex3.cycle, m3.cycle);
  const m4 = await until('complete again', async () => { const m = await mode(); return m.phase === 'complete' && m.cycle === m3.cycle && m; });
  const codes = m4.problems.map(p => p.code).sort();
  assert.deepEqual(codes, ['closed', 'moved', 'session_closed'], JSON.stringify(m4.problems));
  const v = await view(S);
  assert.deepEqual(v.windows.map(w => w.label), ['deep', 'floats', 'zoom', 'made while hidden'], 'recorded tabs first, the new tab after');
  const deep = norm(v).windows[0].layers[0].layout;
  assert.deepEqual(deep, {d: 'horizontal', r: 0.3, a: {d: 'vertical', r: 0.6, a: w1.block_ids[2], b: w1.block_ids[1]}, b: w1.block_ids[0]},
    'the closed pane is pruned from its split; the rest keeps its ratios');
  const zoomTab = v.windows[2];
  assert.deepEqual(zoomTab.layers[0].layout.block_id, w3.block_ids[1], 'the moved pane stays where the user put it');
  const made = v.windows[3].layers[0].layout;
  assert.deepEqual([made.split.before.block_id, made.split.after.block_id], [extra.block_ids[0], w3.block_ids[0]], 'the new tab keeps the moved pane');
  console.log('problems:', m4.problems.map(p => `${p.code}: ${p.message}`).join(' | '));
  console.log('PASS: enter, switch, exact exit with processes and unsaved input, key rules, closed and moved panes, a closed session');
} finally {
  const m = await mode().catch(() => null);
  if (m && m.mode === 'app') await hub('hub.ui.exit', {for: 'user', key: 'cleanup-' + Date.now()});
  await until('cleanup restore', async () => (await mode()).mode === 'workspace').catch(e => console.log(e.message));
  await c.call('session.destroy', {session_id: S});
  await c.call('session.destroy', {session_id: Z});
  c.close();
}
