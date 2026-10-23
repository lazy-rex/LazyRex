// Lab: side panels end to end (panels.go, view/panels.go), with the real renderer and the real
// messaging and worktrees plugins. Lab only: /tmp/rexlab-ot, hub on int.json with the plugins
// lazyrex, messaging and worktrees copied into /tmp/rexlab-ot/plugins. Phases:
//   node panels-lab.mjs 1            cases A, B, E
//   node panels-lab.mjs 2            case C, part 1: a dirty panel waits for the user's choice
//   restart the hub, then:  node panels-lab.mjs 3   case C, part 2: the choice after the restart
//   node panels-lab.mjs 4            case F (needs the lab plugin tests/keepprobe.lua in int.json)
// A: a clean panel is replaced by a form; the form's cancel brings the panel back in the same area.
// B: a dirty panel asks the user. Keep editing keeps the panel and its input; Discard replaces it.
// E: the user closes a dirty panel while it asks: the replacement ends (user_closed), no new pane.
// C: the hub restarts while the renderer asks; after the restart the renderer still asks, and
//    Discard completes the replacement.
// F: a form that replaced a panel is submitted while another of its nodes has unsaved input: the
//    return asks. Keep editing keeps the form; the user restores the note and submits again; a new
//    return row (a new ID) brings the panel back (Codex's review of fe64d25).
// The unknown split outcome (late adoption, superseded late cleanup, user close after an
// unanswered split) and the occupied-slot / no-focus return are in hub/cmd/hub/panels_lab_test.go
// (HARNESS_LAB=1; slotSplit loses the answer on demand). Stale resume is in the view tests.
import {connectControl} from '../lib/rex-client.mjs';
import {execFileSync} from 'node:child_process';
import fs from 'node:fs';
import assert from 'node:assert/strict';
const LAB = process.env.OT_LAB || '/tmp/rexlab-ot', SOCK = `unix://${LAB}/rex.sock`, STATE = `${LAB}/panels-lab.json`;
const phase = process.argv[2];
assert(['1', '2', '3', '4'].includes(phase), 'usage: panels-lab.mjs 1|2|3|4');
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFor = async (what, f, ms = 20000) => { for (let t = 0; t < ms; t += 200) { const v = await f(); if (v) return v; await sleep(200); } assert.fail(`timed out: ${what}`); };
const client = async label => (await c.call('client.list')).clients.filter(x => x.info?.label === label && x.connection_state === 'connected').at(-1)?.client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: await client('lazyrex-hub'), action, args, timeout_ms: 10000})).result;
const rex = (...a) => execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', ...a], {encoding: 'utf8'});
const text = (s, b) => { try { return rex('capture', '-s', s, '-b', b); } catch { return ''; } };
const press = (s, b, ...keys) => { for (const k of keys) { rex('send-key', '-s', s, '-b', b, k); execFileSync('sleep', ['0.2']); } };
const sql = q => execFileSync('sqlite3', [`${LAB}/int/hub.db`, q], {encoding: 'utf8'}).trim();
const attach = s => c.call('session.attach', {session_id: s, event_scope: 'session'}).catch(() => {});
const sview = async s => { await attach(s); return c.call('session.view', {session_id: s}); };
// side is the form pane of window w: {block_id, label, view} or undefined. At most one may exist.
const side = async (s, w) => {
  const win = (await sview(s)).windows.find(x => x.window_id === w);
  const forms = win.layers.flatMap(l => l.blocks).filter(b => b.label.startsWith('lazyrex-form:'));
  assert(forms.length <= 1, `one side pane at most, got ${forms.map(f => f.label)}`);
  return forms[0] && {...forms[0], view: forms[0].label.slice('lazyrex-form:'.length)};
};
const route = id => sql(`SELECT route FROM views WHERE view_id = '${id}'`);
// palette runs a command. A side view whose renderer has not reattached yet (after a hub restart)
// refuses its release; the test waits for the reattach, as a user would retry.
const palette = async (s, b, plugin, cmd) => {
  await attach(s);
  for (let i = 0; ; i++) {
    try { return await c.call('client.action', {action: `lazyrex_${plugin}_${cmd}`, origin: 'palette', session_id: s, block_id: b}); }
    catch (e) { if (i > 20 || !/renderer is not attached/.test(e.message)) throw e; await sleep(500); }
  }
};
const shows = async (s, b, t) => waitFor(`"${t}" on screen`, () => text(s, b).includes(t));
const lines = (s, b) => text(s, b).split('\n');
// focusTo presses Tab until the focused control (marked with ▏) includes t.
const focusTo = async (s, b, t) => {
  for (let i = 0; i < 30; i++) { if (lines(s, b).some(l => /^\s*▏ /.test(l) && l.includes(t))) return; press(s, b, 'tab'); await sleep(500); }
  assert.fail(`no focus on ${t}:\n${text(s, b)}`);
};
const lastRow = () => sql(`SELECT repl_id || '|' || state FROM ui_replacements ORDER BY created DESC, rowid DESC LIMIT 1`).split('|');

