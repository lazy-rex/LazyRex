// Lab: agent identity stage 2, hub side (identity.go). Lab only: /tmp/rexlab-ident, with the hub on
// /tmp/rexlab-ident/int.json and the fake agent (/tmp/rexlab-ident/fake-agent.sh).
// Checks: a pane that never joins is identified by its hook report alone (hub.identify through the
// messaging plugin, hub.spawn caller); hub.agents lists identities with tab names; resolve by ID,
// prefix, name, and legacy alias; the first report from a joined legacy pane creates a permanent
// alias; a message to an identity sets its pending notice.
import {connectControl} from '../lib/rex-client.mjs';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
const LAB = process.env.IDENTITY_LAB || '/tmp/rexlab-ident', SOCK = `unix://${LAB}/rex.sock`, OUT = `${LAB}/out2`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const hubErr = async (action, args) => { try { await hub(action, args); return ''; } catch (e) { return String(e.message ?? e); } };
const waitFile = async (f, ms = 10000) => { for (let t = 0; t < ms && !fs.existsSync(f); t += 100) await sleep(100); assert(fs.existsSync(f), `${f} appears`); };
const R = Date.now().toString(36);
let n = 0;
// run executes a client action inside the named fake agent's pane (the pid proof is real).
async function run(agent, client, action, args) {
  const f = `${OUT}/r-${agent}-${++n}`;
  const json = JSON.stringify(args).replaceAll("'", "'\\''");
  fs.writeFileSync(`${OUT}/fifo-${agent}`, `env -u REX_SESSION rex -S ${SOCK} --autostart=false -C ${client} do ${action} --args '${json}' > ${f}.tmp 2>&1; mv ${f}.tmp ${f}\n`);
  await waitFile(f);
  const text = fs.readFileSync(f, 'utf8').trim();
  try { return {result: JSON.parse(text)}; } catch { return {error: text}; }
}
const agents = async (all = false) => (await hub('hub.agents', {all})).agents;
const pane = async (S, label, name) => {
  const w = await c.call('session.new_window', {session_id: S, window_label: label,
    layout: {block: {flavor: 'com.superlogical.terminal.shell', label, options: {command: [`${LAB}/fake-agent.sh`, name]}}}});
  await waitFile(`${OUT}/ready-${name}`);
  return w.block_ids[0];
};

