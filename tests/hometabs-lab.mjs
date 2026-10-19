// Lab: plugin tabs in a home session (hometabs.go). Lab only: /tmp/rexlab-pal with the hub on
// int.json and the formprobe plugin (tests/formprobe.lua, its lab-only formprobe.declare action).
// Checks: a tab with session_id opens in that session, after the user's tabs, without reordering
// them; a user close keeps it closed for the same revision and a new revision reopens it; a
// navigation shows it; a closed home session sends the tab to the app session; dropping the home
// moves it back.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.PALETTE_LAB || '/tmp/rexlab-pal';
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const clients = (await c.call('client.list')).clients.filter(x => x.connection_state === 'connected');
const hubId = clients.filter(x => x.info?.label === 'lazyrex-hub').at(-1).client_id;
const probeId = clients.filter(x => x.info?.label === 'lazyrex-plugin:formprobe').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const declare = args => c.call('client.perform', {client_id: probeId, action: 'formprobe.declare', args, timeout_ms: 10000});
const waitFor = async (what, f, ms = 10000) => { for (let t = 0; t < ms; t += 100) { const v = await f(); if (v) return v; await sleep(100); } assert.fail(`timed out: ${what}`); };
const startAt = new Date().toISOString();
const events = name => fs.readFileSync(`${LAB}/int/events.jsonl`, 'utf8').split('\n').filter(l => l.includes(`"event":"${name}"`)).map(l => JSON.parse(l)).filter(e => e.at >= startAt);
const homed = async () => (await hub('hub.ui.describe', {plugin: 'formprobe'})).views.find(v => v.tab === 'homed' && v.status === 'open');
const labels = async s => (await c.call('session.view', {session_id: s})).windows.map(w => w.label);
const shell = label => ({window_label: label, layout: {block: {flavor: 'com.superlogical.terminal.shell', options: {command: ['/bin/sleep', '600'], login: false}}}});

