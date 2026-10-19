// Lab: agent identity stage 1 (hub/cmd/hub/identity.go). Lab only: /tmp/rexlab-ident, with the
// hub running on /tmp/rexlab-ident/int.json and the fake agent profile "fakeclaude"
// (/tmp/rexlab-ident/fake-agent.sh). Reports run inside lab panes, so the pid proof is real.
// Checks: cleaned env at spawn; startup, unchanged, clear, prompt, resume; a delayed older hook
// process is stale; an mcp report cannot revive a superseded identity; another kind in the same
// pane is a transition; not_in_pane; rename from the tab label; process exit and pane close detach.
import {connectControl} from '../lib/rex-client.mjs';
import {execFileSync} from 'node:child_process';
import fs from 'node:fs';
import assert from 'node:assert/strict';
const LAB = process.env.IDENTITY_LAB || '/tmp/rexlab-ident', SOCK = `unix://${LAB}/rex.sock`, OUT = `${LAB}/out`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const waitFile = async (f, ms = 10000) => { for (let t = 0; t < ms && !fs.existsSync(f); t += 100) await sleep(100); assert(fs.existsSync(f), `${f} appears`); };
const REX = `env -u REX_SESSION rex -S ${SOCK} --autostart=false -C ${hubId} do`;
let n = 0;
const R = Date.now().toString(36); // conversation IDs are unique per run

// run executes a hub action inside the named fake agent's pane and returns {result} or {error}.
async function run(agent, action, args, {background = false} = {}) {
  const tag = `${agent}-${++n}`, f = `${OUT}/r-${tag}`;
  const json = JSON.stringify(args).replaceAll("'", "'\\''");
  const cmd = `${REX} ${action} --args '${json}' > ${f}.tmp 2>&1; mv ${f}.tmp ${f}`;
  fs.writeFileSync(`${OUT}/fifo-${agent}`, (background ? `(${cmd}) &` : cmd) + '\n');
  if (background) return f;
  return read(f);
}
async function read(f) {
  await waitFile(f);
  const text = fs.readFileSync(f, 'utf8').trim();
  try { return {result: JSON.parse(text)}; } catch { return {error: text}; }
}
const report = (agent, conv, source, extra = {}) => run(agent, 'hub.identity.report', {kind: 'claude', conversation_id: conv, source, ...extra});
const ident = async id => (await hub('hub.identity.list')).identities.find(i => i.identity === id);