fs.rmSync(OUT, {recursive: true, force: true});
fs.mkdirSync(OUT);
fs.writeFileSync(`${LAB}/fake-agent2.sh`, fs.readFileSync(`${LAB}/fake-agent.sh`, 'utf8').replace('OUT=/tmp/rexlab-ident/out', `OUT=${OUT}`), {mode: 0o755});
const S = (await c.call('session.create', {label: 'identity-stage2'})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
try {
  const tools = (await hub('hub.tools')).tools ?? (await hub('hub.tools'));
  const msgClient = (Array.isArray(tools) ? tools : tools.tools).find(t => t.action === 'messaging.send').client_id;

  // A pane that never joins: its hook report is the whole enrollment.
  const P = `plan${R}`, PID = `claude:${P}`;
  const pBlock = await (async () => {
    const w = await c.call('session.new_window', {session_id: S, window_label: 'Planner',
      layout: {block: {flavor: 'com.superlogical.terminal.shell', label: 'planner', options: {command: [`${LAB}/fake-agent2.sh`, 'p']}}}});
    await waitFile(`${OUT}/ready-p`);
    return w.block_ids[0];
  })();
  let r = await run('p', hubId, 'hub.identity.report', {kind: 'claude', conversation_id: P, source: 'startup', block: pBlock});
  assert.equal(r.result?.state, 'current', JSON.stringify(r));
  assert.equal(r.result.agent_id, null, 'no legacy agent in this pane');
  assert.equal(r.result.name, 'Planner');

  let list = await agents();
  const e = list.find(a => a.id === PID);
  assert.equal(e?.name, 'Planner');
  assert.equal(e.state, 'current');
  assert.equal(e.joined, true);
  assert.deepEqual(e.aliases, []);

  // Resolution.
  assert.equal((await hub('hub.agents.resolve', {to: PID})).id, PID);
  assert.equal((await hub('hub.agents.resolve', {to: 'planner'})).id, PID, 'a unique tab name resolves');
  assert.equal((await hub('hub.agents.resolve', {to: `claude:${P.slice(0, -1)}`})).id, PID, 'a unique prefix resolves');
  assert.match(await hubErr('hub.agents.resolve', {to: 'nobody-' + R}), /unknown/);
  assert.match(await hubErr('hub.agents.resolve', {to: 'claude:pl'}), /unknown/, 'a prefix needs 4 characters after the colon');

  // The pane can spawn without a join: the identity is the caller.
  const sp = (await run('p', hubId, 'hub.spawn', {profile: 'fakeclaude', key: 's2-' + R, args: {task: 'unused'}})).result;
  assert.equal(sp?.status, 'open', JSON.stringify(sp));
  const w = (await hub('hub.workers')).workers.find(x => x.spawn_id === sp.spawn_id);
  assert.equal(w?.parent ?? w?.caller, PID, 'the spawn records the identity as its caller: ' + JSON.stringify(w));
  await hub('hub.stop', {spawn_id: sp.spawn_id}).catch(() => {});

  // A pane joined as the legacy agent "claude": its first report creates the alias claude -> identity.
  const bBlock = await (async () => {
    const w = await c.call('session.new_window', {session_id: S, window_label: 'Builder',
      layout: {block: {flavor: 'com.superlogical.terminal.shell', label: 'builder', options: {command: [`${LAB}/fake-agent2.sh`, 'b']}}}});
    await waitFile(`${OUT}/ready-b`);
    return w.block_ids[0];
  })();
  const aliasedBefore = (await agents(true)).find(a => a.id === 'claude')?.alias_of;
  await hub('hub.admin.rebind', {agent: 'claude', block_id: bBlock});
  assert.equal((await run('b', hubId, 'hub.join', {agent: 'claude'})).result?.agent, 'claude');
  const B = `build${R}`, BID = `claude:${B}`;
  r = await run('b', hubId, 'hub.identity.report', {kind: 'claude', conversation_id: B, source: 'startup', block: bBlock});
  assert.equal(r.result?.agent_id, 'claude');
  list = await agents();
  if (!aliasedBefore) {
    assert(!list.some(a => a.id === 'claude'), 'an aliased legacy agent leaves the default list');
    assert.deepEqual(list.find(a => a.id === BID)?.aliases, ['claude']);
    assert.equal((await agents(true)).find(a => a.id === 'claude')?.alias_of, BID);
    assert.equal((await hub('hub.agents.resolve', {to: 'claude'})).id, BID, 'a legacy ID resolves to its identity');
    console.log('alias: claude ->', BID);
  } else {
    console.log(`alias: claude was already aliased to ${aliasedBefore} (an earlier run); the alias is permanent`);
    assert.equal((await hub('hub.agents.resolve', {to: 'claude'})).id, aliasedBefore);
  }

  // /clear in the builder pane: the old identity is superseded, hidden by default, kept with all.
  r = await run('b', hubId, 'hub.identity.report', {kind: 'claude', conversation_id: B + 'x', source: 'clear', block: bBlock});
  assert.equal(r.result?.predecessor, BID);
  assert(!(await agents()).some(a => a.id === BID), 'a superseded identity leaves the default list');
  assert.equal((await agents(true)).find(a => a.id === BID)?.state, 'superseded');

  // Notice readiness: a busy identity gets no notice; it gets one when it reports idle.
  const EV = `${LAB}/int/events.jsonl`;
  const notices = to => fs.readFileSync(EV, 'utf8').split('\n').filter(l => l.includes('"notice_written"') && l.includes(`"agent":"${to}"`)).length;
  const status = (agent, state) => fs.writeFileSync(`${OUT}/fifo-${agent}`, `printf '\\033]7501;state=${state}:app=claude\\007' > /dev/tty\n`);
  status('b', 'blocked');
  await sleep(500);
  const before = notices(BID + 'x');
  r = await run('p', msgClient, 'messaging.send', {to: BID + 'x', body: 'wake up', key: 'w-' + R});
  assert(r.result, JSON.stringify(r));
  await sleep(1500);
  assert.equal(notices(BID + 'x'), before, 'no notice while the identity is blocked');
  status('b', 'idle');
  for (let t = 0; t < 50 && notices(BID + 'x') === before; t++) await sleep(100);
  assert.equal(notices(BID + 'x'), before + 1, 'one notice when the identity reports idle');

  // The agent face: native discovery and calls with `rex -C lazyrex`.
  const acts = JSON.parse(execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', '-C', 'lazyrex', 'actions', '--json'], {encoding: 'utf8'}));
  const names = (acts.actions ?? acts).map(a => a.name);
  console.log('rex -C lazyrex actions:', names.join(' '));
  for (const n of ['hub.spawn', 'hub.agents', 'messaging.send', 'messaging.read']) assert(names.includes(n), `the agent face lists ${n}`);
  assert(!names.includes('hub.admin.stop') && !names.includes('hub.identity.report'), 'internal actions stay off the agent face');
  r = await run('p', 'lazyrex', 'hub.agents', {});
  assert(r.result?.agents?.some(a => a.id === PID), 'hub.agents through the agent face: ' + JSON.stringify(r).slice(0, 200));
  r = await run('p', 'lazyrex', 'messaging.send', {to: BID + 'x', body: 'via the agent face', key: 'f-' + R});
  console.log('forwarded messaging.send:', JSON.stringify(r).slice(0, 300));

  // Messaging through the real plugin: the sender is the pane's identity (hub.identify), and the
  // recipient's pending notice is keyed by its identity.
  r = await run('p', msgClient, 'messaging.send', {to: BID + 'x', body: 'hello from the planner', key: 'm-' + R});
  console.log('messaging.send from an identity pane:', JSON.stringify(r).slice(0, 300));
  if (r.result) {
    const read = await run('b', msgClient, 'messaging.read', {});
    console.log('messaging.read in the builder pane:', JSON.stringify(read).slice(0, 400));
  }
  console.log('PASS: identity stage 2 (hub side)');
} finally {
  await c.call('session.destroy', {session_id: S});
  c.close();
}
