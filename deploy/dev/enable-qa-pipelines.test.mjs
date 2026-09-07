import {test} from 'node:test';
import assert from 'node:assert/strict';
import {configureQAPipelines} from './enable-qa-pipelines.mjs';
import {rolePack} from './seekdb-roles.mjs';
test('QA policy upgrade appends with CAS, preserves edits and source choices, and replays without writes',async()=>{
  const template=rolePack.find(p=>p.key==='seekdb-qa-reviewer').spec;
  let role={...structuredClone(template),role_id:'qa',version:4,instructions:'My custom QA checks'},writes=[];
  const source={source_id:'gitlab-seekdb-tests',kind:'gitlab',enabled:false,interval_seconds:60,version:3,config:{}};
  const api=async(route,method='GET',body)=>{
    if(method==='GET')return route==='/roles'?{roles:[structuredClone(role)]}:{sources:[structuredClone(source)]};
    writes.push({route,body});assert.equal(route,'/roles/qa');assert.equal(body.expected_version,role.version);role={...role,...body.spec,version:role.version+1};return role;
  };
  const plan=await configureQAPipelines(api);assert.equal(plan.role_action,'append-policy');assert.equal(writes.length,0);
  await configureQAPipelines(api,{apply:true});assert.equal(writes.length,1);assert.ok(role.instructions.startsWith('My custom QA checks\n'));
  assert.equal(role.version,5);assert.equal(source.enabled,false);assert.deepEqual(role.boundaries,template.boundaries);
  await configureQAPipelines(api,{apply:true});assert.equal(writes.length,1);
});
