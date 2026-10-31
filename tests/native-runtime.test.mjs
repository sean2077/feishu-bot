import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';

// This runs the REAL pinned Claude Code binary against a deterministic local
// Anthropic-shaped server. It validates the harness, not DeepSeek's reasoning.
test('native CC consumes a busy follow-up, approves a write and continues the same process', {timeout:60000}, async()=>{
  const root=await mkdtemp(join(tmpdir(),'feishu native 中文 '));
  const work=join(root,'工作目录'),home=join(root,'home');
  await mkdir(work);await mkdir(home);
  const target=join(work,'审批 文件.txt');
  const requests=[];let calls=0;
  const server=http.createServer(async(req,res)=>{
    const chunks=[];for await(const chunk of req)chunks.push(chunk);
    const body=JSON.parse(Buffer.concat(chunks).toString());
    if(req.url.includes('count_tokens')){res.writeHead(200,{'content-type':'application/json'});res.end('{"input_tokens":500}');return;}
    requests.push(body);calls++;
    const content=calls===1?{type:'tool_use',id:'toolu_fixture',name:'Write',input:{file_path:target,content:'approved by maintainer'}}:{type:'text',text:'Native fixture completed.'};
    const stop=content.type==='tool_use'?'tool_use':'end_turn';
    const message={id:'msg_'+calls,type:'message',role:'assistant',model:'company-test-model',content:[content],stop_reason:stop,stop_sequence:null,usage:{input_tokens:500,output_tokens:30}};
    if(!body.stream){res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify(message));return;}
    res.writeHead(200,{'content-type':'text/event-stream'});
    const event=(type,data)=>res.write(`event: ${type}\ndata: ${JSON.stringify({type,...data})}\n\n`);
    event('message_start',{message:{...message,content:[],stop_reason:null,usage:{input_tokens:500,output_tokens:0}}});
    event('content_block_start',{index:0,content_block:content.type==='text'?{type:'text',text:''}:{...content,input:{}}});
    event('content_block_delta',{index:0,delta:content.type==='text'?{type:'text_delta',text:content.text}:{type:'input_json_delta',partial_json:JSON.stringify(content.input)}});
    event('content_block_stop',{index:0});event('message_delta',{delta:{stop_reason:stop,stop_sequence:null},usage:{output_tokens:30}});event('message_stop',{});res.end();
  });
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  const env={PATH:process.env.PATH,HOME:home,USERPROFILE:home,CLAUDE_CONFIG_DIR:join(home,'.claude')};
  for(const key of ['SystemRoot','WINDIR','COMSPEC','PATHEXT','TEMP','TMP','CLAUDE_CODE_GIT_BASH_PATH'])if(process.env[key])env[key]=process.env[key];
  const child=spawn(process.execPath,[resolve('dist/worker.js')],{cwd:work,env,stdio:['pipe','pipe','pipe']});
  let stderr='',buffer='',failure;const events=[];
  const send=frame=>child.stdin.write(JSON.stringify(frame)+'\n');
  const delivery=(text,sender='alice')=>({uuid:randomUUID(),roomKey:'room',sessionId:'session',message:{id:randomUUID(),chatId:'chat',chatType:'group',senderId:sender,text,time:Date.now(),mentioned:true,resources:[]},background:[],attachments:[]});
  const first=delivery('FIRST_NATIVE_REQUEST'),second=delivery('BUSY_CORRECTION_FROM_BOB','bob'),third=delivery('THIRD_TURN_CONTINUATION');
  let resultCount=0,permissionCount=0;
  child.stderr.setEncoding('utf8');child.stderr.on('data',chunk=>stderr=(stderr+chunk).slice(-10000));
  child.stdout.setEncoding('utf8');child.stdout.on('data',chunk=>{
    buffer+=chunk;
    for(;;){const i=buffer.indexOf('\n');if(i<0)break;const line=buffer.slice(0,i);buffer=buffer.slice(i+1);
      try{
        const event=JSON.parse(line);events.push(event);
        if(event.type==='error')failure=new Error(event.message+'\n'+stderr);
        if(event.type==='request'){
          permissionCount++;assert.equal(event.kind,'permission');assert.equal(event.name,'Write');
          send({type:'input',delivery:second});send({type:'response',id:event.id,value:{approved:true}});
        }
        if(event.type==='result'){
          assert.equal(event.success,true,stderr);resultCount++;
          if(resultCount===1){assert.ok(event.uuids.includes(first.uuid),'first input is attributed');assert.ok(event.uuids.includes(second.uuid),'busy input is attributed in the same result');send({type:'input',delivery:third});}
          else send({type:'stop'});
        }
      }catch(error){failure=error;child.kill();}
    }
  });
  const deadline=setTimeout(()=>{failure=new Error('native fixture timeout\n'+stderr);child.kill();},50000);
  try{
    send({type:'init',sessionId:'session',workDir:work,homeDir:home,autoApproveWorkspace:false,queueLimit:64,model:{url:`http://127.0.0.1:${server.address().port}`,token:'fixture-not-a-real-key',model:'company-test-model',smallModel:'company-test-model'},actions:[]});
    send({type:'input',delivery:first});
    const code=await new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',resolve);});
    if(failure)throw failure;
    assert.equal(code,0,stderr);assert.equal(permissionCount,1);assert.equal(resultCount,2);
    assert.equal(new Set(events.filter(e=>e.type==='ready').map(e=>e.ccId)).size,1);
    assert.equal(await readFile(target,'utf8'),'approved by maintainer');
    assert.ok(JSON.stringify(requests[1]).includes('BUSY_CORRECTION_FROM_BOB'));
    assert.ok(JSON.stringify(requests.at(-1)).includes('FIRST_NATIVE_REQUEST'));
    assert.ok(requests[0].tools.some(t=>t.name==='Agent'));
    assert.ok(requests[0].tools.some(t=>t.name==='Skill'));
    assert.ok(requests[0].tools.some(t=>t.name==='ScheduleWakeup'));
    assert.equal(events.some(e=>e.type==='thinking'),false);
  }finally{clearTimeout(deadline);child.kill();server.closeAllConnections();await new Promise(r=>server.close(r));await rm(root,{recursive:true,force:true,maxRetries:5,retryDelay:200});}
});
