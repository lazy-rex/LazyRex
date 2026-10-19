// Lab: outside app mode the hub never creates an app session. A closed app session stays closed;
// opening the app creates it again with its views, and Exit hides them. Lab only: /tmp/rexlab-s3ui.
import {connectControl} from '../lib/rex-client.mjs';
import assert from 'node:assert/strict';
const c = await connectControl('/tmp/rexlab-s3ui/rex.sock');
const sleep = ms => new Promise(r => setTimeout(r, ms));
const hubId = (await c.call('client.list')).clients.filter(x => x.info?.label === 'lazyrex-hub' && x.connection_state === 'connected').at(-1).client_id;
const hub = async (action, args = {}) => (await c.call('client.perform', {client_id: hubId, action, args, timeout_ms: 10000})).result;
const until = async (what, f, ms = 20000) => { const t = Date.now(); for (;;) { const v = await f(); if (v) return v; if (Date.now() - t > ms) throw new Error('timeout: ' + what); await sleep(150); } };
const msgs = async () => (await c.call('session.list')).sessions.find(s => s.label === 'LazyRex: Messages');
const windows = async id => { await c.call('session.attach', {session_id: id, event_scope: 'session'}); return ((await c.call('session.view', {session_id: id})).windows ?? []).length; };
assert.equal((await hub('hub.ui.mode')).mode, 'workspace');
const s0 = await until('Messages session', msgs);
await c.call('session.attach', {session_id: s0.session_id, event_scope: 'session'});
await c.call('session.destroy', {session_id: s0.session_id});
await hub('hub.ui.navigate', {plugin: 'lazyrex', tab: 'home'}); // a pass runs
await sleep(3000);
assert.equal(await msgs(), undefined, 'a closed app session stays closed outside app mode');
await hub('hub.ui.navigate', {plugin: 'messaging', tab: 'home'});
const s1 = await until('Messages session again', msgs);
await until('Messages tabs', async () => (await windows(s1.session_id)) > 0);
await until('active', async () => (await hub('hub.ui.mode')).phase === 'active');
await hub('hub.ui.exit', {for: 'user', key: 'app-closed-' + Date.now()});
await until('complete', async () => (await hub('hub.ui.mode')).phase === 'complete');
await until('Messages hidden', async () => (await windows(s1.session_id)) === 0);
assert.equal((await msgs()).session_id, s1.session_id, 'Exit hides the session; it does not close it');
console.log('PASS: a closed app session stays closed; Open creates it with its tabs; Exit hides it');
c.close();
