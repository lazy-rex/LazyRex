// Test the installed hook shape and MCP adapter against the candidate hub in a real lab pane.
import {connectControl} from '../lib/rex-client.mjs';
import {labSocket, until} from '../lab/lab.mjs';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {createInterface} from 'node:readline';
import {fileURLToPath} from 'node:url';
import {mkdtemp, writeFile, readFile, access} from 'node:fs/promises';
import assert from 'node:assert/strict';

const socket = await labSocket();
const c = await connectControl(socket);
const hubId = (await c.call('client.list')).clients.find(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected')?.client_id;
assert(hubId, 'Start the candidate hub in the lab');
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id:hubId, action, args, timeout_ms:10000})).result;
const hubBin = process.env.HUB_BIN; // the lazyrex-hub binary: its hook subcommand is the installed hook
assert(hubBin, 'Set HUB_BIN to the lazyrex-hub binary');
const delay = ms => new Promise(r => setTimeout(r, ms));

if (process.argv[2] === '--driver') {
  const dir = process.argv[3];
  let shim;
  try {
    await until(async () => {try {await access(dir + '/ready'); return true;} catch {return false;}}, 'pane binding');
    const marker = Date.now().toString(36);
    const older = spawn(hubBin, ['hook', 'codex']);
    const olderExit = once(older, 'exit');
    // Keep input open until the newer reporter has completed.
    await delay(300);
    const newer = spawn(hubBin, ['hook', 'codex']);
    const newerExit = once(newer, 'exit');
    newer.stdin.end(JSON.stringify({hook_event_name:'SessionStart', session_id:'new-' + marker, source:'startup'}));
    assert.equal((await newerExit)[0], 0);
    older.stdin.end(JSON.stringify({hook_event_name:'SessionStart', session_id:'old-' + marker, source:'clear'}));
    assert.equal((await olderExit)[0], 0);
    let state = await hub('hub.identity.list', {log:30});
    assert(state.identities.some(x => x.identity === 'codex:new-' + marker && x.state === 'current'));
    assert(!state.identities.some(x => x.identity === 'codex:old-' + marker));
    assert(state.log.some(x => x.identity === 'codex:old-' + marker && x.event === 'stale_report'));

    shim = spawn(process.env.MCP_BIN, ['-server', 'unix://' + socket, '-agent', 'claude'], {env:{...process.env, LAZYREX_AGENT:'claude'}});
    const pending = new Map(); let id = 0, errors = '';
    shim.stderr.on('data', b => {errors += b;});
    createInterface({input:shim.stdout}).on('line', line => {const r = JSON.parse(line); pending.get(r.id)?.(r); pending.delete(r.id);});
    const request = async (method, params) => {
      const key = ++id; let timer;
      try {return await new Promise((resolve, reject) => {
        timer = setTimeout(() => reject(Error(method + ': ' + errors)), 15000);
        pending.set(key, resolve);
        shim.stdin.write(JSON.stringify({jsonrpc:'2.0', id:key, method, params}) + '\n');
      });} finally {clearTimeout(timer); pending.delete(key);}
    };
    await request('initialize', {protocolVersion:'2024-11-05', capabilities:{}, clientInfo:{name:'identity-integration',version:'1'}});
    shim.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'notifications/initialized'}) + '\n');
    const tools = await request('tools/list', {});
    assert(tools.result.tools.some(t => t.name === 'messaging_read'));
    const result = await request('tools/call', {name:'messaging_read', arguments:{}, _meta:{threadId:'mcp-' + marker, sessionId:'wrong-root'}});
    assert(!result.error && !result.result.isError, JSON.stringify(result));
    state = await hub('hub.identity.list');
    const current = state.identities.find(x => x.identity === 'codex:mcp-' + marker);
    assert.equal(current?.state, 'current');
    assert.equal(current?.agent_id, 'claude');
    assert.equal(state.identities.find(x => x.identity === 'codex:new-' + marker)?.state, 'superseded');
    await writeFile(dir + '/result.json', JSON.stringify({ok:true, current}));
  } catch (error) {
    await writeFile(dir + '/result.json', JSON.stringify({ok:false, error:error.stack}));
  } finally {
    if (shim && shim.exitCode === null && shim.signalCode === null) {const exited = once(shim, 'exit'); shim.stdin.end(); await exited;}
    c.close();
  }
} else {
  assert(process.env.MCP_BIN, 'Set MCP_BIN to the candidate adapter');
  const dir = await mkdtemp('/tmp/lazyrex-identity-transport-');
  const session = (await c.call('session.create', {label:'identity-transport'})).session_id;
  try {
    await c.call('session.attach', {session_id:session, event_scope:'session'});
    const pane = await c.call('session.new_window', {session_id:session, window_label:'Transport test', layout:{block:{flavor:'com.superlogical.terminal.shell', options:{command:['/usr/bin/env', 'MCP_BIN=' + process.env.MCP_BIN, 'HUB_BIN=' + process.env.HUB_BIN, process.execPath, fileURLToPath(import.meta.url), '--driver', dir], login:false}}}});
    await hub('hub.admin.rebind', {agent:'claude', block_id:pane.block_ids[0]});
    await writeFile(dir + '/ready', 'ready');
    const result = await until(async () => {try {return JSON.parse(await readFile(dir + '/result.json', 'utf8'));} catch (e) {if(e.code === 'ENOENT') return false; throw e;}}, 'transport result', 40000);
    assert.equal(result.ok, true, result.error);
    console.log('PASS: delayed real hook is stale; MCP metadata changes the hub identity before a messaging call');
    console.log(JSON.stringify(result));
  } finally {
    await c.call('session.destroy', {session_id:session}); c.close();
    console.log('Evidence: ' + dir);
  }
}
