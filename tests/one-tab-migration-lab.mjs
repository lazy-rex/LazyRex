// Lab: the one-tab migration (every plugin in the LazyRex session, app mode retired). Lab only:
// /tmp/rexlab-ot, with the hub on int.json (plugins lazyrex and messaging, copied into
// /tmp/rexlab-ot/plugins) and the binaries in /tmp/rexlab-ot/{old,new}. Two phases:
//   with the OLD hub, view and plugins:   node one-tab-migration-lab.mjs 1
//   stop the hub, install the NEW ones, start the hub, then:   node one-tab-migration-lab.mjs 2
//   then:   node one-tab-migration-lab.mjs 3   (new build: status, menu, Ctrl+Q)
//   then:   node one-tab-migration-lab.mjs 4, restart the hub, node one-tab-migration-lab.mjs 5
//           (Move to session with a draft to a new session; an agent's thread defaults to its session)
//   then:   node one-tab-migration-lab.mjs 6   (New Thread from the palette into another open session)
// Phase 1 makes the live state: app mode active (a user session hidden), threads in the Messages
// app session, a saved draft in one thread, a user close of another thread.
// Phase 2: the new hub removes the app-mode state at start (the hidden user session stays
// detached: docs/hub.md, "Removed: app mode"); the threads open in
// the LazyRex session with the draft; the closed thread stays closed; the old app session stays;
// the Home tab stays first; the renderer writes the declared status (OSC 7501, app=rex) and a
// view close (hub.view.close) clears nothing else and keeps the tab closed.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.OT_LAB || '/tmp/rexlab-ot', SOCK = `unix://${LAB}/rex.sock`, STATE = `${LAB}/one-tab.json`;
const phase = process.argv[2];
assert(['1', '2', '3', '4', '5', '6'].includes(phase), 'usage: one-tab-migration-lab.mjs 1|2|3|4|5|6');
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFor = async (what, f, ms = 20000) => { for (let t = 0; t < ms; t += 150) { const v = await f(); if (v) return v; await sleep(150); } assert.fail(`timed out: ${what}`); };
const client = async label => (await c.call('client.list')).clients.filter(x => x.info?.label === label && x.connection_state === 'connected').at(-1)?.client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: await client('lazyrex-hub'), action, args, timeout_ms: 10000})).result;
const rex = (...a) => execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', ...a], {encoding: 'utf8'});
const screen = (s, b) => { try { return rex('capture', '-s', s, '-b', b).replace(/\s+/g, ''); } catch { return ''; } };
const press = (s, b, ...keys) => { for (const k of keys) { rex('send-key', '-s', s, '-b', b, k); execFileSync('sleep', ['0.15']); } };
const pick = async (s, b, label) => {
  press(s, b, 'enter');
  await waitFor('select opens', () => screen(s, b).includes('✓'));
  rex('send', '-s', s, '-b', b, label); await sleep(250);
  press(s, b, 'enter');
  await waitFor(`${label} chosen in the closed field`, () => screen(s, b).includes((label+' ▾').replace(/\s+/g,'')) && !screen(s, b).includes('✓'));
};
const describe = p => hub('hub.ui.describe', {plugin: p});
const view = async (p, tab) => (await describe(p)).views.find(v => v.tab === tab && v.status === 'open' && v.block_id && v.attached);
const attach = s => c.call('session.attach', {session_id: s, event_scope: 'session'}).catch(() => {});
const sview = async s => { await attach(s); return c.call('session.view', {session_id: s}); };
const labels = async s => (await sview(s)).windows.map(w => w.label);
const sessions = async () => (await c.call('session.list')).sessions;
const shell = label => ({window_label: label, layout: {block: {flavor: 'com.superlogical.terminal.shell', options: {command: ['/bin/sleep', '3600'], login: false}}}});
const formBlocks = async s => (await sview(s)).windows.flatMap(x => x.layers.flatMap(l => l.blocks)).filter(b => b.label.startsWith('lazyrex-form:'));
const lines = (s, b) => { try { return rex('capture', '-s', s, '-b', b).split('\n'); } catch { return []; } };
// focusTo presses Tab until the focused control (the renderer marks it with ▏) includes text.
// Keys are sometimes lost (roadmap, known defects), so it checks the screen after each key.
const focusTo = async (s, b, text) => {
  for (let i = 0; i < 30; i++) {
    if (lines(s, b).some(l => /^\s*▏ /.test(l) && l.includes(text))) return;
    press(s, b, 'tab'); await sleep(500);
  }
  assert.fail(`no focus on ${text}:\n${lines(s, b).join('\n')}`);
};
// openMenu presses Ctrl+O in a thread until its menu form opens, and returns the form pane.
const openMenu = async (s, b) => {
  const old = new Set((await formBlocks(s)).map(x => x.block_id));
  for (let i = 0; i < 5; i++) {
    press(s, b, 'ctrl+o');
    for (let t = 0; t < 10; t++) { const f = (await formBlocks(s)).find(x => !old.has(x.block_id)); if (f) { await waitFor('menu rendered', () => lines(s, f.block_id).some(l => l.includes('Move to session'))); return f.block_id; } await sleep(300); }
  }
  assert.fail('no menu form');
};
// startAgent runs a fake agent in a new session and reports its identity; run(cmd, out) runs a
// command in its pane.
const startAgent = async R => {
  const alpha = 'alpha' + R;
  const A = (await c.call('session.create', {label: 'agent-' + R, initial_windows: [shell('x')]})).session_id;
  await attach(A);
  const w = await c.call('session.new_window', {session_id: A, window_label: 'Alpha', focus: false,
    layout: {block: {flavor: 'com.superlogical.terminal.shell', label: alpha, options: {command: [`${LAB}/fake-agent.sh`, alpha]}}}});
  await waitFor('agent ready', () => fs.existsSync(`${LAB}/out/ready-${alpha}`));
  const run = (cmd, out) => fs.writeFileSync(`${LAB}/out/fifo-${alpha}`, `${cmd} > ${LAB}/out/${out} 2>&1\n`);
  run(`rex -S ${SOCK} --autostart=false -C ${await client('lazyrex-hub')} do hub.identity.report --args '{"kind":"claude","conversation_id":"${alpha}","source":"startup","block":"${w.block_ids[0]}"}'`, 'rep-' + alpha);
  await waitFor('identity reported', () => fs.existsSync(`${LAB}/out/rep-${alpha}`) && fs.readFileSync(`${LAB}/out/rep-${alpha}`, 'utf8').includes('current'));
  return {alpha, A, run};
};
const status = async (s, b) => { await attach(s); return (await c.call('com.superlogical.terminal.program_status', {session_id: s, block_id: b, args: {}})).records ?? []; };