const home = (await hub('hub.ui.describe', {plugin: 'lazyrex'})).views.find(v => v.tab === 'home' && v.status === 'open' && v.block_id);
assert(home, 'the lab has an open Home');
const S = home.session_id, H = home.block_id;
const W = (await sview(S)).windows.find(w => w.layers.some(l => l.blocks.some(b => b.block_id === H))).window_id;
const clear = async () => { const p = await side(S, W); if (p) { rex('block', 'close', '-s', S, p.block_id); await waitFor('slot empty', async () => !(await side(S, W))); } };
// openThreads opens the threads panel from Home and makes it dirty when dirty is set.
const openThreads = async dirty => {
  await palette(S, H, 'messaging', 'threads');
  const p = await waitFor('threads panel', async () => { const x = await side(S, W); return x && route(x.view) === '/panels/threads' && x; });
  await shows(S, p.block_id, 'Conversations');
  if (dirty) {
    const mark = 'dirty' + Date.now().toString(36).slice(-4);
    for (let i = 0; i < 5 && !text(S, p.block_id).includes(mark); i++) { press(S, p.block_id, 'ctrl+n'); await sleep(400); rex('send', '-s', S, '-b', p.block_id, mark); await sleep(600); }
    await shows(S, p.block_id, mark);
    return {...p, mark};
  }
  return p;
};