fs.rmSync(OUT, {recursive: true, force: true});
fs.mkdirSync(OUT);
const S = (await c.call('session.create', {label: 'identity-lab'})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'});
try {
  // The boot pane joins as the static agent "claude", so it may spawn.
  const boot = await c.call('session.new_window', {session_id: S, window_label: 'boot',
    layout: {block: {flavor: 'com.superlogical.terminal.shell', label: 'boot', options: {command: [`${LAB}/fake-agent.sh`, 'boot']}}}});
  await waitFile(`${OUT}/ready-boot`);
  const bootBlock = boot.block_ids[0];
  await hub('hub.admin.rebind', {agent: 'claude', block_id: bootBlock}); // a rerun: free the static agent
  assert.equal((await run('boot', 'hub.join', {agent: 'claude'})).result?.agent, 'claude');
  const sp = (await run('boot', 'hub.spawn', {profile: 'fakeclaude', key: 'id-' + Date.now(), args: {task: 'a1'}})).result;
  assert.equal(sp?.status, 'open', JSON.stringify(sp));
  await waitFile(`${OUT}/ready-a1`);
  const [, argSid] = fs.readFileSync(`${OUT}/argv-a1`, 'utf8').split('\n');
  assert.equal(argSid, '', 'the hub does not add --session-id: the hook reports the conversation');
  const sid = `start-${R}`;
  const envA1 = fs.readFileSync(`${OUT}/env-a1`, 'utf8'), envBoot = fs.readFileSync(`${OUT}/env-boot`, 'utf8');
  assert(!/^CLAUDECODE=/m.test(envA1), 'the agent pane has no CLAUDECODE');
  for (const k of ['CLAUDE_CODE_CHILD_SESSION', 'CLAUDE_CODE_SESSION_ID', 'CLAUDE_CODE_ENTRYPOINT', 'CLAUDE_PID', 'CLAUDE_CODE_MESSAGING_SOCKET'])
    assert(!new RegExp(`^${k}=`, 'm').test(envA1), `the agent pane has no ${k}`);
  console.log(`spawn: boot pane had CLAUDECODE: ${/^CLAUDECODE=/m.test(envBoot)}`);
  const A = 'claude:' + sid;
  const blk = sp.block_id, hint = {block: blk};
  let r = await report('a1', sid, 'startup', hint);
  assert.equal(r.result?.state, 'current', JSON.stringify(r));
  assert.equal(r.result.agent_id, sp.agent_id);
  assert.equal((await ident(A))?.agent_id, sp.agent_id);
  r = await report('a1', sid, 'startup', hint);
  assert.equal(r.result?.state, 'unchanged', JSON.stringify(r));
  r = await report('a1', `conv-b-${R}`, 'clear', hint);
  assert.equal(r.result?.state, 'current');
  assert.equal(r.result.predecessor, A, '/clear names the old identity as predecessor');
  assert.equal((await ident(A)).state, 'superseded');

  // A delayed hook from an older process: its sh starts first, sleeps, then execs the CLI.
  const tag = `${OUT}/r-late`;
  const late = JSON.stringify({kind: 'claude', conversation_id: `conv-late-${R}`, source: 'clear', ...hint});
  fs.writeFileSync(`${OUT}/late.sh`, `sleep 2; exec ${REX} hub.identity.report --args '${late}'\n`);
  // One line: the parent shell waits for both, so the late process keeps its ancestry (an orphan
  // is reparented to launchd and fails the pid proof).
  const cf = `${OUT}/r-newer`, newer = JSON.stringify({kind: 'claude', conversation_id: `conv-c-${R}`, source: 'prompt', ...hint});
  fs.writeFileSync(`${OUT}/fifo-a1`, `(sh ${OUT}/late.sh > ${tag}.tmp 2>&1; mv ${tag}.tmp ${tag}) & sleep 0.3; ${REX} hub.identity.report --args '${newer}' > ${cf}.tmp 2>&1; mv ${cf}.tmp ${cf}; wait\n`);
  r = await read(cf);
  assert.equal(r.result?.state, 'current', 'a prompt with an unseen ID repairs a lost /clear');
  r = await read(tag);
  assert.match(r.error ?? '', /stale/, 'the older hook process is stale: ' + JSON.stringify(r));
  assert.equal(await ident(`claude:conv-late-${R}`), undefined, 'a stale report records no identity');

  r = await report('a1', `conv-b-${R}`, 'mcp', hint);
  assert.match(r.error ?? '', /stale/, 'an mcp report cannot revive a superseded identity');
  r = await report('a1', `conv-b-${R}`, 'prompt', hint);
  assert.equal(r.result?.state, 'current', 'a newer hook report can (lost resume repair)');
  assert.equal(r.result.predecessor, A, 'a revived identity keeps its predecessor');
  assert.equal((await ident(`claude:conv-c-${R}`)).state, 'superseded');

  // Another kind in the same pane (the user quits Claude and starts Codex there) is a transition.
  r = await run('a1', 'hub.identity.report', {kind: 'codex', conversation_id: `t1-${R}`, source: 'mcp', ...hint});
  assert.equal(r.result?.state, 'current', JSON.stringify(r));
  r = await run('a1', 'hub.identity.report', {kind: 'claude', conversation_id: `x-${R}`, source: 'mcp', block: bootBlock});
  assert.equal(r.result?.state, 'current', JSON.stringify(r));
  // A wrong hint does not move the report: the ancestry decides.
  assert.equal((await ident(`claude:x-${R}`))?.block_id, blk, 'a wrong block hint is ignored');

  // The static agent's pane (not spawned): any kind; agent_id is the joined agent.
  r = await run('boot', 'hub.identity.report', {kind: 'codex', conversation_id: `thread-1-${R}`, source: 'mcp', block: bootBlock});
  assert.equal(r.result?.state, 'current');
  assert.equal(r.result.agent_id, 'claude');
  assert.equal(r.result.name, 'boot', 'the name is the tab label');

  // A caller outside any pane.
  let outside = '';
  try {
    execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', '-C', hubId, 'do', 'hub.identity.report',
      '--args', JSON.stringify({kind: 'claude', conversation_id: 'z', source: 'startup'})], {encoding: 'utf8', stdio: 'pipe'});
  } catch (e) { outside = e.stderr + e.stdout; }
  assert.match(outside, /not_in_pane/);

  // Rename the tab: window_label_changed updates the name.
  const bootWin = boot.window_id;
  await c.call('session.set_window_label', {session_id: S, window_id: bootWin, label: 'renamed tab'});
  for (let t = 0; t < 30 && (await ident(`codex:thread-1-${R}`)).name !== 'renamed tab'; t++) await sleep(100);
  assert.equal((await ident(`codex:thread-1-${R}`)).name, 'renamed tab');

  // The agent process exits in a kept-open pane: child_exited detaches its identity.
  const sp2 = (await run('boot', 'hub.spawn', {profile: 'fakeclaude', key: 'id2-' + R, args: {task: 'a2-' + R}})).result;
  assert.equal(sp2?.status, 'open', JSON.stringify(sp2));
  await waitFile(`${OUT}/ready-a2-${R}`);
  const sid2 = `a2-start-${R}`;
  assert.equal((await report(`a2-${R}`, sid2, 'startup', {block: sp2.block_id})).result?.state, 'current');
  fs.writeFileSync(`${OUT}/fifo-a2-${R}`, 'kill -TERM $PPID\n');
  for (let t = 0; t < 50 && (await ident('claude:' + sid2)).state !== 'detached'; t++) await sleep(100);
  assert.equal((await ident('claude:' + sid2)).state, 'detached', 'a process exit detaches the identity');

  // Close the spawned pane: its identity is detached and keeps its record.
  const cur = (await hub('hub.identity.list')).identities.find(x => x.block_id === blk && x.state === 'current').identity;
  await c.call('session.attach', {session_id: sp.session_id, event_scope: 'session'});
  await c.call('block.close', {session_id: sp.session_id, block_id: blk});
  for (let t = 0; t < 50 && (await ident(cur)).state !== 'detached'; t++) await sleep(100);
  assert.equal((await ident(cur)).state, 'detached');
  const lg = (await hub('hub.identity.list', {log: 50})).log.map(e => `${e.event} ${e.identity}`).reverse();
  console.log(lg.join('\n'));
  console.log('PASS: identity stage 1');
} finally {
  await c.call('session.destroy', {session_id: S});
  c.close();
}
