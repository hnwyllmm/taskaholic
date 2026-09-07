import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {execFileSync} from 'node:child_process';
import {isDeepStrictEqual} from 'node:util';
import {rolePack} from './seekdb-roles.mjs';

const specOf = role => Object.fromEntries(['name','description','capabilities','instructions','output_contract','boundaries'].map(k=>[k,role[k]]));

// Additive and resumable. Never overwrite a user's role, member, or system slot.
export async function seedRoles(api, {apply=false,members=false,modelID='',runtimeID='dev-cursor'}={}) {
  const [{roles},{agents},{runtimes}] = await Promise.all([api('/roles'),api('/agents'),api('/runtimes')]);
  if(members) {
    const runtime=runtimes.find(r=>r.runtime_id===runtimeID);
    if(runtime?.state!=='ONLINE'||!runtime.capabilities?.adapters?.['codex-agent']?.role_instructions)throw Error('Codex runtime is not ready');
    const catalog=await api('/runtimes/'+encodeURIComponent(runtimeID)+'/models?adapter_id=codex-agent');
    if(modelID && (catalog.status!=='ready'||!catalog.models.some(m=>m.id===modelID)))throw Error('Requested model is not in the live Codex catalog');
  }
  const bindings=await api('/system/agents');
  // Validate every collision before the first write. Re-running never resets
  // an edited role to a bundled template or changes a member's chosen model.
  const plan=rolePack.map(({key,spec})=>{
    const matches=roles.filter(r=>r.name===spec.name);
    if(matches.length>1)throw Error('Ambiguous existing role: '+spec.name);
    const role=matches[0],name=spec.name+' · Codex';
    if(role&&!isDeepStrictEqual(specOf(role),spec))throw Error('Existing role differs; explicit review required: '+spec.name);
    const agent=agents.find(a=>a.name===name);
    if(members&&agent&&(!role||agent.role_id!==role.role_id||agent.runtime_id!==runtimeID||agent.adapter_id!=='codex-agent'||agent.model_id!==modelID))throw Error('Existing member differs; refusing to overwrite: '+name);
    return {key,spec,role,agent,name};
  });
  if(!apply)return plan.map(p=>({role:p.spec.name,role_action:p.role?'retain':'create',member_action:!members?'none':p.agent?'retain':'create',model:modelID||'runtime default'}));
  const result=[];
  for(const item of plan) {
    let role=item.role,agent=item.agent;
    if(!role) {
      let draft=await api('/role-drafts','POST',{description:item.spec.description,idempotency_key:'dev-seekdb-role-pack-v1:'+item.key});
      if(draft.state==='PUBLISHED') {
        role=await api('/roles/'+encodeURIComponent(draft.published_role_id));
        if(!isDeepStrictEqual(specOf(role),item.spec))throw Error('Published seed role was edited; refusing to overwrite');
      } else {
        if(draft.state!=='DRAFT'||draft.messages.length||draft.spec.name&&!isDeepStrictEqual(draft.spec,item.spec))throw Error('Seed draft has unexpected user changes; refusing to overwrite');
        if(!isDeepStrictEqual(draft.spec,item.spec))draft=await api('/role-drafts/'+draft.draft_id,'PUT',{expected_version:draft.version,spec:item.spec});
        role=await api('/role-drafts/'+draft.draft_id+'/publish','POST',{expected_version:draft.version});
      }
    }
    if(members&&!agent)agent=await api('/agents','POST',{name:item.name,role_id:role.role_id,runtime_id:runtimeID,adapter_id:'codex-agent',model_id:modelID,max_concurrent:1});
    result.push({name:item.spec.name,role_id:role.role_id,agent_id:members?agent.agent_id:undefined,model:members?agent.model_id:undefined});
  }
  if(!isDeepStrictEqual((await api('/system/agents')).bindings,bindings.bindings))throw Error('System bindings changed during seed; inspect before retrying');
  return result;
}

async function main() {
  const root='/data/wangyunlai.wyl/workspace/work-assistant';
  if(process.cwd()!==root)throw Error('Run from the dev deployment root; this seed must not target the Mac instance.');
  const args=process.argv.slice(2),apply=args.includes('--apply'),members=args.includes('--members');
  const modelIndex=args.indexOf('--model');
  if(args.some((v,i)=>!['--apply','--members','--model'].includes(v)&&!(modelIndex>=0&&i===modelIndex+1))||modelIndex>=0&&(!args[modelIndex+1]||args[modelIndex+1].startsWith('--')))throw Error('Usage: node deploy/dev/seed-seekdb-roles.mjs [--apply] [--members --model MODEL]');
  const modelID=modelIndex<0?'':args[modelIndex+1];
  const api=async(endpoint,method='GET',body)=>{
    const response=await fetch('http://127.0.0.1:17343/api/v1'+endpoint,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(15000)});
    if(!response.ok)throw Error(endpoint+' HTTP '+response.status);
    return response.json();
  };
  const options={members,modelID};
  const plan=await seedRoles(api,options);
  console.log(JSON.stringify({apply,plan},null,2));
  if(!apply)return;
  const status=await api('/admin/backups');
  const snapshot=async()=>{
    const point=await api('/admin/backups','POST');
    execFileSync(path.join(root,'bin/assistantctl'),['backup-verify','--from',path.join(status.directory,point.id)],{stdio:'pipe',timeout:45000});
    return point.id;
  };
  console.log('Verified pre-configuration backup:',await snapshot());
  console.log(JSON.stringify({configured:await seedRoles(api,{...options,apply:true})},null,2));
  console.log('Verified post-configuration backup:',await snapshot());
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url))await main();
