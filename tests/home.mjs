// Test real Rex Lua with a controlled hub. This does not test the launch service.
import {connectControl} from '../lib/rex-client.mjs';
import {labSocket, until} from '../lab/lab.mjs';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import assert from 'node:assert/strict';
import {fileURLToPath} from 'node:url';
const socket=await labSocket();
const hub=await connectControl(socket), control=await connectControl(socket);
let child, pluginId, tabs;
let failNavigation=false, failProfiles=false, failMode=false;
let mode={mode:'app',cycle:'cycle-1',phase:'active',problems:[]};
let exitResult={cycle:'cycle-1',phase:'restoring',problems:[]};
let spawnResult={status:'open',spawn_id:'w-new'};
const calls=[];
const fixtures={profiles:[{id:'shell',name:'Shell',kind:'accessory'},{id:'agent',name:'Agent',kind:'agent',args:[{name:'task',label:'Task',required:true,default:'Inspect changes'},{name:'mode',label:'Mode',default:'review'}]}],workers:[
  {spawn_id:'w1',title:'Shell 1',state:'running',window_id:'window:1',status:{state:'idle',msg:'Ready'}},
  {spawn_id:'w2',title:'Old worker',state:'closed'},
  {spawn_id:'w3',title:'Failed launch',state:'failed',error:'Command not found'}],
  plugins:[{id:'lazyrex',name:'LazyRex',tab:'home'},{id:'messaging',name:'Messages',tab:'home'}],
  agents:[{id:'user',name:'User',joined:true},{id:'codex',name:'Codex',joined:true,ready:true}]};