const H = (await c.call('session.create', {label: 'home-tabs', initial_windows: [shell('b-first'), shell('a-second')]})).session_id;
await c.call('session.attach', {session_id: H, event_scope: 'session'});
const cleanup = [];
try {
  // 1. A home tab opens in its home session, after the user's tabs, which keep their order.
  await declare({session_id: H, revision: 'r1'});
  const v1 = await waitFor('homed view in H', async () => { const v = await homed(); return v?.session_id === H && v.window_id && v; });
  await waitFor('tab titled', async () => (await labels(H)).includes('Homed'));
  assert.deepEqual((await labels(H)).slice(0, 2), ['b-first', 'a-second'], 'the user\'s tabs keep their order');
  await sleep(1500);
  assert.deepEqual((await labels(H)).slice(0, 2), ['b-first', 'a-second'], 'no reorder in a later pass');

  // 2. The user closes it: it stays closed for the same revision; a new revision reopens it.
  await c.call('block.close', {session_id: H, block_id: v1.block_id});
  await waitFor('closed', async () => !(await homed()));
  await declare({session_id: H, revision: 'r1'});
  await sleep(2000);
  assert.equal(await homed(), undefined, 'same revision: stays closed');
  await declare({session_id: H, revision: 'r2'});
  const v2 = await waitFor('reopened in H', async () => { const v = await homed(); return v?.session_id === H && v.window_id && v; });
  assert(events('tab_reopened').some(e => e.tab === 'homed' && e.revision === 'r2'));
  const active = async () => (await c.call('session.view', {session_id: H})).active_window_id;
  assert.notEqual(await active(), v2.window_id, 'a reopen does not take focus');

  // 3. A navigation shows it.
  await hub('hub.ui.navigate', {plugin: 'formprobe', tab: 'homed'});
  await waitFor('navigation focuses the home tab', async () => (await active()) === v2.window_id);

  // 4. Dropping the home moves the tab to the app session; the view in H goes.
  const app = (await hub('hub.ui.describe', {plugin: 'formprobe'})).default_session_id;
  await declare({session_id: '', revision: 'r2'});
  await waitFor('moved to the app session', async () => { const v = await homed(); return v?.session_id === app && v; }, 20000);
  await waitFor('gone from H', async () => !(await labels(H)).includes('Homed'), 20000);

  // 5. A home session that is gone: the tab stays in the app session.
  const G = (await c.call('session.create', {label: 'gone-home', initial_windows: [shell('x')]})).session_id;
  await c.call('session.attach', {session_id: G, event_scope: 'session'});
  await c.call('session.destroy', {session_id: G});
  await declare({session_id: G, revision: 'r3'});
  await sleep(2000);
  assert.equal((await homed())?.session_id, app, 'a missing home falls back to the app session');
  // 6. A home that was alive, then destroyed: the tab closes with it; a new revision brings back
  // a session with the same name, and the tab lives there (the maintainer).
  const NAME = 'recreate-' + Date.now().toString(36);
  const H2 = (await c.call('session.create', {label: NAME, initial_windows: [shell('y')]})).session_id;
  await c.call('session.attach', {session_id: H2, event_scope: 'session'});
  await declare({session_id: H2, revision: 'r4'});
  await waitFor('homed in H2', async () => (await homed())?.session_id === H2);
  await c.call('session.destroy', {session_id: H2});
  await waitFor('closed with its session', async () => !(await homed()));
  await sleep(1500);
  assert.equal(await homed(), undefined, 'a closed tab in a gone home creates nothing');
  await declare({session_id: H2, revision: 'r5'});
  const v6 = await waitFor('homed in a new session', async () => { const v = await homed(); return v && v.session_id !== H2 && v.session_id !== app && v; }, 20000);
  const again = (await c.call('session.list')).sessions.find(x => x.session_id === v6.session_id);
  assert.equal(again?.label, NAME, 'the new session has the home\'s name');
  assert.equal((await hub('hub.ui.describe', {plugin: 'formprobe'})).home_redirects?.[H2], v6.session_id, 'describe reports the redirect');
  await c.call('session.attach', {session_id: v6.session_id, event_scope: 'session'});
  cleanup.push(v6.session_id);

  // 7. In the replacement session the tab is at home (Codex review of b3d5465): a renderer exit
  // counts as a crash, not a legacy retirement; a user close keeps it closed for the same revision.
  const exits = events('view_exited').length;
  execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', `unix://${LAB}/rex.sock`, '--autostart=false',
    'send-key', '-s', v6.session_id, '-b', v6.block_id, 'ctrl+q']);
  const exited = await waitFor('renderer exit', () => events('view_exited').slice(exits).find(e => e.view_id === v6.view_id));
  assert(!exited.legacy && exited.crashes >= 1, 'an exit at home counts a crash: ' + JSON.stringify(exited));
  const v7 = await waitFor('tab opened again in the replacement', async () => { const v = await homed(); return v?.view_id !== v6.view_id && v?.session_id === v6.session_id && v.block_id && v; }, 20000);
  await c.call('block.close', {session_id: v7.session_id, block_id: v7.block_id});
  const closed = await waitFor('user close', () => events('view_closed').find(e => e.view_id === v7.view_id));
  assert(!closed.legacy, 'a close at home is a user close: ' + JSON.stringify(closed));
  await declare({session_id: H2, revision: 'r5'});
  await sleep(2000);
  assert.equal(await homed(), undefined, 'same revision: stays closed in the replacement');
  await declare({session_id: H2, revision: 'r6'});
  await waitFor('new revision reopens in the replacement', async () => (await homed())?.session_id === v6.session_id, 20000);
  console.log('PASS: home tabs');
} finally {
  await declare({}).catch(() => {});
  for (const x of [H, ...cleanup]) await c.call('session.destroy', {session_id: x}).catch(() => {});
  c.close();
}
