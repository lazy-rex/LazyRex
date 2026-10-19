// Lab: session renames (hometabs.go sessionRenamed, mayLabel). Lab only: /tmp/rexlab-pal with the
// hub on int.json and the formprobe plugin (its lab-only formprobe.declare action). Four phases:
//   node session-rename-lab.mjs 1   restart the hub      node session-rename-lab.mjs 2
//   stop the hub                    node session-rename-lab.mjs 3
//   start the hub                   node session-rename-lab.mjs 4
// 1: a home renamed twice in quick succession and closed at once comes back with the last name;
//    the user renames the LazyRex session twice in quick succession.
// 2: after the restart, the LazyRex session keeps the user's last name.
// 3: with the hub stopped, the user renames the LazyRex session again.
// 4: after the start, the hub keeps that name too; the lab session gets its name back.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.PALETTE_LAB || '/tmp/rexlab-pal', STATE = `${LAB}/session-rename.json`;
const phase = process.argv[2];
assert(['1', '2', '3', '4'].includes(phase), 'usage: session-rename-lab.mjs 1|2|3|4');
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFor = async (what, f, ms = 15000) => { for (let t = 0; t < ms; t += 150) { const v = await f(); if (v) return v; await sleep(150); } assert.fail(`timed out: ${what}`); };
const client = async label => (await c.call('client.list')).clients.filter(x => x.info?.label === label && x.connection_state === 'connected').at(-1)?.client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: await client('lazyrex-hub'), action, args, timeout_ms: 10000})).result;
const declare = async args => c.call('client.perform', {client_id: await client('lazyrex-plugin:formprobe'), action: 'formprobe.declare', args, timeout_ms: 10000});
const homed = async () => (await hub('hub.ui.describe', {plugin: 'formprobe'})).views.find(v => v.tab === 'homed' && v.status === 'open');
const labelOf = async s => (await c.call('session.list')).sessions.find(x => x.session_id === s)?.label;
const rename = async (s, label) => { await c.call('session.attach', {session_id: s, event_scope: 'session'}); await c.call('session.set_label', {session_id: s, label}); };
const shell = label => ({window_label: label, layout: {block: {flavor: 'com.superlogical.terminal.shell', options: {command: ['/bin/sleep', '600'], login: false}}}});

if (phase === '1') {
  const R = Date.now().toString(36);
  // A home renamed twice in quick succession, then closed: it comes back with the last name.
  const H = (await c.call('session.create', {label: 'rename-0-' + R, initial_windows: [shell('x')]})).session_id;
  await c.call('session.attach', {session_id: H, event_scope: 'session'});
  await declare({session_id: H, revision: 'r1'});
  await waitFor('homed in H', async () => (await homed())?.session_id === H);
  await rename(H, 'rename-1-' + R);
  await rename(H, 'rename-2-' + R);
  await sleep(300); // the events reach the hub; no reconcile pass is needed
  await c.call('session.destroy', {session_id: H});
  await waitFor('closed with its session', async () => !(await homed()));
  await declare({session_id: H, revision: 'r2'});
  const v = await waitFor('homed in a new session', async () => { const x = await homed(); return x && x.session_id !== H && x; }, 20000);
  assert.equal(await labelOf(v.session_id), 'rename-2-' + R, 'the replacement has the last name');
  await declare({});
  await c.call('session.attach', {session_id: v.session_id, event_scope: 'session'});
  await c.call('session.destroy', {session_id: v.session_id});

  // The user renames the LazyRex session twice in quick succession.
  const L = (await c.call('session.list')).sessions.find(x => x.label === 'LazyRex')?.session_id;
  assert(L, 'the lab has a LazyRex session');
  await rename(L, 'My LazyRex A ' + R);
  await rename(L, 'My LazyRex B ' + R);
  await waitFor('recorded as the user\'s name', () => fs.readFileSync(`${LAB}/int/events.jsonl`, 'utf8').includes(`"label":"My LazyRex B ${R}"`));
  fs.writeFileSync(STATE, JSON.stringify({L, R}));
  console.log('PASS: phase 1. Restart the hub, then run phase 2.');
} else {
  const {L, R} = JSON.parse(fs.readFileSync(STATE, 'utf8'));
  if (phase === '2') {
    await sleep(3000);
    assert.equal(await labelOf(L), 'My LazyRex B ' + R, 'the hub keeps the user\'s last name after a restart');
    console.log('PASS: phase 2. Stop the hub, then run phase 3.');
  } else if (phase === '3') {
    assert(!(await client('lazyrex-hub')), 'phase 3 needs the hub stopped');
    // Precondition: the hub labeled the session last and holds no user name for it, so only the
    // comparison of the current label with the hub's last label can keep the rename (lab DB write).
    execFileSync('sqlite3', [`${LAB}/int/hub.db`, `UPDATE ui_session_names SET hub_label = 'LazyRex', user_label = '' WHERE session_id = '${L}'`]);
    await rename(L, 'Renamed while stopped ' + R);
    console.log('PASS: phase 3. Start the hub, then run phase 4.');
  } else {
    await waitFor('hub up', () => client('lazyrex-hub'));
    await sleep(3000);
    assert.equal(await labelOf(L), 'Renamed while stopped ' + R, 'a rename while the hub was stopped is kept');
    await rename(L, 'LazyRex'); // lab hygiene, as the user
    console.log('PASS: phase 4');
  }
}
c.close();
