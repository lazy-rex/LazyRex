// Lab: renaming a message thread by renaming its Rex tab (tabrename.go + messaging view.rename).
// Lab only: /tmp/rexlab-ident, hub on /tmp/rexlab-ident/int.json, fake agent fake-agent.sh.
// Checks: a user rename reaches the plugin and sticks, literally ("Review (2)"); labels the hub
// writes (unread counts) never become custom titles; Rex's generated label is ignored.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.IDENTITY_LAB || '/tmp/rexlab-ident', SOCK = `unix://${LAB}/rex.sock`, OUT = `${LAB}/out3`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const waitFor = async (what, f, ms = 10000) => { for (let t = 0; t < ms; t += 100) { const v = await f(); if (v) return v; await sleep(100); } assert.fail(`timed out: ${what}`); };
const R = Date.now().toString(36);
const EV = `${LAB}/int/events.jsonl`;
const renames = () => fs.readFileSync(EV, 'utf8').split('\n').filter(l => l.includes('"event":"tab_rename"'));
let n = 0;
async function run(action, args) {
  const f = `${OUT}/r-${++n}`;
  fs.writeFileSync(`${OUT}/fifo-q`, `env -u REX_SESSION rex -S ${SOCK} --autostart=false -C lazyrex do ${action} --args '${JSON.stringify(args)}' > ${f}.tmp 2>&1; mv ${f}.tmp ${f}\n`);
  await waitFor(f, () => fs.existsSync(f));
  return fs.readFileSync(f, 'utf8');
}

fs.rmSync(OUT, {recursive: true, force: true});
fs.mkdirSync(OUT);
fs.writeFileSync(`${LAB}/fake-agent3.sh`, fs.readFileSync(`${LAB}/fake-agent.sh`, 'utf8').replace('OUT=/tmp/rexlab-ident/out', `OUT=${OUT}`), {mode: 0o755});
const S = (await c.call('session.create', {label: 'tab-rename'})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
try {
  const w = await c.call('session.new_window', {session_id: S, window_label: 'Quill',
    layout: {block: {flavor: 'com.superlogical.terminal.shell', label: 'q', options: {command: [`${LAB}/fake-agent3.sh`, 'q']}}}});
  await waitFor('agent ready', () => fs.existsSync(`${OUT}/ready-q`));
  const Q = `claude:quill${R}`;
  fs.writeFileSync(`${OUT}/fifo-q`, `env -u REX_SESSION rex -S ${SOCK} --autostart=false -C ${hubId} do hub.identity.report --args '{"kind":"claude","conversation_id":"quill${R}","source":"startup","block":"${w.block_ids[0]}"}' > ${OUT}/rep 2>&1\n`);
  await waitFor('report', () => fs.existsSync(`${OUT}/rep`) && fs.readFileSync(`${OUT}/rep`, 'utf8').includes('current'));
  console.log(await run('messaging.send', {to: 'user', body: 'first', key: 'a-' + R}));

  const tabKey = `thread/${Q}`;
  const title = async () => execFileSync('sqlite3', ['-readonly', `${LAB}/int/hub.db`,
    `SELECT title FROM ui_tabs WHERE plugin = 'messaging' AND tab = '${tabKey}'`], {encoding: 'utf8'}).trim();
  const t0 = await waitFor('thread tab declared', title);
  console.log('declared title:', t0);
  const before = renames().length;
  await hub('hub.ui.navigate', {plugin: 'messaging', tab: tabKey});
  const view = await waitFor('view open', async () => {
    const d = await hub('hub.ui.describe', {plugin: 'messaging'});
    const v = d.views?.find(v => v.tab === tabKey && v.status === 'open' && v.window_id);
    return v && {...v, session_id: d.session_id};
  });
  const sess = view.session_id, win = view.window_id;
  await c.call('session.attach', {session_id: sess, event_scope: 'session'});
  const label = async () => (await c.call('session.view', {session_id: sess})).windows.find(x => x.window_id === win)?.label;
  await sleep(1500); // seen clears the unread count: the hub writes a new label
  assert.equal(renames().length, before, 'labels the hub writes are not renames: ' + await label());

  // The user renames the tab, with a literal "(2)".
  await c.call('session.set_window_label', {session_id: sess, window_id: win, label: 'Review (2)'});
  // The plugin keeps the literal title; it adds its unread count after it while mail is unread.
  await waitFor('plugin title', async () => /^Review \(2\)( \(\d+\))?$/.test(await title()));
  await sleep(1000);
  assert.equal(await label(), await title(), 'the tab shows the declared title');
  assert(String(await label()).startsWith('Review (2)'), 'the tab keeps the user title');
  assert.equal(renames().length, before + 1);

  // A new unread message: the hub writes the count label; it must not become the custom title.
  console.log(await run('messaging.send', {to: 'user', body: 'second', key: 'b-' + R}));
  await sleep(2000);
  console.log('after unread:', await title(), '|', await label());
  assert(String(await title()).startsWith('Review (2)'), 'the custom title stays');
  assert.equal(renames().length, before + 1, 'the hub-written unread label is not a rename');

  // A -> B -> A: a user rename back to an earlier title is still a rename.
  const base = /^(.*?)( \(\d+\))?$/;
  const plain = async () => (await title()).match(base)[1];
  for (const t of ['Alpha', 'Beta', 'Alpha']) {
    await c.call('session.set_window_label', {session_id: sess, window_id: win, label: t});
    await waitFor(`title ${t}`, async () => (await plain()) === t);
  }
  const afterABA = renames().length;
  assert.equal(afterABA, before + 4, 'A -> B -> A gives three renames');

  // Rapid renames reach the plugin in order; the last one wins.
  for (const t of ['R1', 'R2', 'R3']) await c.call('session.set_window_label', {session_id: sess, window_id: win, label: t});
  await waitFor('title R3', async () => (await plain()) === 'R3');
  await sleep(1500);
  assert.equal(await plain(), 'R3', 'the last rename wins');
  const rapid = renames().slice(afterABA).map(l => JSON.parse(l).title);
  assert.deepEqual(rapid.filter(t => /^R\d$/.test(t)), ['R1', 'R2', 'R3'], 'renames arrive in order: ' + rapid);
  assert(String(await label()).startsWith('R3'), 'the tab shows R3');
  const settled = renames().length;

  // Rex's generated label (an empty label) is not a rename; the declared title comes back.
  await c.call('session.set_window_label', {session_id: sess, window_id: win, label: ''});
  await waitFor('declared title restored', async () => String(await label()).startsWith('R3'));
  assert.equal(renames().length, settled, 'the generated label is not a rename');

  // That restore was a hub write (its echo is consumed). Another user title, then the hub's label
  // again: the second is still a user rename.
  const hubLabel = await label();
  await c.call('session.set_window_label', {session_id: sess, window_id: win, label: 'Other'});
  await waitFor('title Other', async () => (await plain()) === 'Other');
  await c.call('session.set_window_label', {session_id: sess, window_id: win, label: hubLabel});
  await waitFor(`title ${hubLabel} again`, async () => (await title()).startsWith(hubLabel));
  const last = renames().slice(settled).map(l => JSON.parse(l).title);
  assert.deepEqual(last, ['Other', hubLabel], 'hub label -> user title -> hub label: two renames; got ' + last);
  console.log('PASS: tab rename');
} finally {
  await c.call('session.destroy', {session_id: S});
  c.close();
}
