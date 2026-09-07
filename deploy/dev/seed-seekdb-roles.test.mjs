import {test} from 'node:test';
import assert from 'node:assert/strict';
import {rolePack} from './seekdb-roles.mjs';
import {seedRoles} from './seed-seekdb-roles.mjs';

function fixture(){
  const roles=[],agents=[],drafts=new Map(),writes=[];
  const api=async(route,method='GET',body)=>{
    if(method!=='GET')writes.push([route,method]);
    if(route==='/roles')return {roles};
    if(route==='/runtimes')return {runtimes:[{runtime_id:'dev',state:'ONLINE',capabilities:{adapters:{'codex-agent':{role_instructions:true}}}}]};
    if(route.startsWith('/runtimes/'))return {status:'ready',models:[{id:'fixture-model'}]};
    if(route==='/system/agents')return {bindings:[{slot:'home_chat',agent_id:'original-cursor'}]};
    if(route==='/agents'){
      if(method==='GET')return {agents};
      const a={...body,agent_id:'a'+agents.length};agents.push(a);return a;
    }
    if(route==='/role-drafts'){
      if(drafts.has(body.idempotency_key))return drafts.get(body.idempotency_key);
      const d={draft_id:'d'+drafts.size,state:'DRAFT',messages:[],spec:{description:body.description},version:1};drafts.set(body.idempotency_key,d);return d;
    }
    const draft=[...drafts.values()].find(d=>route.startsWith('/role-drafts/'+d.draft_id));
    if(draft){
      assert.equal(body.expected_version,draft.version);
      if(method==='PUT'){draft.spec=body.spec;draft.version++;return draft;}
      const role={...draft.spec,role_id:'r'+roles.length,version:1};roles.push(role);draft.state='PUBLISHED';draft.published_role_id=role.role_id;return role;
    }
    throw Error('unexpected request '+route);
  };
  return {api,roles,agents,writes};
}

test('role pack covers requested responsibilities and does not grant permissions',()=>{
  assert.equal(rolePack.length,5);
  assert.equal(new Set(rolePack.map(r=>r.key)).size,5);
  const developer=rolePack.find(r=>r.key==='seekdb-developer').spec;
  assert.equal(developer.name,'SeekDB / seekdb-bindings 开发者');
  assert.ok(developer.capabilities.includes('seekdb.develop'));
  assert.ok(developer.capabilities.includes('seekdb-bindings.develop'));
  assert.ok(developer.capabilities.includes('package.validate'));
  assert.equal(rolePack.filter(r=>r.key==='seekdb-bindings-developer').length,0);
  for(const {spec} of rolePack){
    assert.ok(Buffer.byteLength(spec.name)<=200);
    assert.ok(Buffer.byteLength(spec.instructions)<=32000);
    assert.ok(Buffer.byteLength(spec.output_contract)<=8000);
    assert.ok(spec.boundaries.length>0);
    assert.ok(spec.instructions.includes('只读'));
    for(const capability of spec.capabilities)assert.match(capability,/^[a-z][a-z0-9._-]*$/);
  }
  const general=rolePack.find(r=>r.key==='seekdb-general-reviewer').spec.instructions;
  for(const term of ['完整阅读','code-review','过度防御','无谓单测','无法读取'])assert.ok(general.includes(term));
  const qa=rolePack.find(r=>r.key==='seekdb-qa-reviewer').spec.instructions;
  for(const term of ['可测试性','实际命令','最终二进制','证据','测试场景'])assert.ok(qa.includes(term));
});
test('seed defaults to read-only preview, creates five roles/members, and replays without writes',async()=>{
  const f=fixture(),opts={members:true,modelID:'fixture-model'};
  assert.equal((await seedRoles(f.api,opts)).length,5);assert.equal(f.writes.length,0);
  assert.equal((await seedRoles(f.api,{...opts,apply:true})).length,5);
  assert.equal(f.roles.length,5);assert.equal(f.agents.length,5);
  const count=f.writes.length;
  await seedRoles(f.api,{...opts,apply:true});assert.equal(f.writes.length,count);
  assert.ok(f.agents.every(a=>a.adapter_id==='codex-agent'&&a.max_concurrent===1));
});
test('user edits and unverified models fail before any writes',async()=>{
  const f=fixture();
  await assert.rejects(seedRoles(f.api,{apply:true,members:true,modelID:'unknown'}),/live Codex catalog/);
  assert.equal(f.writes.length,0);
  f.roles.push({...rolePack.find(r=>r.key==='seekdb-general-reviewer').spec,instructions:'user customized',role_id:'existing'});
  await assert.rejects(seedRoles(f.api,{apply:true}),/Existing role differs/);
  assert.equal(f.writes.length,0);
});
