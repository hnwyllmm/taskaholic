// Run on dev after deploying schema 15. Preview is read-only; --apply appends
// the QA policy using CAS, creates the collection source if absent, and places
// existing GitLab auth in a private host file. No pipeline is started here.
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import {fileURLToPath} from 'node:url';
import {rolePack} from './seekdb-roles.mjs';

export async function configureQAPipelines(api,{apply=false}={}){
  const [{roles},{sources}]=await Promise.all([api('/roles'),api('/sources')]);
  const template=rolePack.find(p=>p.key==='seekdb-qa-reviewer').spec;
  const matches=roles.filter(r=>r.name===template.name&&r.capabilities.includes('qa.review'));
  if(matches.length!==1)throw Error('Expected one exact SeekDB QA role; refusing an ambiguous update');
  const role=matches[0],heading='## 大范围 PR 回归测试',addition=template.instructions.slice(template.instructions.indexOf(heading));
  if(role.instructions.includes(heading)&&!role.instructions.endsWith(addition))throw Error('QA pipeline policy already edited; inspect before updating');
  const source=sources.find(s=>s.source_id==='gitlab-seekdb-tests');
  if(source&&source.kind!=='gitlab')throw Error('Reserved source identity already occupied');
  const update=!role.instructions.endsWith(addition);
  const plan={role_id:role.role_id,expected_version:role.version,role_action:update?'append-policy':'retain',source_action:source?'retain':'create',starts_pipeline:false};
  if(!apply)return plan;
  if(update){
    const spec=Object.fromEntries(['name','description','capabilities','instructions','output_contract','boundaries'].map(k=>[k,role[k]]));
    spec.instructions+='\n'+addition;
    await api('/roles/'+encodeURIComponent(role.role_id),'PUT',{expected_version:role.version,spec});
  }
  if(!source)await api('/sources/gitlab-seekdb-tests','PUT',{expected_version:0,source:{source_id:'gitlab-seekdb-tests',kind:'gitlab',name:'GitLab 回归测试',enabled:true,interval_seconds:15,config:{}}});
  return plan;
}

async function main(){
  if(process.cwd()!=='/data/wangyunlai.wyl/workspace/work-assistant')throw Error('Run from the deployed dev workspace');
  const apply=process.argv.includes('--apply');
  const api=async(route,method='GET',body)=>{
    const response=await fetch('http://127.0.0.1:17343/api/v1'+route,{method,headers:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(15000)});
    if(!response.ok)throw Error('Control API HTTP '+response.status+' at '+route);
    return response.json();
  };
  const config=path.join(os.homedir(),'.config','work-assistant'),file=path.join(config,'gitlab.token');
  const exists=fs.existsSync(file);
  if(exists&&(!fs.lstatSync(file).isFile()||fs.lstatSync(file).isSymbolicLink()))throw Error('Credential path must be a regular private file');
  const token=exists?fs.readFileSync(file,'utf8').trim():(process.env.GITLAB_TOKEN||process.env.GITLAB_PRIVATE_TOKEN||'').trim();
  if(!token)throw Error('Existing dev GitLab credential is missing');
  const response=await fetch('https://gitlab.oceanbase-dev.com/api/v4/projects/obqa%2Fseekdb_test',{headers:{'PRIVATE-TOKEN':token},redirect:'error',signal:AbortSignal.timeout(15000)});
  if(!response.ok)throw Error('GitLab credential verification HTTP '+response.status);
  const project=await response.json();if(project.path_with_namespace!=='obqa/seekdb_test')throw Error('Wrong test repository');
  const plan=await configureQAPipelines(api);
  if(apply){
    if(!exists){fs.mkdirSync(config,{recursive:true,mode:0o700});const fd=fs.openSync(file,'wx',0o600);try{fs.writeFileSync(fd,token+'\n');fs.fsyncSync(fd);}finally{fs.closeSync(fd);}}
    if((fs.statSync(file).mode&0o077)!==0)throw Error('GitLab credential file must be private (0600)');
    await configureQAPipelines(api,{apply:true});
  }
  console.log(JSON.stringify({...plan,applied:apply,credential:exists?'retained':apply?'installed-private-file':'would-install-private-file',gitlab_project_id:project.id}));
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url))main().catch(error=>{console.error(error.message);process.exitCode=1;});
