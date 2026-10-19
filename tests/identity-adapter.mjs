// Test the MCP adapter against a controlled hub on an isolated Rex server.
import {connectControl} from '../lib/rex-client.mjs';
import {labSocket} from '../lab/lab.mjs';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {createInterface} from 'node:readline';
import assert from 'node:assert/strict';

const socket = await labSocket();
assert(process.env.MCP_BIN, 'Set MCP_BIN to the candidate adapter binary');
const hub = await connectControl(socket);
const calls = [], pending = new Map();
let child, nextId = 0, stderr = '';
try {
  console.log('Rex', JSON.stringify(await hub.call('server.version')));
  assert(!(await hub.call('client.list')).clients.some(c => c.info?.label === 'lazyrex-hub' && c.connection_state === 'connected'), 'Use a lab without a hub');
  await hub.call('client.set_label', {label:'lazyrex-hub'});
  const hubId = (await hub.call('client.list')).clients.find(c => c.info?.label === 'lazyrex-hub').client_id;
  const names = ['hub.join', 'hub.tools', 'hub.identity.report', 'probe.echo'];
  await hub.call('client.keymap', {actions:names.map(name => ({name, kind:'client', title:name, args:{type:'object'}}))});
  hub.events.on('event', async frame => {
    if (frame.payload?.type !== 'client_perform_requested') return;
    const ev = frame.payload.payload;
    calls.push({action:ev.action, args:ev.args});
    try {
      assert.equal(ev.requester.principal.pid, child.pid);
      if (ev.action === 'hub.identity.report' && ev.args.conversation_id === 'stale') {
        await hub.call('client.reply', {call_id:ev.call_id, error:{code:'stale', message:'stale: old conversation'}});
        return;
      }
      const result = ev.action === 'hub.tools'
        ? {tools:[{name:'probe_echo', action:'probe.echo', client_id:hubId}]}
        : ev.action === 'probe.echo' ? ev.args : {};
      await hub.call('client.reply', {call_id:ev.call_id, result});
    } catch (error) {
      await hub.call('client.reply', {call_id:ev.call_id, error:{code:'internal', message:error.message}});
    }
  });
  child = spawn(process.env.MCP_BIN, ['-server', 'unix://' + socket, '-agent', 'lab'], {
    env:{...process.env, LAZYREX_AGENT:'lab', REX_BLOCK:'block:hint', CODEX_THREAD_ID:'stale-env', CODEX_SESSION_ID:'stale-env'}
  });
  child.stderr.on('data', b => {stderr += b;});
  createInterface({input:child.stdout}).on('line', line => {
    const response = JSON.parse(line);
    pending.get(response.id)?.(response);
    pending.delete(response.id);
  });
  const request = async (method, params) => {
    const id = ++nextId;
    let timer;
    try {
      return await new Promise((resolve, reject) => {
        timer = setTimeout(() => reject(Error('MCP timeout: ' + method + '\n' + stderr)), 15000);
        pending.set(id, resolve);
        child.stdin.write(JSON.stringify({jsonrpc:'2.0', id, method, params}) + '\n');
      });
    } finally {clearTimeout(timer); pending.delete(id);}
  };
  assert(!(await request('initialize', {protocolVersion:'2024-11-05', capabilities:{}, clientInfo:{name:'identity-test', version:'1'}})).error);
  child.stdin.write(JSON.stringify({jsonrpc:'2.0', method:'notifications/initialized'}) + '\n');
  const list = await request('tools/list', {});
  assert.equal(list.result.tools[0].name, 'probe_echo');
  assert.deepEqual(calls.map(c => c.action), ['hub.join', 'hub.tools']);
  assert.deepEqual(calls[0].args, {agent:'lab'});
  const invoke = (value, meta) => request('tools/call', {name:'probe_echo', arguments:{value}, ...(meta ? {_meta:meta} : {})});
  const replies = await Promise.all([
    invoke('first', {threadId:'conversation-a', sessionId:'wrong-root'}),
    invoke('second', {threadId:'conversation-b'}),
    invoke('claude'),
    invoke('rejected-report', {threadId:'stale'}),
    invoke('invalid-meta', {threadId:42}),
  ]);
  for (const reply of replies) assert(!reply.error && !reply.result.isError, JSON.stringify(reply));
  assert.deepEqual(replies.map(r => JSON.parse(r.result.content[0].text).value), ['first','second','claude','rejected-report','invalid-meta']);
  const reports = calls.filter(c => c.action === 'hub.identity.report');
  assert.deepEqual(reports.map(c => c.args.conversation_id).sort(), ['conversation-a','conversation-b','stale']);
  for (const report of reports) {
    assert.equal(report.args.kind, 'codex');
    assert.equal(report.args.source, 'mcp');
    assert.equal(report.args.block, 'block:hint');
    const after = calls[calls.indexOf(report) + 1];
    assert.equal(after.action, 'probe.echo', 'A report and its dispatch stay together');
    assert.equal(after.args.value, {'conversation-a':'first','conversation-b':'second',stale:'rejected-report'}[report.args.conversation_id]);
  }
  assert.match(stderr, /conversation identity report:.*stale/);
  console.log('PASS: startup join, discovery without identity, per-call thread identity, concurrent ordering, absent metadata, and advisory report failure');
} finally {
  if (child && child.exitCode === null && child.signalCode === null) {
    const exited = once(child, 'exit');
    child.stdin.end();
    await exited;
  }
  hub.close();
}
