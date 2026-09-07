// Deployment-specific seed. No platform writes; existing source configurations
// are never overwritten. Without --apply this only prints a preview.
import assert from 'node:assert/strict';
const base='http://127.0.0.1:17343/api/v1';
const call=async(route,method='GET',body)=>{
  const token=process.env.WORK_ASSISTANT_API_TOKEN;
  const response=await fetch(base+route,{method,headers:{'Content-Type':'application/json',...(token?{Authorization:'Bearer '+token}:{})},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(12000)});
  assert.ok(response.ok,route+' HTTP '+response.status);
  return response.json();
};
const existing=await call('/sources');
const sources=[
  {source_id:'github',kind:'github',name:'GitHub PR 跟踪',enabled:true,interval_seconds:5,config:{ignore_logins:[]}},
  {source_id:'antmultica-seekdb',kind:'antmultica',name:'AntMultica · 我的 SeekDB 工单',enabled:true,interval_seconds:60,config:{workspace_id:'44029359-53fc-4a6a-bd6f-aae91c2bd754',workspace_slug:'seekdb',assignee_id:'51410f21-5e32-490e-875d-eb3f28fb4095',iteration_key:'迭代',iteration_value:'1.5.0'}},
];
const missing=sources.filter(source=>!existing.sources?.some(s=>s.source_id===source.source_id));
if(process.argv.includes('--apply')){
  for(const source of missing){
    await call('/sources/'+source.source_id,'PUT',{source,expected_version:0});
    console.log('Created '+source.source_id);
  }
  console.log(JSON.stringify({created:missing.map(s=>s.source_id),preservedExisting:true,externalPlatformWrites:false}));
}else{
  console.log(JSON.stringify({preview:true,sources:missing},null,2));
}
