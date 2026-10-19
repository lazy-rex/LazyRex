// Lab: an agent in a tab the user never named is named after its terminal title, without the
// leading status glyph, and the name follows the title (identity.go paneName). Lab only:
// /tmp/rexlab-pal with the hub on int.json and /tmp/rexlab-pal/fake-agent.sh.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import assert from 'node:assert/strict';
const LAB = process.env.PALETTE_LAB || '/tmp/rexlab-pal', SOCK = `unix://${LAB}/rex.sock`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFor = async (what, f, ms = 15000) => { for (let t = 0; t < ms; t += 150) { const v = await f(); if (v) return v; await sleep(150); } assert.fail(`timed out: ${what}`); };
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const R = Date.now().toString(36), name = 'title' + R;
const S = (await c.call('session.create', {label: 'pane-names-' + R})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
try {
  // A tab without a label: Rex gives it "Window " + 8 characters of its ID.
  const w = await c.call('session.new_window', {session_id: S, focus: false,
    layout: {block: {flavor: 'com.superlogical.terminal.shell', label: name, options: {command: [`${LAB}/fake-agent.sh`, name]}}}});
  await waitFor('agent ready', () => fs.existsSync(`${LAB}/out/ready-${name}`));
  const run = line => fs.writeFileSync(`${LAB}/out/fifo-${name}`, line + '\n');
  const title = t => run(`printf '\\033]2;%s\\007' '${t}' > /dev/tty`);
  title('✳ First title');
  run(`rex -S ${SOCK} --autostart=false -C ${hubId} do hub.identity.report --args '{"kind":"claude","conversation_id":"${name}","source":"startup","block":"${w.block_ids[0]}"}' > ${LAB}/out/rep-${name} 2>&1`);
  const id = `claude:${name}`;
  const nameOf = async () => (await hub('hub.agents', {all: true})).agents.find(a => a.id === id)?.name;
  await waitFor('named after the title', async () => (await nameOf()) === 'First title');
  // A status glyph change does not rename; a new title does.
  title('⠂ First title');
  await sleep(1500);
  assert.equal(await nameOf(), 'First title', 'a glyph change keeps the name');
  title('✳ Second title');
  await waitFor('the name follows the title', async () => (await nameOf()) === 'Second title');
  // A tab label the user set wins over the title.
  await c.call('session.set_window_label', {session_id: S, window_id: w.window_id, label: 'Named tab'});
  await waitFor('the tab label wins', async () => (await nameOf()) === 'Named tab');
  title('✳ Third title');
  await sleep(2000);
  assert.equal(await nameOf(), 'Named tab', 'a title change does not override the user label');
  console.log('PASS: pane names');
} finally {
  await c.call('session.destroy', {session_id: S}).catch(() => {});
  c.close();
}
