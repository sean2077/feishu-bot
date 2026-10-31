import assert from 'node:assert/strict';
import http from 'node:http';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { parseConfig } from '../dist/config.js';
import { runtimeFactory,cleanupOwnedContainers,instanceLabel } from '../dist/runtime.js';
import { runCommand } from '../dist/actions.js';

// Real Docker boundary + real CC binary, with a deterministic local provider.
const dir=await mkdtemp(join(tmpdir(),'fb-docker-'));
const secretPath=join(dir,'host-private.txt');await writeFile(secretPath,'not mounted');
const script=`(async()=>{const a=require('node:assert/strict'),f=require('node:fs');a.equal(f.existsSync(${JSON.stringify(secretPath)}),false);a.equal(f.existsSync('/var/run/docker.sock'),false);a.equal(process.env.COMPANY_MODEL_TOKEN,undefined);a.equal(process.env.FEISHU_APP_SECRET,undefined);a.throws(()=>f.writeFileSync('/attachments/not-allowed','x'));a.throws(()=>f.writeFileSync('/usr/not-allowed','x'));await a.rejects(fetch('http://192.0.2.1',{signal:AbortSignal.timeout(500)}));console.log('ISOLATION_PASS');})().catch(e=>{console.error(e);process.exitCode=1;})`;
const command='node -e '+"'"+script.replaceAll("'","'\\''")+"'";
let requests=0,verified=false,authSeen=false;
const server=http.createServer(async(req,res)=>{
  const chunks=[];for await(const chunk of req)chunks.push(chunk);const body=JSON.parse(Buffer.concat(chunks).toString());
  authSeen||=req.headers.authorization==='Bearer host-only-fixture';
  if(req.url.includes('count_tokens')){res.writeHead(200,{'content-type':'application/json'});res.end('{"input_tokens":500}');return;}
  requests++;
  if(requests>1){
    const results=body.messages.filter(m=>m.role==='user'&&Array.isArray(m.content)).flatMap(m=>m.content).filter(c=>c.type==='tool_result'&&c.tool_use_id==='isolation-tool');
    verified=results.some(c=>!c.is_error&&/^ISOLATION_PASS(?:\n|$)/m.test(typeof c.content==='string'?c.content:c.content.map(v=>v.text||'').join('\n')));
  }
  const content=requests===1?{type:'tool_use',id:'isolation-tool',name:'Bash',input:{command,description:'Verify execution boundary'}}:{type:'text',text:verified?'ISOLATION_VERIFIED':'ISOLATION_FAILED'};
  const stop=content.type==='tool_use'?'tool_use':'end_turn';
  res.writeHead(200,{'content-type':'text/event-stream'});const event=(type,data)=>res.write(`event: ${type}\ndata: ${JSON.stringify({type,...data})}\n\n`);
  event('message_start',{message:{id:'msg_'+requests,type:'message',role:'assistant',model:'fixture-model',content:[],stop_reason:null,stop_sequence:null,usage:{input_tokens:500,output_tokens:0}}});
  event('content_block_start',{index:0,content_block:content.type==='text'?{type:'text',text:''}:{...content,input:{}}});
  event('content_block_delta',{index:0,delta:content.type==='text'?{type:'text_delta',text:content.text}:{type:'input_json_delta',partial_json:JSON.stringify(content.input)}});
  event('content_block_stop',{index:0});event('message_delta',{delta:{stop_reason:stop,stop_sequence:null},usage:{output_tokens:40}});event('message_stop',{});res.end();
});await new Promise(r=>server.listen(0,'127.0.0.1',r));
process.env.COMPANY_MODEL_TOKEN='host-only-fixture';process.env.FEISHU_APP_SECRET='host-feishu-fixture';
const config=parseConfig({tenantId:'fixture',appId:'cli_fixture',dataDir:dir,runtime:{mode:'docker'},model:{baseUrl:`http://127.0.0.1:${server.address().port}`,allowHttp:true,model:'fixture-model'},projects:[{id:'p',maintainers:['admin']}],groups:[],dms:[]});
const room={key:'fixture-room',chatId:'fixture-chat',projectId:'p',maintainers:['admin']};
const session={id:randomUUID(),roomKey:room.key,state:'open',createdAt:Date.now()};
let worker,resolveResult,rejectResult;
const result=new Promise((resolve,reject)=>{resolveResult=resolve;rejectResult=reject;});
const deadline=setTimeout(()=>rejectResult(new Error('Docker native test timed out')),60000);
try{
  worker=await runtimeFactory(config,['host-only-fixture','host-feishu-fixture'],(event,detail)=>console.log(event,JSON.stringify(detail)))(room,session,event=>{
    if(event.type==='result')resolveResult(event);
    if(event.type==='error')rejectResult(new Error(event.message));
    if(event.type==='request')worker.respond(event.id,{approved:false,error:'Unexpected permission request in isolated fixture'});
  });
  await worker.send({uuid:randomUUID(),roomKey:room.key,sessionId:session.id,message:{id:randomUUID(),chatId:room.chatId,chatType:'group',senderId:'alice',text:'Run the isolation fixture.',time:Date.now(),mentioned:true,resources:[]},background:[],attachments:[]});
  const output=await result;assert.equal(output.success,true);assert.equal(output.text,'ISOLATION_VERIFIED');assert.equal(verified,true);assert.equal(authSeen,true);
  const containers=await runCommand('docker',['ps','-q','--filter',`label=io.feishu-bot.instance=${instanceLabel(config)}`]);assert.ok(containers.output.trim());
  const inspect=await runCommand('docker',['inspect','--format','{{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}}',containers.output.trim()]);assert.equal(inspect.output.trim(),'none true');
  console.log('Docker + native CC isolation fixture passed');
}finally{
  clearTimeout(deadline);await worker?.stop();await cleanupOwnedContainers(config).catch(()=>{});server.closeAllConnections();await new Promise(r=>server.close(r));await rm(dir,{recursive:true,force:true,maxRetries:5,retryDelay:200});
}
