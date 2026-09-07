// Explicit opt-in integration against an isolated local instance. Never writes
// production tasks, members, native sessions or database files.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import net from 'node:net';
import {spawn} from 'node:child_process';
import {once} from 'node:events';
import {randomUUID} from 'node:crypto';
import {setTimeout as delay} from 'node:timers/promises';
import assert from 'node:assert/strict';
const root=path.resolve(process.argv[2]||'.');
const directory=fs.mkdtempSync(path.join(os.tmpdir(),'wa-reasoning-smoke-'));
const listener=net.createServer();listener.listen(0,'127.0.0.1');await once(listener,'listening');
const port=listener.address().port;await new Promise(resolve=>listener.close(resolve));
const env=Object.fromEntries(Object.entries(process.env).filter(([key])=>!key.startsWith('ASSISTANT_')));
env.ASSISTANT_BACKUP_DIR=path.join(directory,'backups');
const child=spawn(path.join(root,'bin/assistant-local'),['--data',path.join(directory,'data'),'--listen','127.0.0.1:'+port,'--runtime-id','reasoning-smoke','--adapter','codex-agent','--extra-adapters','cursor-agent','--codex-binary','/home/wangyunlai.wyl/.n/bin/codex','--cursor-binary','/home/wangyunlai.wyl/.local/bin/agent','--model','gpt-5.6-sol'],{cwd:root,env,stdio:['ignore','ignore','ignore']});
const api=async(route,method='GET',body,want=200)=>{
  const response=await fetch('http://127.0.0.1:'+port+'/api/v1'+route,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(12000)});
  if(want!==null)assert.equal(response.status,want,route+' HTTP status');
  else assert.ok(response.ok,route+' HTTP '+response.status);
  return response.json();
};
const waitFor=async(check,timeout)=>{const start=Date.now();while(Date.now()-start<timeout){if(child.exitCode!==null)throw Error('isolated service exited');const result=await check();if(result)return result;await delay(700);}throw Error('isolated check timed out');};
try{
  await waitFor(async()=>{try{return(await api('/system')).eligible_agents===1;}catch{return false;}},20000);
  const catalog=await api('/runtimes/reasoning-smoke/models?adapter_id=codex-agent');
  const model=catalog.models.find(m=>m.id==='gpt-5.6-sol');
  for(const value of ['low','high'])assert.ok(model?.reasoning_efforts.some(e=>e.id===value));
  const cursor=await api('/runtimes/reasoning-smoke/models?adapter_id=cursor-agent');
  assert.equal(cursor.status,'ready');assert.ok(cursor.models.some(m=>m.reasoning_efforts?.length>1));
  let member=(await api('/agents')).agents[0];
  const invalid={name:member.name,model_id:member.model_id,reasoning_effort:'not-supported',max_concurrent:member.max_concurrent,state:member.state,expected_version:member.version};
  await api('/agents/'+member.agent_id,'PUT',invalid,400);
  assert.deepEqual(await api('/agents/'+member.agent_id),member);
  const created=await api('/agents','POST',{name:'isolated-explicit-effort',role_id:member.role_id,runtime_id:member.runtime_id,adapter_id:member.adapter_id,model_id:member.model_id,reasoning_effort:'high',max_concurrent:1},201);
  assert.equal(created.reasoning_effort,'high');
  member=await api('/agents/'+member.agent_id,'PUT',{...invalid,reasoning_effort:'low'});
  const marker=randomUUID();
  const task=await api('/work/tasks','POST',{title:'隔离验证：推理强度与原 Session',goal:'不要使用工具或读取文件。记住校验标记 '+marker+'，只交付 ready.txt，内容为 READY。不要把标记写入交付文件或可见消息。按工作协议返回 review 和简短总结。',agent_id:member.agent_id,idempotency_key:randomUUID()},201);
  const route='/work/tasks/'+task.task_id;
  const first=await waitFor(async()=>{const d=await api(route);if(d.detail.task.state==='BLOCKED')throw Error('isolated delivery blocked');return d.work.reviews.length?d:false;},180000);
  const review=first.work.reviews[0],delivery=first.detail.runs.find(r=>r.run_id===review.run_id);
  assert.equal(delivery.reasoning_effort,'low');assert.equal(delivery.execution_configured,true);assert.equal(delivery.execution_model_id,member.model_id);
  assert.ok(first.work.artifacts.some(a=>a.name==='ready.txt'&&a.content.includes('READY')));
  console.log('Real delivery with low effort and persisted execution settings verified');
  member=await api('/agents/'+member.agent_id,'PUT',{...invalid,reasoning_effort:'high',expected_version:member.version});
  const question=await api(route+'/reviews/'+review.review_id+'/messages','POST',{message:'前一轮让你记住的校验标记是什么？只在 message 中回复标记，不修改产物。',idempotency_key:randomUUID(),expected_discussion_version:review.discussion_version},202);
  const done=await waitFor(async()=>{const d=await api(route),turn=d.work.review_turns.find(t=>t.turn_id===question.turn_id);if(turn?.state==='FAILED')throw Error('isolated review failed');return turn?.state==='COMPLETED'?{d,turn}:false;},180000);
  const chat=done.d.detail.runs.find(r=>r.run_id===done.turn.run_id);
  assert.equal(chat.reasoning_effort,'high');assert.equal(chat.execution_configured,true);
  assert.ok(done.turn.answer.includes(marker),'original native context was lost');
  assert.equal(chat.session_id,delivery.session_id);assert.equal(chat.model_id,delivery.model_id);
  assert.equal(done.d.detail.session.agent_session_ref,first.detail.session.agent_session_ref);
  assert.deepEqual(done.d.work.artifacts,first.work.artifacts);
  const latest=done.d.work.reviews.find(r=>r.review_id===review.review_id);
  // Acceptance uses the actual endpoint contract; retain the test data as audit.
  await api(route+'/reviews/'+review.review_id,'POST',{decision:'APPROVED',comment:'isolated validation',expected_discussion_version:latest.discussion_version},null);
  const final=await api(route);assert.equal(final.detail.summary.executor.reasoning_effort,'low');
  console.log(JSON.stringify({isolated:true,directory,memberCreateAndUpdate:true,invalidRejected:true,deliveryEffort:'low',reviewEffort:'high',sameNativeSession:true,immutableArtifacts:true,summaryEffort:'low'}));
}finally{
  if(child.exitCode===null&&child.signalCode===null){const stopped=once(child,'exit');child.kill('SIGTERM');const timeout=setTimeout(()=>child.kill('SIGKILL'),12000);try{await stopped;}finally{clearTimeout(timeout);}}
}