try {
  console.log('Rex',JSON.stringify(await control.call('server.version')));
  const label='home-test-'+process.pid;
  await hub.call('client.set_label',{label});
  const hubId=(await control.call('client.list')).clients.find(c=>c.info?.label===label).client_id;
  const names=['register','ui.tabs','profiles','workers','plugins','agents','spawn','stop','navigate.worker','ui.navigate','ui.mode','ui.exit'];
  await hub.call('client.keymap',{actions:names.map(n=>({name:'hub.'+n,kind:'client',title:n}))});
  hub.events.on('event',async frame=>{
    if(frame.payload?.type!=='client_perform_requested') return;
    const ev=frame.payload.payload;
    try {
      calls.push({action:ev.action,args:ev.args});
      if(failMode && ev.action==='hub.ui.mode') throw new Error('mode unavailable');
      if(failProfiles && ev.action==='hub.profiles') throw new Error('Invalid profile file');
      if(failNavigation && ev.action==='hub.navigate.worker') throw new Error('navigation unavailable');
      let result;
      if(ev.action==='hub.register') {pluginId=ev.requester.client_id;assert.equal(ev.args.plugin,'lazyrex');result={generation:'1'};}
      else if(ev.action==='hub.ui.tabs') {tabs=ev.args.tabs;result={tabs:1};}
      else if(ev.action==='hub.ui.mode') result=mode;
      else if(ev.action==='hub.ui.exit') result=exitResult;
      else if(fixtures[ev.action.slice(4)]) result={[ev.action.slice(4)]:fixtures[ev.action.slice(4)]};
      else if(ev.action==='hub.spawn') result=spawnResult;
      else result={status:'accepted'};
      await hub.call('client.reply',{call_id:ev.call_id,result});
    } catch(error) {await hub.call('client.reply',{call_id:ev.call_id,error:{code:'internal',message:error.message}});}
  });
  child=spawn('rex',['-S','unix://'+socket,'--autostart=false','do',process.env.HOME_PLUGIN || fileURLToPath(new URL('../plugins/lazyrex.lua',import.meta.url)),'--all',`hub_client_id=${hubId}`,'instance_id=home-test'],{env:{...process.env,REX_SERVER:'unix://'+socket,REX_SESSION:'',REX_BLOCK:'',REX_CLIENT:''}});
  child.stdout.pipe(process.stdout);child.stderr.pipe(process.stderr);
  await until(async()=> (await control.call('client.list')).clients.some(c=>c.client_id===pluginId&&c.info?.label==='lazyrex-plugin:lazyrex'),'home registration');
  const call=async(action,args={},connection=hub)=>(await connection.call('client.perform',{client_id:pluginId,action,args:{route:'/',user:'user',...args},timeout_ms:10000})).result;
  assert.deepEqual(tabs,[{key:'home',title:'LazyRex',route:'/'}]);
  const rendered=await call('view.render');
  assert.deepEqual(Object.keys(rendered),['tree'],'view.render returns the hub envelope');
  const tree=rendered.tree;
  assert.equal(tree.title,'LazyRex');
  assert.equal(tree.nodes.find(n=>n.id==='plugins').items.length,1);
  assert.equal(tree.nodes.find(n=>n.id==='agents').items[0].label,'Codex');
  assert.equal(tree.nodes.find(n=>n.id==='workers').items[0].detail,'idle · Ready');
  assert.equal(tree.nodes.find(n=>n.id==='stop').fields[0].options.length,1);
  assert.equal(tree.nodes.find(n=>n.id==='workers').items[2].detail,'failed · Command not found');
  const agentForm=tree.nodes.find(n=>n.id==='launch/agent');
  assert.deepEqual(agentForm.fields[1],{id:'arg/task',label:'Task',kind:'multiline',required:true,value:'Inspect changes'});
  assert.equal(agentForm.fields[2].value,'review');
  assert.equal(tree.nodes.find(n=>n.id==='launch/shell').fields.length,1);
  failProfiles=true;
  const failedProfiles=(await call('view.render')).tree;
  assert.match(failedProfiles.nodes.find(n=>n.id==='profiles_error').text,/Invalid profile file/);
  assert(failedProfiles.nodes.some(n=>n.id==='stop'),'profile errors keep worker controls');
  assert(failedProfiles.nodes.some(n=>n.id==='plugins'),'profile errors keep navigation');
  failProfiles=false;
  const run=(node,action,input,key='operation-1')=>call('view.action',{node_id:node,action,input,operation_key:key});
  const exitWidget=tree.nodes.find(n=>n.id==='app_mode');
  assert.equal(exitWidget.action.label,'Exit LazyRex');
  assert(!exitWidget.action.closes_view,'Exit preserves the Home renderer and form input');
  assert.match(exitWidget.text,/App mode is active/);
  const exiting=await run('app_mode','exit',{},'exit-1');
  assert.equal(exiting.cycle,'cycle-1');
  assert.equal(exiting.phase,'restoring');
  assert.deepEqual(calls.at(-1),{action:'hub.ui.exit',args:{for:'user',key:'exit-1'}});
  mode={mode:'app',cycle:'cycle-2',phase:'active',problems:[]};
  await run('app_mode','exit',{},'exit-1');
  assert.deepEqual(calls.at(-1),{action:'hub.ui.exit',args:{for:'user',key:'exit-1'}},'Exit retries preserve the key; the hub owns cycle binding');
  mode={mode:'app',cycle:'cycle-1',phase:'restoring',restored:{windows:2,blocks:3},problems:[{block_id:'block:missing',code:'not_found',message:'Pane is no longer available.'}]};
  const progress=(await call('view.render')).tree.nodes.find(n=>n.id==='app_mode');
  assert.match(progress.text,/Restoring workspace panes/);
  assert.match(progress.text,/2 windows, 3 panes/);
  assert.match(progress.text,/block:missing: Pane is no longer available/);
  mode.retry={attempts:3,error:'The Rex connection is unavailable.'};
  const retryStatus=(await call('view.render')).tree.nodes.find(n=>n.id==='app_mode');
  assert.match(retryStatus.text,/Restoration will retry. Failed attempts: 3/);
  assert.match(retryStatus.text,/Rex connection is unavailable/);
  mode={mode:'app',cycle:'cycle-3',phase:'capturing',problems:[]};
  assert.match((await call('view.render')).tree.nodes.find(n=>n.id==='app_mode').text,/Preparing app mode/);
  mode={mode:'workspace',cycle:'cycle-3',phase:'refused',problems:[{session_id:'session:one',code:'unsupported',message:'Unsupported layout'}]};
  const refused=(await call('view.render')).tree.nodes.find(n=>n.id==='app_mode');
  assert.match(refused.text,/No workspace panes were hidden/);
  assert.match(refused.text,/session:one: Unsupported layout/);
  failMode=true;
  const unavailable=(await call('view.render')).tree;
  assert(unavailable.nodes.some(n=>n.id==='plugins'));
  assert.equal(unavailable.nodes.find(n=>n.id==='app_mode').action.enabled,true);
  await run('app_mode','exit',{},'exit-2');
  assert.equal(calls.at(-1).action,'hub.ui.exit');
  failMode=false;
  await assert.rejects(run('app_mode','exit',{},''),/operation key/);
  await run('plugins','open',{item_id:'messaging'});
  assert.deepEqual(calls.at(-1),{action:'hub.ui.navigate',args:{plugin:'messaging',tab:'home'}});
  await run('agents','message',{item_id:'codex'});
  assert.deepEqual(calls.at(-1),{action:'hub.ui.navigate',args:{plugin:'messaging',tab:'thread/codex'}});
  await run('workers','open',{item_id:'w1'});
  assert.deepEqual(calls.at(-1),{action:'hub.navigate.worker',args:{spawn_id:'w1'}});
  await run('launch/shell','launch',{values:{profile:'shell',title:'Test'}});
  assert.deepEqual(calls.at(-2),{action:'hub.spawn',args:{profile:'shell',title:'Test',key:'operation-1',for:'user'}});
  assert.deepEqual(calls.at(-1),{action:'hub.navigate.worker',args:{spawn_id:'w-new'}});
  spawnResult={status:'pending',spawn_id:'w-new'};
  await run('launch/agent','launch',{values:{title:'Task tab','arg/task':'Read code\nExplain findings','arg/mode':'review'}},'named-args');
  const namedRequest={action:'hub.spawn',args:{profile:'agent',title:'Task tab',key:'named-args',for:'user',args:{task:'Read code\nExplain findings',mode:'review'}}};
  assert.deepEqual(calls.at(-1),namedRequest);
  fixtures.profiles=[];
  await run('launch/agent','launch',{values:{title:'Task tab','arg/task':'Read code\nExplain findings','arg/mode':'review'}},'named-args');
  assert.deepEqual(calls.at(-1),namedRequest,'named inputs survive retry after profile removal');
  await assert.rejects(run('launch/agent','launch',{values:{title:'','arg/task':42}}),/must be text/);

  await run('launch/shell','launch',{values:{profile:'shell',title:'Test'}});
  assert.equal(calls.at(-1).action,'hub.spawn','launch retry reaches durable service even after profile removal');
  fixtures.workers=[];
  await run('workers','dismiss',{item_id:'w3'});
  assert.deepEqual(calls.at(-1),{action:'hub.stop',args:{spawn_id:'w3',key:'operation-1',for:'user'}});
  await run('stop','stop',{values:{worker:'w1'}});
  assert.deepEqual(calls.at(-1),{action:'hub.stop',args:{spawn_id:'w1',key:'operation-1',for:'user'}});
  await assert.rejects(call('view.action',{node_id:'launch/shell',action:'launch',input:{values:{profile:'shell',title:''}}}),/operation key/);
  await assert.rejects(call('view.render',{},control),/Only the hub/);
  await assert.rejects(call('view.render',{user:'codex'}),/Unknown home route/);
  await assert.rejects(call('view.render',{route:'/bad'}),/Unknown home route/);
  await assert.rejects(run('plugins','open',{item_id:'missing'}),/no longer available/);
  spawnResult={status:'open',spawn_id:'w-new'};
  failNavigation=true;
  const launched=await run('launch/shell','launch',{values:{profile:'shell',title:'Test'}});
  assert.equal(launched.status,'open','a navigation failure does not report a failed launch');
  const notice=(await call('view.render')).tree.nodes.find(n=>n.id==='navigation_notice');
  assert.match(notice.text,/worker started/);
  await call('hub.changed',{what:'profiles'});
  const {tree:empty}=await call('view.render');
  assert(empty.nodes.some(n=>n.id==='no_profiles'));
  assert(!empty.nodes.some(n=>n.id==='stop'));
  console.log('PASS: home rendering, navigation, mutation keys, retries, and caller checks');
} finally {
  if(child&&child.exitCode===null&&child.signalCode===null){child.kill('SIGTERM');await once(child,'exit');}
  control.close();hub.close();
}
