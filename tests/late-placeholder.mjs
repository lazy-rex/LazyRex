// Lab: restore placeholders whose new_window outcome was unknown are closed when they appear late,
// after their cycle completed (appmode.go closeLatePlaceholders). Lab only: /tmp/rexlab-s3ui, with
// the hub's state at /tmp/rexlab-s3ui/int/hub.db (dev.sh on the lab config).
// Records: a proven placeholder (closed, record deleted), a pane with only the label (kept, record
// kept), and a record whose session is gone (record deleted). The user pane stays. The late panes
// appear while the hub is idle: a Rex layout event, not a hub call, must wake the cleanup.
import {connectControl} from '../lib/rex-client.mjs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const DB = '/tmp/rexlab-s3ui/int/hub.db';
const c = await connectControl('/tmp/rexlab-s3ui/rex.sock');
const sql = q => execFileSync('sqlite3', [DB, q], {encoding: 'utf8'}).trim();
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const run = Date.now().toString(36);
const [proven, unproven, gone] = ['ph-late-a-' + run, 'ph-late-b-' + run, 'ph-late-c-' + run];
const S = (await c.call('session.create', {label: 'late-placeholder'})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
const pane = (label, command) => ({block: {flavor: 'com.superlogical.terminal.shell', label, options: {command, login: false}}});
const loop = 'while :; do sleep 600; done';
// The user pane exists first. The records exist before any placeholder (an unknown outcome).
const u = await c.call('session.new_window', {session_id: S, window_label: 'user', layout: pane('user pane', ['/bin/sh', '-c', loop])});
const at = new Date().toISOString();
sql(`INSERT INTO ui_restores VALUES ('${proven}', 'hidden:cycle-done:${S}:0', '${at}'), ('${unproven}', 'hidden:cycle-done:${S}:1', '${at}'),
  ('${gone}', 'hidden:cycle-done:session:00000000-0000-7000-8000-000000000000:0', '${at}')`);
try {
  // One pass registers the records: the hub attaches to the session, and drops the gone record.
  await hub('hub.ui.navigate', {plugin: 'lazyrex', tab: 'home'});
  for (let i = 0; i < 50 && sql(`SELECT COUNT(*) FROM ui_restores WHERE nonce = '${gone}'`) !== '0'; i++) await sleep(100);
  assert.equal(sql(`SELECT COUNT(*) FROM ui_restores WHERE nonce = '${gone}'`), '0', 'a record whose session is gone goes');
  await sleep(7000); // the hub goes idle: no pass, no retry pending
  // The late panes appear with no navigation or other call to the hub. Only Rex events wake it.
  const w = await c.call('session.new_window', {session_id: S, window_label: 'late', focus: false,
    layout: pane('lazyrex-placeholder:' + proven, ['/bin/sh', '-c', loop, 'lazyrex-placeholder', proven])});
  const w2 = await c.call('session.new_window', {session_id: S, window_label: 'label only', focus: false,
    layout: pane('lazyrex-placeholder:' + unproven, ['/bin/sh', '-c', loop])});
  const t0 = Date.now();
  for (let i = 0; i < 150 && sql(`SELECT COUNT(*) FROM ui_restores WHERE nonce = '${proven}'`) !== '0'; i++) await sleep(100);
  console.log(`late placeholder handled in ${Date.now() - t0} ms`);
  const blocks = (await c.call('session.list_blocks', {session_id: S})).blocks.map(b => b.block_id);
  assert(!blocks.includes(w.block_ids[0]), 'the proven late placeholder is closed');
  assert(blocks.includes(u.block_ids[0]), 'the user pane stays');
  assert(blocks.includes(w2.block_ids[0]), 'a pane with only the label stays');
  assert.equal(sql(`SELECT COUNT(*) FROM ui_restores WHERE nonce = '${proven}'`), '0', 'the closed placeholder record goes');
  assert.equal(sql(`SELECT COUNT(*) FROM ui_restores WHERE nonce = '${unproven}'`), '1', 'an unproven record stays');
  console.log('PASS: with the hub idle, a late proven placeholder wakes cleanup and closes; a label-only pane and its record stay; a gone session drops its record');
} finally {
  sql(`DELETE FROM ui_restores WHERE nonce IN ('${proven}', '${unproven}', '${gone}')`);
  await c.call('session.destroy', {session_id: S});
  c.close();
}
