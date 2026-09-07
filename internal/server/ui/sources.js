'use strict';
const {el,api,notice,link,date}=WA;
const $=id=>document.getElementById(id);
const state={sources:[],targets:[],events:[],reviews:[],roles:[],projects:[],tasks:[],editing:null,busy:false,loading:false};
const eventLabels={'antmultica.issue':'工单新增 / 更新','github.head':'PR 新版本','github.comment':'PR 评论 / 评审意见','github.ci_failed':'CI 失败','github.review_result':'Agent 评审汇总','github.merged':'PR 已合并','github.closed':'PR 已关闭'};
const stateLabels={PENDING:'待处理',APPLIED:'已处理',RECORDED:'仅记录',SUPERSEDED:'旧版本',COMPLETED:'已交付'};
function options(select,items,idKey,empty){
  select.replaceChildren();
  if(empty!=null){const n=el('option',empty);n.value='';select.append(n);}
  for(const item of items){const n=el('option',item.name||item.title);n.value=item[idKey];select.append(n);}
}
async function action(fn){
  if(state.busy)return;
  state.busy=true;
  document.querySelectorAll('button').forEach(b=>b.disabled=true);
  notice('');
  try{await fn();}catch(e){
    notice(e.message,true);
    const error=$('source-dialog').open?$('source-error'):$('pr-dialog').open?$('pr-error'):null;
    if(error){error.textContent=e.message;error.hidden=false;}
  }finally{state.busy=false;document.querySelectorAll('button').forEach(b=>b.disabled=false);}
}
async function refresh(){
  if(state.loading)return;
  state.loading=true;
  try{
    const [data,roles,projects,tasks]=await Promise.all([api('/sources'),api('/roles'),api('/projects'),api('/work/tasks')]);
    for(const name of ['sources','targets','events','reviews'])state[name]=data[name]||[];
    state.roles=roles.roles||[];state.projects=projects.projects||[];state.tasks=tasks.tasks||[];
    $('connection').textContent='已连接';render();
  }finally{state.loading=false;}
}
function render(){
  $('sources').replaceChildren();
  const manual=el('article',null,'card');manual.append(el('span','手动输入','eyebrow'),el('h2','人工录入'),el('p','通过首页聊天或工作列表创建任务。'),link('创建工作 →','/tasks'));
  $('sources').append(manual);
  for(const s of state.sources){
    const c=el('article',null,'card');
    c.append(el('span',s.enabled?'已启用':'已停用','eyebrow'),el('h2',s.name),el('p',s.kind==='antmultica'?s.config.workspace_slug+' · 迭代 '+s.config.iteration_value+' · 精确指派人过滤':'跟踪已登记的 PR · '+(s.config.reviewer_role_ids?.length||0)+' 个评审角色'),el('p','每 '+s.interval_seconds+' 秒检查 · 错误或限流时自动退避'));
    const b=el('button','修改配置');b.onclick=()=>editSource(s);c.append(b);$('sources').append(c);
  }
  $('targets').replaceChildren();
  for(const t of state.targets){
    const source=state.sources.find(s=>s.source_id===t.source_id),c=el('article',null,'card source-row'),body=el('div');
    body.append(el('h3',t.task_id?t.entity:source?.name||t.entity),el('p',t.error?'连接需要检查':!source?.enabled?'任务源已停用':t.enabled?'正在跟踪':'已停止跟踪','source-meta'),el('p','最近成功：'+(t.last_success_ms?date(t.last_success_ms):'尚未成功')+' · 下次允许轮询：'+(t.next_poll_ms?date(t.next_poll_ms):'即将开始'),'source-meta'));
    if(t.head_sha)body.append(el('p','当前版本：'+t.head_sha,'source-meta'));
    if(t.error)body.append(el('p',t.error,'source-error'));
    const actions=el('div',null,'source-actions');
    if(t.task_id)actions.append(link('原任务 →','/tasks#'+encodeURIComponent(t.task_id)));
    const b=el('button',t.enabled?'暂停轮询':'恢复轮询');b.onclick=()=>action(async()=>{await api('/source-targets/'+encodeURIComponent(t.target_id),'PUT',{enabled:!t.enabled});await refresh();});actions.append(b);
    c.append(body,actions);$('targets').append(c);
  }
  if(!state.targets.length)$('targets').append(el('p','尚无轮询目标。添加 AntMultica 源或登记一个 PR。','empty-state'));
  $('reviews').replaceChildren();
  const batches=new Map();
  for(const r of state.reviews){const key=r.target_id+':'+r.head_sha;if(!batches.has(key))batches.set(key,[]);batches.get(key).push(r);}
  for(const batch of batches.values()){
    const target=state.targets.find(t=>t.target_id===batch[0].target_id),c=el('article',null,'card');
    c.append(el('h3',batch[0].head_sha.slice(0,12)+' · '+batch.filter(r=>r.state==='COMPLETED').length+'/'+batch.length+' 已交付'),el('p',target?.entity||batch[0].target_id,'source-meta'));
    for(const r of batch){const line=el('p');line.append(link(state.roles.find(x=>x.role_id===r.role_id)?.name||r.role_id,'/tasks#'+encodeURIComponent(r.task_id)),el('span',' · '+(stateLabels[r.state]||r.state),'source-meta'));c.append(line);}
    $('reviews').append(c);
  }
  if(!batches.size)$('reviews').append(el('p','PR 出现新版本后，这里会列出邀请的评审角色、任务和结果。','empty-state'));
  $('events').replaceChildren();
  for(const e of state.events){
    const c=el('article',null,'card source-row'),body=el('div');
    body.append(el('h3',eventLabels[e.kind]||e.kind),el('p',date(e.created_at_ms)+' · '+(stateLabels[e.state]||e.state)+' · '+(e.title||e.entity||''),'source-meta'));
    if(e.error)body.append(el('p',e.error,'source-error'));
    c.append(body);
    if(e.task_id)c.append(link('查看任务 →','/tasks#'+encodeURIComponent(e.task_id)));
    const details=el('details');details.append(el('summary','事件内容'),el('pre',e.message));c.append(details);$('events').append(c);
  }
  if(!state.events.length)$('events').append(el('p','暂无外部事件。没有更新时，不会创建新的任务。','empty-state'));
}
function editSource(source){
  state.editing=source;
  const c=source.config;
  $('source-form').reset();$('source-error').hidden=true;
  $('editor-title').textContent=source.version?'修改任务源':'添加任务源';
  $('source-name').value=source.name;$('source-enabled').value=String(source.enabled);$('source-interval').value=source.interval_seconds;
  $('multica-fields').hidden=source.kind!=='antmultica';$('github-fields').hidden=source.kind!=='github';
  for(const [id,key] of [['workspace-id','workspace_id'],['workspace-slug','workspace_slug'],['assignee-id','assignee_id'],['iteration-key','iteration_key'],['iteration-value','iteration_value']])$(id).value=c[key]||'';
  $('workspace-id').readOnly=!!source.version;$('assignee-id').readOnly=!!source.version;
  options($('source-role'),state.roles,'role_id','自动判断合适角色');$('source-role').value=c.role_id||'';
  options($('source-project'),state.projects,'project_id','不附加团队资料');$('source-project').value=c.project_id||'';
  $('defer-assignment').value=String(!!c.defer_assignment);$('ignore-logins').value=(c.ignore_logins||[]).join(', ');
  $('reviewer-roles').replaceChildren();
  for(const role of state.roles){const label=el('label'),input=el('input');input.type='checkbox';input.value=role.role_id;input.checked=(c.reviewer_role_ids||[]).includes(role.role_id);label.append(input,el('span',role.name));$('reviewer-roles').append(label);}
  $('source-dialog').showModal();
}
function newSource(kind){editSource({source_id:kind+'-'+WA.key(),kind,name:kind==='github'?'GitHub PR 跟踪':'AntMultica 工单',enabled:false,version:0,interval_seconds:kind==='github'?10:60,config:{workspace_slug:'seekdb',iteration_key:'迭代',iteration_value:'1.5.0'}});}
$('add-multica').onclick=()=>newSource('antmultica');$('add-github').onclick=()=>newSource('github');
$('close-source').onclick=()=>$('source-dialog').close();
$('source-form').onsubmit=e=>{e.preventDefault();action(async()=>{
  const original=state.editing,config={project_id:$('source-project').value};
  if(original.kind==='antmultica'){
    Object.assign(config,{workspace_id:$('workspace-id').value.trim(),workspace_slug:$('workspace-slug').value.trim(),assignee_id:$('assignee-id').value.trim(),iteration_key:$('iteration-key').value.trim(),iteration_value:$('iteration-value').value.trim(),role_id:$('source-role').value,defer_assignment:$('defer-assignment').value==='true'});
  }else{
    config.reviewer_role_ids=Array.from($('reviewer-roles').querySelectorAll('input:checked'),n=>n.value);
    config.ignore_logins=$('ignore-logins').value.split(/[,，]/).map(s=>s.trim()).filter(Boolean);
  }
  const source={...original,name:$('source-name').value.trim(),enabled:$('source-enabled').value==='true',interval_seconds:Number($('source-interval').value),config};
  await api('/sources/'+encodeURIComponent(source.source_id),'PUT',{source,expected_version:original.version});
  $('source-dialog').close();await refresh();notice('任务源配置已保存。停用不会删除记录；修改迭代不会删除旧任务。');
});};
$('register-pr').onclick=()=>{
  $('pr-form').reset();$('pr-error').hidden=true;
  options($('pr-task'),state.tasks.filter(t=>t.assigned_agent_id&&t.state!=='COMPLETED'),'task_id','选择已开始执行的原任务');
  const requested=new URLSearchParams(location.search).get('task');if(requested)$('pr-task').value=requested;
  options($('pr-source'),state.sources.filter(s=>s.kind==='github'),'source_id','选择任务源');
  if(state.sources.filter(s=>s.kind==='github').length===1)$('pr-source').value=state.sources.find(s=>s.kind==='github').source_id;
  $('pr-dialog').showModal();
};
$('close-pr').onclick=()=>$('pr-dialog').close();
$('pr-form').onsubmit=e=>{e.preventDefault();action(async()=>{
  await api('/work/tasks/'+encodeURIComponent($('pr-task').value)+'/pull-requests','POST',{url:$('pr-url').value.trim(),source_id:$('pr-source').value});
  $('pr-dialog').close();await refresh();notice('PR 已登记，后续事件将返回原任务的成员和 Session。');
});};
$('connect').onclick=()=>action(async()=>{WA.saveToken($('token').value);await refresh();});
$('refresh').onclick=()=>action(refresh);
window.addEventListener('beforeunload',e=>{if($('source-dialog').open||$('pr-dialog').open){e.preventDefault();e.returnValue='';}});
async function poll(){if(!state.busy&&!document.hidden&&!$('source-dialog').open&&!$('pr-dialog').open){try{await refresh();}catch(e){notice(e.message,true);}}setTimeout(poll,5000);}
action(refresh);setTimeout(poll,5000);
