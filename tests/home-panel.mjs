// Test Home panel effects with the real Lua plugin and a controlled hub.
import {connectControl} from '../lib/rex-client.mjs';
import {labSocket, until} from '../lab/lab.mjs';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {fileURLToPath} from 'node:url';
import assert from 'node:assert/strict';

const socket = await labSocket();
const hub = await connectControl(socket), control = await connectControl(socket);
let child, pluginId;
const navigation = [];
try {
  console.log('Rex', JSON.stringify(await control.call('server.version')));
  const label = 'home-panel-test-' + process.pid;
  await hub.call('client.set_label', {label});
  const hubId = (await control.call('client.list')).clients.find(c => c.info?.label === label).client_id;
  const fixtures = {profiles: [], workers: [], agents: [], plugins: [
    {id:'lazyrex', tab:'home'}, {id:'messaging', name:'Messages', panel:'threads'},
    {id:'worktrees', name:'Worktrees', panel:'worktrees'}, {id:'legacy', tab:'home'}
  ]};
  const actions = ['register', 'ui.tabs', 'profiles', 'workers', 'plugins', 'agents', 'ui.navigate'];
  await hub.call('client.keymap', {actions: actions.map(name => ({name:'hub.' + name, kind:'client', title:name}))});
  hub.events.on('event', async frame => {
    if (frame.payload?.type !== 'client_perform_requested') return;
    const ev = frame.payload.payload;
    try {
      let result = {};
      if (ev.action === 'hub.register') { pluginId = ev.requester.client_id; result = {generation:'1'}; }
      else if (ev.action === 'hub.ui.navigate') { navigation.push(ev.args); result = {status:'open'}; }
      else if (fixtures[ev.action.slice(4)]) result = {[ev.action.slice(4)]:fixtures[ev.action.slice(4)]};
      await hub.call('client.reply', {call_id:ev.call_id, result});
    } catch (error) { await hub.call('client.reply', {call_id:ev.call_id, error:{code:'internal',message:error.message}}); }
  });
  child = spawn('rex', ['-S','unix://' + socket,'--autostart=false','do', fileURLToPath(new URL('../plugins/lazyrex.lua',import.meta.url)), '--all', 'hub_client_id=' + hubId, 'instance_id=home-panel-test'], {env:{...process.env,REX_SERVER:'unix://' + socket,REX_SESSION:'',REX_BLOCK:'',REX_CLIENT:''}});
  child.stderr.pipe(process.stderr);
  await until(async () => (await control.call('client.list')).clients.some(c => c.client_id === pluginId && c.info?.label === 'lazyrex-plugin:lazyrex'), 'Home registration');
  const call = async (action,args={}) => (await hub.call('client.perform', {client_id:pluginId, action, args:{route:'/',user:'user',...args},timeout_ms:10000})).result;
  const tree = (await call('view.render')).tree;
  assert.deepEqual(tree.nodes.find(n => n.id === 'plugins').items.map(i => i.id), ['messaging','worktrees','legacy']);
  for (const [plugin, command] of [['messaging','threads'],['worktrees','worktrees']]) {
    const effect = await call('view.action',{node_id:'plugins',action:'open',input:{item_id:plugin}});
    assert.deepEqual(effect,{panel:{plugin,command}});
  }
  assert.equal(navigation.length,0,'Home returns a bound panel effect instead of choosing a window itself');
  await call('view.action',{node_id:'plugins',action:'open',input:{item_id:'legacy'}});
  assert.deepEqual(navigation,[{plugin:'legacy',tab:'home'}]);
  console.log('PASS Home lists panel plugins, returns declared panel effects, and preserves tab navigation');
} finally {
  if (child && child.exitCode === null && child.signalCode === null) { child.kill('SIGINT'); await once(child,'exit'); }
  control.close(); hub.close();
}
