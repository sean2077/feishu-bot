/** Trusted HOST-side recipes. Never copy this executor or service secrets into a worker.
 * Replace these recipes with existing trusted CLIs where available. No arbitrary URL,
 * project, job, repository, base branch, or HTTP method is accepted from the agent.
 */
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';

const [operation,...args]=process.argv.slice(2);
function required(name){const value=process.env[name];if(!value)throw new Error(`Missing setting: ${name}`);return value;}
function endpoint(name){
  const url=new URL(required(name));
  const localTest=process.env.BOT_ACTION_TEST_HTTP==='1'&&['127.0.0.1','localhost'].includes(url.hostname);
  if((url.protocol!=='https:'&&!localTest)||url.username||url.password||url.search||url.hash)throw new Error(`Invalid endpoint: ${name}`);
  return url.toString().replace(/\/$/,'');
}
async function request(url,options={}){
  const response=await fetch(url,{...options,redirect:'manual',signal:AbortSignal.timeout(45000)});
  if(!response.ok){await response.body?.cancel();throw new Error(`Service returned HTTP ${response.status}; writes are not retried`);}
  const chunks=[];let size=0;
  if(response.body)for await(const bytes of response.body){size+=bytes.length;if(size>1024*1024){throw new Error('Service response exceeds limit');}chunks.push(Buffer.from(bytes));}
  const text=Buffer.concat(chunks).toString();return {data:text?JSON.parse(text):{},location:response.headers.get('location')};
}
function json(value){return {'content-type':'application/json',...value};}
async function jenkins(){
  const base=endpoint('JENKINS_URL');
  const job=required('JENKINS_JOB').split('/').map(segment=>'/job/'+encodeURIComponent(segment)).join('');
  const headers={authorization:'Basic '+Buffer.from(required('JENKINS_USER')+':'+required('JENKINS_TOKEN')).toString('base64')};
  if(operation==='jenkins-status'){
    const {data}=await request(base+job+'/lastBuild/api/json?tree=number,result,building,url,duration,timestamp',{headers});return data;
  }
  const revision=args[0];if(!revision||!/^[A-Za-z0-9][A-Za-z0-9_./-]{0,99}$/.test(revision))throw new Error('Invalid revision');
  // API-token authentication is used; password/cookie authentication is not supported.
  const result=await request(base+job+'/buildWithParameters?'+new URLSearchParams({REVISION:revision}),{method:'POST',headers});
  return {queued:true,queueUrl:result.location};
}
async function gitlabPublish(){
  const [patch,title]=args;if(!patch||!title||title.length>200)throw new Error('Expected a snapshotted patch and a short MR title');
  const base=endpoint('GITLAB_URL'),repo=endpoint('GITLAB_REPO_URL');
  if(new URL(base).origin!==new URL(repo).origin)throw new Error('Repository and GitLab API must share an origin');
  const branch='feishu-bot/'+randomUUID();const target=required('GITLAB_BASE_BRANCH');
  if(!/^[A-Za-z0-9][A-Za-z0-9_./-]{0,99}$/.test(target))throw new Error('Invalid configured base branch');
  const root=await mkdtemp(join(tmpdir(),'feishu-publish-'));const work=join(root,'checkout'),hooks=join(root,'empty-hooks');await mkdir(hooks);
  const token=required('GITLAB_TOKEN');
  const env={...process.env,HOME:root,USERPROFILE:root,GIT_CONFIG_NOSYSTEM:'1',GIT_CONFIG_GLOBAL:process.platform==='win32'?'NUL':'/dev/null',GIT_TERMINAL_PROMPT:'0',GIT_CONFIG_COUNT:'1',GIT_CONFIG_KEY_0:`http.${new URL(base).origin}/.extraHeader`,GIT_CONFIG_VALUE_0:'Authorization: Basic '+Buffer.from('oauth2:'+token).toString('base64')};
  const git=parameters=>new Promise((resolve,reject)=>{
    const child=spawn(process.env.GIT_EXECUTABLE||'git',['-c',`core.hooksPath=${hooks}`,'-c','protocol.file.allow=never','-c','protocol.ext.allow=never','-c','commit.gpgSign=false',...parameters],{env,stdio:'ignore',shell:false,windowsHide:true});
    child.once('error',()=>reject(new Error('Cannot start trusted git executable')));child.once('close',code=>code===0?resolve():reject(new Error(`git operation failed (${code}); inspect branch ${branch} before retrying`)));
  });
  try{
    await git(['clone','--no-recurse-submodules','--single-branch','--depth','1','--branch',target,'--',repo,work]);
    await git(['-C',work,'checkout','-b',branch]);
    await git(['-C',work,'apply','--check','--',patch]);
    await git(['-C',work,'apply','--index','--',patch]);
    await git(['-C',work,'-c','user.name=Feishu Project Bot','-c','user.email=feishu-bot@example.invalid','commit','-m',title]);
    await git(['-C',work,'push','origin',`HEAD:refs/heads/${branch}`]);
    const {data}=await request(base+'/api/v4/projects/'+encodeURIComponent(required('GITLAB_PROJECT_ID'))+'/merge_requests',{method:'POST',headers:json({'PRIVATE-TOKEN':token}),body:JSON.stringify({source_branch:branch,target_branch:target,title,remove_source_branch:true})});
    return {url:data.web_url,iid:data.iid,branch};
  }finally{await rm(root,{recursive:true,force:true,maxRetries:5,retryDelay:200});}
}
async function feishu(){
  const base='https://open.feishu.cn/open-apis';
  const auth=await request(base+'/auth/v3/tenant_access_token/internal',{method:'POST',headers:json({}),body:JSON.stringify({app_id:required('FEISHU_ACTION_APP_ID'),app_secret:required('FEISHU_ACTION_APP_SECRET')})});
  if(auth.data.code!==0||!auth.data.tenant_access_token)throw new Error('Feishu service identity authentication failed');
  const headers=json({authorization:'Bearer '+auth.data.tenant_access_token});let path,method='GET',body;
  const encode=encodeURIComponent;
  if(operation==='feishu-doc-read')path='/docx/v1/documents/'+encode(required('FEISHU_DOCUMENT_ID'))+'/raw_content';
  else if(operation==='feishu-doc-append'){
    if(!args[0]||args[0].length>10000)throw new Error('Invalid document text');method='POST';
    path='/docx/v1/documents/'+encode(required('FEISHU_DOCUMENT_ID'))+'/blocks/'+encode(required('FEISHU_DOCUMENT_ROOT_BLOCK'))+'/children';
    body={children:[{block_type:2,text:{elements:[{text_run:{content:args[0]}}]}}]};
  }else{
    path='/bitable/v1/apps/'+encode(required('FEISHU_BASE_TOKEN'))+'/tables/'+encode(required('FEISHU_TABLE_ID'))+'/records';
    if(operation==='feishu-table-list')path+='?page_size=50';
    else{
      if(!/^rec[A-Za-z0-9]+$/.test(args[0]||''))throw new Error('Invalid record ID');
      const fields=JSON.parse(args[1]||'{}'),allowed=new Set(required('FEISHU_WRITABLE_FIELDS').split(','));
      if(!fields||Array.isArray(fields)||Object.keys(fields).some(key=>!allowed.has(key)))throw new Error('Field is outside configured write scope');
      method='PUT';path+='/'+encode(args[0]);body={fields};
    }
  }
  const result=await request(base+path,{method,headers,body:body?JSON.stringify(body):undefined});
  if(result.data.code!==0)throw new Error(`Feishu API error ${result.data.code}`);
  return result.data.data;
}
try{
  let result;
  if(['jenkins-status','jenkins-build'].includes(operation))result=await jenkins();
  else if(operation==='gitlab-publish')result=await gitlabPublish();
  else if(['feishu-doc-read','feishu-doc-append','feishu-table-list','feishu-table-update'].includes(operation))result=await feishu();
  else throw new Error('Unknown configured operation');
  console.log(JSON.stringify(result));
}catch(error){console.error(error instanceof Error?error.message:'Operation failed');process.exitCode=1;}