if (phase === '1') {
  const R = Date.now().toString(36);
  const W = (await c.call('session.create', {label: 'work-' + R, initial_windows: [shell('w-one'), shell('w-two')]})).session_id;
  await c.call('session.attach', {session_id: W, event_scope: 'session'});
  // The old hub enters app mode on a navigation to a plugin other than Home.
  await hub('hub.ui.navigate', {plugin: 'messaging', tab: 'thread/claude'});
  await waitFor('app mode active', async () => (await hub('hub.ui.mode')).phase === 'active');
  const appSess = (await describe('messaging')).session_id;
  assert(appSess, 'the old hub made a Messages app session');
  const v = await waitFor('thread/claude open', () => view('messaging', 'thread/claude'));
  const vc = await waitFor('thread/codex open', () => view('messaging', 'thread/codex'));
  assert.equal(v.session_id, appSess);
  // A saved draft in thread/claude.
  const DRAFT = 'otdraft' + R;
  await waitFor('thread rendered', () => screen(v.session_id, v.block_id).includes('Ctrl+Qquit'));
  // Keys right after a Tab are sometimes lost (roadmap, known defects): Tab and type again until
  // the composer shows the draft.
  for (let i = 0; i < 4 && !screen(v.session_id, v.block_id).includes(DRAFT); i++) {
    press(v.session_id, v.block_id, 'tab');
    await sleep(600);
    press(v.session_id, v.block_id, ...DRAFT);
    await sleep(1500);
  }
  assert(screen(v.session_id, v.block_id).includes(DRAFT), 'draft typed');
  await sleep(3000); // the renderer saves the draft (hub.view.draft.save)
  // A user close of thread/codex.
  await c.call('session.attach', {session_id: vc.session_id, event_scope: 'session'});
  await c.call('block.close', {session_id: vc.session_id, block_id: vc.block_id});
  await waitFor('thread/codex closed', async () => (await describe('messaging')).tabs.find(t => t.tab === 'thread/codex')?.suppressed);
  fs.writeFileSync(STATE, JSON.stringify({R, W, appSess, DRAFT}));
  console.log('PASS: phase 1. Stop the hub, install the new build, start it, then run phase 2.');
} else if (phase === '3') {
  const R = Date.now().toString(36);
  const lazy = (await sessions()).find(s => s.label === 'LazyRex').session_id;
  // 1. A message from an agent: the Messages tab declares an unread status; its renderer writes it.
  const {alpha, A, run} = await startAgent(R);
  const inbox = await waitFor('Messages home in LazyRex', async () => { const x = await view('messaging', 'home'); return x?.session_id === lazy && x; });
  // Baseline: no unread. The lab has no app to focus a pane, so send each thread pane the terminal
  // focus-in report (CSI I), as the app would.
  for (const x of (await describe('messaging')).views.filter(x => x.status === 'open' && x.tab.startsWith('thread/') && x.block_id)) {
    await attach(x.session_id);
    rex('send', '-s', x.session_id, '-b', x.block_id, '\x1b[I');
  }
  await waitFor('no unread', async () => !(await status(lazy, inbox.block_id)).some(r => r.state === 'done'));
  run(`rex -S ${SOCK} --autostart=false -C lazyrex do messaging.send --args '{"to":"user","body":"status please","key":"st-${R}"}'`, 'send-' + alpha);
  const rec = await waitFor('unread status record', async () => (await status(lazy, inbox.block_id)).find(r => r.state === 'done'));
  console.log('inbox record:', JSON.stringify(rec));
  assert.equal(rec.app, 'rex', 'app=rex');
  assert.match(rec.msg ?? '', /unread/);
  // 2. Reading the thread clears it.
  const tab = `thread/claude:${alpha}`;
  await hub('hub.ui.navigate', {plugin: 'messaging', tab});
  const tv = await waitFor('thread open', async () => { const x = await view('messaging', tab); return x?.session_id && x; });
  console.log('thread session:', tv.session_id === lazy ? 'LazyRex' : tv.session_id === A ? 'agent session' : tv.session_id);
  // The lab has no app to focus the pane: send the terminal focus-in report (CSI I) as the app would.
  await waitFor('thread rendered', () => /Ctrl+O/.test(screen(tv.session_id, tv.block_id)));
  const unread = async () => +(((await status(lazy, inbox.block_id)).find(r => r.state === 'done')?.msg ?? '0').match(/\d+/)?.[0] ?? 0);
  const n0 = await unread();
  assert((await status(tv.session_id, tv.block_id)).some(r => r.state === 'done'), 'the unread thread reports done');
  rex('send', '-s', tv.session_id, '-b', tv.block_id, '\x1b[I');
  await waitFor('thread status cleared after reading', async () => !(await status(tv.session_id, tv.block_id)).length, 20000);
  await waitFor('inbox count down by one', async () => (await unread()) === n0 - 1, 20000);
  // 3. Ctrl+O opens the thread menu as a form in the side pane; the form reports blocked/question.
  const fm = {block_id: await openMenu(tv.session_id, tv.block_id)};
  const fr = await waitFor('form status', async () => (await status(tv.session_id, fm.block_id)).find(r => r.state === 'blocked'));
  console.log('form record:', JSON.stringify(fr));
  assert.equal(fr.kind, 'question');
  assert(/Move/.test(screen(tv.session_id, fm.block_id)), 'the menu lists Move to session: ' + screen(tv.session_id, fm.block_id));
  press(tv.session_id, fm.block_id, 'ctrl+q');
  await waitFor('menu closed', async () => !(await formBlocks(tv.session_id)).length);
  // 4. Ctrl+Q closes the thread as the user: it stays closed.
  press(tv.session_id, tv.block_id, 'ctrl+q');
  await waitFor('closed by the user', async () => (await describe('messaging')).tabs.find(t => t.tab === tab)?.suppressed);
  await sleep(3000);
  assert.equal(await view('messaging', tab), undefined, 'Ctrl+Q: the thread does not reopen');
  await c.call('session.destroy', {session_id: A});
  console.log('PASS: phase 3');
} else if (phase === '4') {
  const {DRAFT} = JSON.parse(fs.readFileSync(STATE, 'utf8'));
  const R = Date.now().toString(36), NEW = 'moved' + R; // lowercase: send-key sends a capital as lowercase (lab)
  // 1. Move to session: thread/claude (with the saved draft) to a new session.
  const v = await waitFor('thread/claude open', () => view('messaging', 'thread/claude'));
  for (const f of await formBlocks(v.session_id)) await c.call('block.close', {session_id: v.session_id, block_id: f.block_id}); // an earlier failed run
  await waitFor('no forms', async () => !(await formBlocks(v.session_id)).length);
  const menu = await openMenu(v.session_id, v.block_id);
  await focusTo(v.session_id, menu, 'Move to session');
  press(v.session_id, menu, 'enter');
  // The menu closes and the move form opens in its own pane.
  const form = await waitFor('move form', async () => { for (const f of await formBlocks(v.session_id)) if (lines(v.session_id, f.block_id).some(l => l.includes('Move thread'))) return f.block_id; });
  await pick(v.session_id, form, 'New session…');
  press(v.session_id, form, 'tab'); await sleep(600);
  for (let i = 0; i < 3 && !screen(v.session_id, form).includes(NEW.replace(/\s/g, '')); i++) { press(v.session_id, form, ...NEW.replace(' ', '')); await sleep(800); }
  const typed = NEW.replace(' ', '');
  assert(screen(v.session_id, form).includes(typed), 'name typed: ' + lines(v.session_id, form).join('\n'));
  // Buttons have no ▏ mark: from the name field, one Tab reaches Move thread (lab, this form).
  press(v.session_id, form, 'tab'); await sleep(800);
  press(v.session_id, form, 'enter');
  const moved = await waitFor('moved to the new session', async () => {
    const x = await view('messaging', 'thread/claude');
    const label = x && (await sessions()).find(s => s.session_id === x.session_id)?.label;
    return label === typed && x;
  }, 30000);
  await waitFor('draft kept after the move', () => screen(moved.session_id, moved.block_id).includes(DRAFT));
  // 2. An agent's new thread defaults to the agent's own session (hub.identify session_id).
  const {A, run} = await startAgent(R);
  run(`rex -S ${SOCK} --autostart=false -C lazyrex do messaging.thread_create --args '{"members":["user","codex"],"title":"Agent ${R}","key":"tc-${R}"}'`, 'tc-' + R);
  await waitFor('thread_create answered', () => fs.existsSync(`${LAB}/out/tc-${R}`) && fs.readFileSync(`${LAB}/out/tc-${R}`, 'utf8').length > 0);
  console.log('thread_create:', fs.readFileSync(`${LAB}/out/tc-${R}`, 'utf8').trim());
  const tab = await waitFor('agent thread declared', async () => (await describe('messaging')).tabs.find(t => t.session_id === A)?.tab);
  const av = await waitFor('agent thread open in its session', async () => { const x = await view('messaging', tab); return x?.session_id === A && x; }, 30000);
  fs.writeFileSync(`${LAB}/one-tab-4.json`, JSON.stringify({moved: moved.session_id, view: moved.view_id, A, tab, DRAFT}));
  console.log('PASS: phase 4. Restart the hub, then run phase 5.');
} else if (phase === '6') {
  // New Thread from the palette in session A, with another open session (W) chosen: the group's
  // home, view and focus land in W (Codex's acceptance check).
  const {W} = JSON.parse(fs.readFileSync(STATE, 'utf8'));
  const R = Date.now().toString(36), TITLE = 'grp' + R;
  const {A} = await startAgent(R);
  const wlabel = (await sessions()).find(x => x.session_id === W).label;
  const seenTabs = new Set((await describe('messaging')).tabs.map(x => x.tab));
  const src = (await sview(A)).windows[0].focused_block_id;
  await c.call('client.action', {action: 'lazyrex_messaging_new_thread', origin: 'palette', session_id: A, block_id: src});
  const f = await waitFor('new thread form', async () => (await formBlocks(A))[0]?.block_id);
  await waitFor('form rendered', () => lines(A, f).some(l => l.includes('Session *')));
  await pick(A, f, wlabel);
  press(A, f, 'tab'); await sleep(600); press(A, f, 'tab'); await sleep(600); // name, then title
  for (let i = 0; i < 3 && !screen(A, f).includes(TITLE); i++) { press(A, f, ...TITLE); await sleep(800); }
  assert(screen(A, f).includes(TITLE), 'title typed: ' + lines(A, f).join('\n'));
  // The focused checkbox is drawn bold in the accent color (dark palette: the lab has no app).
  const focused = () => (rex('capture', '-s', A, '-b', f, '--format', 'html').match(/color: rgb\(165, 180, 252\);font-weight: bold;">([^<]*)</) || [])[1]?.trim() ?? '';
  for (let i = 0; i < 30 && !focused().includes('Alpha'); i++) { press(A, f, 'tab'); await sleep(500); }
  assert(focused().includes('Alpha'), 'focus on Alpha: ' + focused());
  press(A, f, 'space'); await sleep(500);
  for (let i = 0; i < 30 && focused(); i++) { press(A, f, 'tab'); await sleep(500); } // past the checkboxes to Create
  press(A, f, 'enter');
  const t = await waitFor('group declared with home W', async () => (await describe('messaging')).tabs.find(x => x.session_id === W && x.tab.startsWith('thread/g-') && !seenTabs.has(x.tab)), 20000);
  const v = await waitFor('group open in W', async () => { const x = await view('messaging', t.tab); return x?.session_id === W && x; }, 20000);
  await waitFor('group focused in W', async () => (await sview(W)).active_window_id === v.window_id);
  await waitFor('tab titled', async () => (await sview(W)).windows.find(x => x.window_id === v.window_id)?.label === TITLE);
  console.log('PASS: phase 6');
} else if (phase === '5') {
  const {moved, A, tab, DRAFT} = JSON.parse(fs.readFileSync(`${LAB}/one-tab-4.json`, 'utf8'));
  await waitFor('hub up', () => client('lazyrex-hub'));
  await sleep(4000);
  const v = await waitFor('thread/claude open', () => view('messaging', 'thread/claude'));
  assert.equal(v.session_id, moved, 'the moved thread stays in its new session after a restart');
  await waitFor('draft still there', () => screen(v.session_id, v.block_id).includes(DRAFT));
  assert.equal((await view('messaging', tab))?.session_id, A, 'the agent thread stays in the agent session');
  console.log('PASS: phase 5');
} else {
  const {R, W, appSess, DRAFT} = JSON.parse(fs.readFileSync(STATE, 'utf8'));
  await waitFor('hub up', () => client('lazyrex-hub'));
  // 1. App mode is removed: the hub restores nothing. The hidden user session stays detached.
  // 2. The threads open in the LazyRex session; the draft is kept.
  const lazy = (await sessions()).find(s => s.label === 'LazyRex')?.session_id;
  assert(lazy, 'the LazyRex session exists');
  const d = await describe('messaging');
  assert.equal(d.session_id, lazy, 'messaging uses the LazyRex session');
  const v = await waitFor('thread/claude in LazyRex', async () => { const x = await view('messaging', 'thread/claude'); return x?.session_id === lazy && x; }, 30000);
  await waitFor('draft kept', () => screen(lazy, v.block_id).includes(DRAFT), 15000);
  // 3. The closed thread stays closed.
  await sleep(2000);
  assert.equal(await view('messaging', 'thread/codex'), undefined, 'a closed thread stays closed');
  assert((await describe('messaging')).tabs.find(t => t.tab === 'thread/codex')?.suppressed, 'still suppressed');
  // 4. The old app session stays (it may hold user panes); no messaging view remains there.
  assert((await sessions()).some(s => s.session_id === appSess), 'the old app session is kept');
  assert(!(await describe('messaging')).views.some(x => x.status === 'open' && x.session_id === appSess), 'no view left in the old app session');
  // 5. Home stays the first tab of the LazyRex session.
  const home = await waitFor('home view', () => view('lazyrex', 'home'));
  await waitFor('home first', async () => (await sview(lazy)).windows[0]?.window_id === home.window_id);
  // 6. Status: a message to the user makes an unread count; the Messages renderer reports it.
  const inbox = await waitFor('messaging home in LazyRex', async () => { const x = await view('messaging', 'home'); return x?.session_id === lazy && x; });
  console.log('status before:', JSON.stringify(await status(lazy, inbox.block_id)));
  // 7. Ctrl+Q in a thread closes it as the user (needs the new renderer's hub.view.close).
  if (process.env.OT_CTRLQ) {
    press(lazy, v.block_id, 'ctrl+q');
    await waitFor('thread/claude closed by the user', async () => (await describe('messaging')).tabs.find(t => t.tab === 'thread/claude')?.suppressed);
    await sleep(3000);
    assert.equal(await view('messaging', 'thread/claude'), undefined, 'Ctrl+Q: the thread does not reopen');
  }
  console.log('PASS: phase 2');
}
c.close();