if (phase === '1') {
  await clear();
  // A: clean panel -> form -> the form's cancel brings the panel back in the same area.
  const p0 = await openThreads(false);
  await palette(S, p0.block_id, 'messaging', 'new_thread');
  const f = await waitFor('form replaces the panel', async () => { const x = await side(S, W); return x && route(x.view) === '/forms/new-thread' && x; });
  await shows(S, f.block_id, 'Create thread');
  for (let i = 0; i < 5 && (await side(S, W))?.view === f.view; i++) { press(S, f.block_id, 'escape'); await sleep(1500); }
  const back = await waitFor('the panel returns', async () => { const x = await side(S, W); return x && route(x.view) === '/panels/threads' && x; });
  assert.notEqual(back.view, p0.view, 'a new panel view');
  console.log('PASS A: clean panel -> form -> cancel -> panel');

  // B: dirty panel. Keep editing keeps it and its input; Discard replaces it.
  await clear();
  const p1 = await openThreads(true);
  await palette(S, p1.block_id, 'worktrees', 'worktrees');
  await shows(S, p1.block_id, 'Replace this panel');
  assert.equal(lastRow()[1], 'confirming');
  press(S, p1.block_id, 'escape'); // Esc = Keep editing
  await waitFor('cancelled', () => lastRow()[1] === 'cancelled');
  await waitFor('prompt gone', () => !text(S, p1.block_id).includes('Replace this panel'));
  assert((await side(S, W)).view === p1.view && text(S, p1.block_id).includes(p1.mark), 'Keep editing keeps the panel and its input');
  await palette(S, p1.block_id, 'worktrees', 'worktrees');
  await shows(S, p1.block_id, 'Replace this panel');
  for (let i = 0; i < 4 && !/>\s*Discard and replace/.test(text(S, p1.block_id)); i++) { press(S, p1.block_id, 'tab'); await sleep(400); }
  press(S, p1.block_id, 'enter');
  await waitFor('worktrees panel replaces it', async () => { const x = await side(S, W); return x && route(x.view) === '/panels/worktrees'; });
  assert.equal(lastRow()[1], 'done');
  console.log('PASS B: dirty panel: Keep editing, then Discard and replace');

  // E: the user closes a dirty panel while it asks.
  await clear();
  const p2 = await openThreads(true);
  await palette(S, p2.block_id, 'worktrees', 'worktrees');
  await shows(S, p2.block_id, 'Replace this panel');
  const [id] = lastRow();
  rex('block', 'close', '-s', S, p2.block_id);
  await waitFor('user_closed', () => sql(`SELECT state FROM ui_replacements WHERE repl_id = '${id}'`) === 'user_closed');
  await sleep(2000);
  assert(!(await side(S, W)), 'no new pane after the user closed the panel');
  console.log('PASS E: user close while asking: user_closed, no new pane');
} else if (phase === '2') {
  await clear();
  const p = await openThreads(true);
  await palette(S, p.block_id, 'worktrees', 'worktrees');
  await shows(S, p.block_id, 'Replace this panel');
  const [id, state] = lastRow();
  assert.equal(state, 'confirming');
  fs.writeFileSync(STATE, JSON.stringify({id, block: p.block_id, view: p.view, mark: p.mark}));
  console.log('PASS: phase 2. Restart the hub, then run phase 3.');
} else if (phase === '4') {
  await clear();
  await palette(S, H, 'keepprobe', 'kp');
  const p = await waitFor('probe panel', async () => { const x = await side(S, W); return x && route(x.view) === '/panels/kp' && x; });
  await shows(S, p.block_id, 'kp-panel');
  // The panel's one control is a button; Enter runs it (Tab moves focus to it if it lacks it).
  let f;
  for (let i = 0; i < 6 && !f; i++) {
    press(S, p.block_id, i % 2 ? 'tab' : 'enter'); await sleep(1500);
    const x = await side(S, W); if (x && route(x.view) === '/forms/two') f = x;
  }
  assert(f, 'the form replaces the panel');
  await shows(S, f.block_id, 'Save note');
  // Tab order (observed): Field A, Submit one, Note, Save note; the form opens on Field A. The
  // renderer shows focus only by color, so the test checks where the text lands.
  const noteBox = () => { const l = lines(S, f.block_id), i = l.findIndex(x => x.trim() === 'Note'); return l.slice(i, i + 4).join('\n'); };
  press(S, f.block_id, 'tab', 'tab');
  rex('send', '-s', S, '-b', f.block_id, 'zz');
  await waitFor('the note is typed', () => noteBox().includes('zz'), 5000);
  press(S, f.block_id, 'shift+tab', 'enter'); // Submit one
  await shows(S, f.block_id, 'Replace this');
  const [first, st] = lastRow();
  assert.equal(st, 'confirming');
  press(S, f.block_id, 'escape'); // Keep editing
  await waitFor('kept', () => sql(`SELECT state || kept FROM ui_replacements WHERE repl_id = '${first}'`) === 'cancelled1');
  await waitFor('prompt gone', () => !text(S, f.block_id).includes('Replace this'));
  assert.equal((await side(S, W)).view, f.view, 'Keep editing keeps the form');
  press(S, f.block_id, 'tab', 'backspace', 'backspace'); // Submit one -> Note
  await waitFor('note restored', () => !noteBox().includes('zz'), 5000);
  press(S, f.block_id, 'shift+tab', 'enter'); // Submit one
  const back = await waitFor('the panel returns', async () => { const x = await side(S, W); return x && route(x.view) === '/panels/kp' && x; });
  const [second, st2] = lastRow();
  assert.notEqual(second, first, 'a new submit gets a new return row');
  assert.equal(st2, 'done');
  assert.notEqual(back.view, p.view);
  console.log('PASS F: Keep editing, then a new submit returns to the panel');
} else {
  const {id, block, view} = JSON.parse(fs.readFileSync(STATE, 'utf8'));
  await waitFor('hub up', () => client('lazyrex-hub'));
  await waitFor('renderer reattached', () => sql(`SELECT attachment != '' FROM views WHERE view_id = '${view}' AND status = 'open'`) === '1');
  assert.equal(sql(`SELECT state FROM ui_replacements WHERE repl_id = '${id}'`), 'confirming', 'confirming survives the restart');
  await shows(S, block, 'Replace this panel');
  for (let i = 0; i < 4 && !/>\s*Discard and replace/.test(text(S, block)); i++) { press(S, block, 'tab'); await sleep(400); }
  press(S, block, 'enter');
  await waitFor('done after the restart', () => sql(`SELECT state FROM ui_replacements WHERE repl_id = '${id}'`) === 'done');
  const x = await side(S, W);
  assert(x && route(x.view) === '/panels/worktrees', 'the worktrees panel replaced it');
  console.log('PASS C: restart during confirmation, then Discard');
}
c.close();
