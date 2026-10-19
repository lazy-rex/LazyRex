// End-to-end identity migration and native Rex actions. The test owns its hub and agent panes.
import {connectControl} from '../lib/rex-client.mjs';
import {labSocket,until} from '../lab/lab.mjs';
import {spawn,execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {once} from 'node:events';
import {mkdtemp,writeFile,readFile} from 'node:fs/promises';
import {fileURLToPath} from 'node:url';
import assert from 'node:assert/strict';

const run=promisify(execFile),socket=await labSocket();
const delay=ms=>new Promise(r=>setTimeout(r,ms));
const readJSON=async file=>{try{return JSON.parse(await readFile(file,'utf8'));}catch(e){if(e.code==='ENOENT'||e instanceof SyntaxError)return null;throw e;}};
if(process.argv[2]==='--agent'){
 const dir=process.argv[3],name=process.argv[4];let last=0;
 await writeFile(`${dir}/${name}-ready`,'ready');
 for(;;){
  const request=await readJSON(`${dir}/${name}-request.json`);
  if(!request||request.seq===last){await delay(20);continue;}
  last=request.seq;
  let result;
  try{
   const argv=['-S','unix://'+socket,'--autostart=false','-C',request.client,'do',request.action,'--args',JSON.stringify(request.args||{})];
   const response=await run(process.env.REX_BIN||'rex',argv,{timeout:15000});
   result={ok:true,value:JSON.parse(response.stdout)};
  }catch(e){result={ok:false,error:String(e.stderr||e.stack)};}
  await writeFile(`${dir}/${name}-result.json`,JSON.stringify({seq:last,...result}));
 }
}else{
 assert(process.env.HUB_BIN&&process.env.VIEW_BIN,'Set HUB_BIN and VIEW_BIN to candidate binaries');
 const c=await connectControl(socket),dir=await mkdtemp('/tmp/lazyrex-native-');
 let hubProcess,session,seq=0,logs='';
 const plugins=['lazyrex','messaging'].map(name=>({name,path:fileURLToPath(new URL(name==='lazyrex'?'../plugins/lazyrex.lua':`../../${name[0].toUpperCase()+name.slice(1)}/${name}.lua`,import.meta.url))}));
 await writeFile(dir+'/profiles.json',JSON.stringify({profiles:[]}));
 await writeFile(dir+'/hub.json',JSON.stringify({server:'unix://'+socket,state:dir+'/state',view:process.env.VIEW_BIN,profiles:dir+'/profiles.json',agents:[{id:'user',name:'User'},{id:'legacy-a',name:'A'},{id:'legacy-b',name:'B'}],plugins}));
 const clients=async()=>(await c.call('client.list')).clients;
 const find=async label=>(await clients()).filter(x=>x.info?.label===label&&x.connection_state==='connected');
 const hub=async(action,args={})=>{const [h]=await find('lazyrex-hub');return(await c.call('client.perform',{client_id:h.client_id,action,args,timeout_ms:10000})).result;};
 const start=async()=>{
  hubProcess=spawn(process.env.HUB_BIN,['-config',dir+'/hub.json'],{env:{...process.env,REX_SERVER:'unix://'+socket,REX_SESSION:'',REX_BLOCK:''}});
  hubProcess.stdout.on('data',b=>{logs+=b;});hubProcess.stderr.on('data',b=>{logs+=b;});
  await until(async()=>{
   if(hubProcess.exitCode!==null)throw Error(logs);
   const [face]=await find('lazyrex');if(!face)return false;
   const info=await c.call('client.inspect',{client_id:face.client_id});
   return info.actions?.some(a=>a.name==='messaging.send');
  },'native messaging actions',30000);
 };
 const stop=async signal=>{if(hubProcess&&hubProcess.exitCode===null&&hubProcess.signalCode===null){const done=once(hubProcess,'exit');hubProcess.kill(signal);await done;}};
 const call=async(name,client,action,args={})=>{
  if(client==='lazyrex-hub') client=(await find(client))[0].client_id;
  const id=++seq;
  await writeFile(`${dir}/${name}-request.json`,JSON.stringify({seq:id,client,action,args}));
  const result=await until(async()=>{const r=await readJSON(`${dir}/${name}-result.json`);return r?.seq===id&&r;},action,20000);
  assert.equal(result.ok,true,result.error);return result.value;
 };
 try{
  assert.equal((await find('lazyrex-hub')).length,0,'Use a lab without another hub');
  await start();
  session=(await c.call('session.create',{label:'native-identity-test'})).session_id;
  await c.call('session.attach',{session_id:session,event_scope:'session'});
  const panes={};
  for(const name of ['a','b']){
   const pane=await c.call('session.new_window',{session_id:session,window_label:'Agent '+name,layout:{block:{flavor:'com.superlogical.terminal.shell',options:{command:[process.execPath,fileURLToPath(import.meta.url),'--agent',dir,name],login:false}}}});
   panes[name]=pane.block_ids[0];
   await hub('hub.admin.rebind',{agent:'legacy-'+name,block_id:panes[name]});
   await call(name,'lazyrex-hub','hub.join',{agent:'legacy-'+name});
  }
  const sendArgs={to:'legacy-b',body:'before identity migration',key:'before-identity'};
  const sent=await call('a','lazyrex','messaging.send',sendArgs);
  const before=await call('b','lazyrex','messaging.read');
  assert.equal(before.messages[0].from,'legacy-a');
  await call('a','lazyrex-hub','hub.identity.report',{kind:'codex',conversation_id:'native-a',source:'startup',block:panes.a});
  await call('b','lazyrex-hub','hub.identity.report',{kind:'claude',conversation_id:'native-b',source:'startup',block:panes.b});
  const after=await call('b','lazyrex','messaging.read');
  assert.equal(after.messages[0].from,'codex:native-a');assert.equal(after.messages[0].to,'claude:native-b');
  assert.equal(after.messages[0].id,sent.id);assert.equal(after.messages[0].cursor,before.messages[0].cursor);
  assert.deepEqual(await call('a','lazyrex','messaging.send',sendArgs),sent);
  await call('b','lazyrex','messaging.ack',{ids:[sent.id]});
  assert.deepEqual((await call('b','lazyrex','messaging.read')).messages,{});
  const unjoined=await c.call('session.new_window',{session_id:session,window_label:'Unjoined agent',layout:{block:{flavor:'com.superlogical.terminal.shell',options:{command:[process.execPath,fileURLToPath(import.meta.url),'--agent',dir,'c'],login:false}}}});
  await call('c','lazyrex-hub','hub.identity.report',{kind:'claude',conversation_id:'native-unjoined',source:'startup',block:unjoined.block_ids[0]});
  const unjoinedSend=await call('c','lazyrex','messaging.send',{to:'claude:native-b',body:'No MCP join required',key:'unjoined'});
  assert.equal((await call('b','lazyrex','messaging.read')).messages[0].id,unjoinedSend.id);
  await call('b','lazyrex','messaging.ack',{ids:[unjoinedSend.id]});
  await call('a','lazyrex-hub','hub.identity.report',{kind:'codex',conversation_id:'native-a-new',source:'clear',block:panes.a});
  const parked=await call('b','lazyrex','messaging.send',{to:'legacy-a',body:'for the original conversation',key:'parked'});
  assert.deepEqual((await call('a','lazyrex','messaging.read')).messages,{});
  await call('a','lazyrex-hub','hub.identity.report',{kind:'codex',conversation_id:'native-a',source:'resume',block:panes.a});
  assert.equal((await call('a','lazyrex','messaging.read')).messages[0].id,parked.id);
  const oldFace=(await find('lazyrex'))[0].client_id;
  await stop('SIGKILL');
  await until(async()=>!(await clients()).some(x=>x.client_id===oldFace),'unattached face disappears');
  await start();
  const face=await find('lazyrex');assert.equal(face.length,1);assert.notEqual(face[0].client_id,oldFace);
  // Use the label through the actual Rex CLI immediately after restart.
  assert.equal((await call('a','lazyrex','messaging.read')).messages[0].id,parked.id);
  assert.deepEqual(await call('a','lazyrex','messaging.send',sendArgs),sent);
  console.log('PASS: native Rex forwarding, real caller identity, mailbox migration, old-key retry, clear/resume isolation, and hub kill/restart');
 }finally{
  await stop('SIGTERM');
  if(session)await c.call('session.destroy',{session_id:session}).catch(()=>{});
  await writeFile(dir+'/hub.log',logs);c.close();console.log('Evidence: '+dir);
 }
}
