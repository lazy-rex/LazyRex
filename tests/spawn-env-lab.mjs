// Lab: spawned panes do not inherit agent variables (workers.go). Lab only: SPAWN_ENV_LAB
// (default /tmp/rexlab-profiles-claude). Start the lab server with stale variables:
//   LAZYREX_AGENT=stale-agent CODEX_THREAD_ID=stale-thread spikes/lab/server.sh $LAB
// and the hub on $LAB/env.json (plugins: tests/spawnprobe.lua; profiles: $LAB/env-profiles.json
// with the agent profile "envdump", which writes its environment to $LAB/out/env-agent).
// Checks: the built-in shell has neither variable; an agent has a fresh LAZYREX_AGENT and no
// CODEX_THREAD_ID.
import {connectControl} from '../lib/rex-client.mjs';
import {execFileSync} from 'node:child_process';
import fs from 'node:fs';
import assert from 'node:assert/strict';
const LAB = process.env.SPAWN_ENV_LAB || '/tmp/rexlab-profiles-claude', SOCK = `unix://${LAB}/rex.sock`, OUT = `${LAB}/out`;
const c = await connectControl(`${LAB}/rex.sock`);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFile = async (f, ms = 10000) => { for (let t = 0; t < ms && !(fs.existsSync(f) && fs.statSync(f).size > 0); t += 100) await sleep(100); assert(fs.existsSync(f), `${f} appears`); await sleep(200); };
const clients = (await c.call('client.list')).clients.filter(x => x.connection_state === 'connected');
const probe = clients.filter(x => x.info?.label === 'lazyrex-plugin:spawnprobe').at(-1).client_id;
const started = [];
const stop = spawn_id => c.call('client.perform', {client_id: probe, action: 'probe.stop', args: {spawn_id}, timeout_ms: 15000});
const spawn = async args => { const r = await spawnRaw(args); if (r?.spawn_id) started.push(r.spawn_id); return r; };
const spawnRaw = async args => (await c.call('client.perform', {client_id: probe, action: 'probe.spawn', args, timeout_ms: 15000})).result;
const rex = (...a) => execFileSync('env', ['-u', 'REX_SESSION', '-u', 'REX_BLOCK', 'rex', '-S', SOCK, '--autostart=false', ...a], {encoding: 'utf8'});
const envOf = f => Object.fromEntries(fs.readFileSync(f, 'utf8').split('\n').filter(l => l.includes('=')).map(l => [l.slice(0, l.indexOf('=')), l.slice(l.indexOf('=') + 1)]));

fs.rmSync(OUT, {recursive: true, force: true});
fs.mkdirSync(OUT);
const R = Date.now().toString(36);
// The lab server itself has the stale variables (checked through a plain pane it starts).
const S = (await c.call('session.create', {label: 'spawn-env-lab', initial_windows: [{window_label: 'plain',
  layout: {block: {flavor: 'com.superlogical.terminal.shell', label: 'plain', options: {command: ['/bin/sh', '-c', `env > ${OUT}/env-plain; exec sleep 600`], login: false}}}}]})).session_id;
await c.call('session.attach', {session_id: S, event_scope: 'session'}); // destroy needs an attached client
try {
  await waitFile(`${OUT}/env-plain`);
  assert.equal(envOf(`${OUT}/env-plain`).LAZYREX_AGENT, 'stale-agent', 'the lab server has a stale LAZYREX_AGENT');
  assert.equal(envOf(`${OUT}/env-plain`).CODEX_THREAD_ID, 'stale-thread', 'the lab server has a stale CODEX_THREAD_ID');

  const sh = await spawn({profile: 'shell', key: 'shell-' + R, cwd: LAB});
  assert.equal(sh.status, 'open', JSON.stringify(sh));
  assert(!sh.agent_id, 'the shell has no agent');
  await sleep(1500); // the login shell starts
  rex('send', '-s', sh.session_id, '-b', sh.block_id, `env > ${OUT}/env-shell\n`);
  await waitFile(`${OUT}/env-shell`);
  const se = envOf(`${OUT}/env-shell`);
  assert(!('LAZYREX_AGENT' in se), 'the shell has no LAZYREX_AGENT: ' + se.LAZYREX_AGENT);
  assert(!('CODEX_THREAD_ID' in se), 'the shell has no CODEX_THREAD_ID');
  assert.equal(fs.realpathSync(se.PWD), fs.realpathSync(LAB), 'the shell starts in cwd');

  const ag = await spawn({profile: 'envdump', key: 'agent-' + R});
  assert.equal(ag.status, 'open', JSON.stringify(ag));
  await waitFile(`${OUT}/env-agent`);
  const ae = envOf(`${OUT}/env-agent`);
  assert.equal(ae.LAZYREX_AGENT, ag.agent_id, 'the agent has its fresh agent ID');
  assert.notEqual(ae.LAZYREX_AGENT, 'stale-agent');
  assert(!('CODEX_THREAD_ID' in ae), 'the agent has no CODEX_THREAD_ID');
  console.log('spawn-env-lab: pass', {shell: sh.spawn_id, agent: ag.agent_id});
} finally {
  for (const id of started) await stop(id).catch(e => console.error('stop', id, e.message));
  await c.call('session.destroy', {session_id: S}).catch(() => {});
  c.close?.();
}
