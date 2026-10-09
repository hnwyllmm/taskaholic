'use strict';
window.WAPermissions=(()=>{
 const {el,api,link,notice}=WA;
 const names={'development.execute':'隔离开发 / PR 发布',windows_seekdb_phase0:'Windows Phase 0 验证','agent.network_access':'Agent 网络访问','agent.host_full_access':'Agent 宿主机完整访问'};
 const states={PENDING:'等待授权',APPROVED:'已授权，等待调度',USED:'已放行执行',DENIED:'已拒绝',STALE:'已失效'};
 const effects={allow:'预授权允许',ask:'每次确认',deny:'禁止'};
 let busy=false;
	if(document.body.dataset.page==='home'){
	 const section=el('section');section.append(el('h2','等待执行授权'),link('管理预授权规则 →','/permissions'));const box=el('div');box.id='permission-requests';section.append(box);document.querySelector('.home-inbox')?.prepend(section);
	}
 async function act(fn){if(busy)return;busy=true;try{await fn();}catch(e){notice(e.message,true);}finally{busy=false;}}
 function renderRequests(box,requests,refresh,{compact=false,emptyText='当前没有待授权事项。'}={}){
  box.replaceChildren();const shown=(requests||[]).filter(r=>!compact||r.state==='PENDING');
  if(!shown.length){if(emptyText)box.append(el('p',emptyText,'muted small'));return;}
  for(const r of shown){const card=el('section',null,'inbox-card');card.append(el('strong',r.title),el('p',`${names[r.operation]||r.operation} · ${r.runtime_id} · ${r.repository}`),el('p',states[r.state]||r.state,'small'),el('p',r.reason,'small muted'),link('查看任务 →','/tasks#'+r.task_id));
   if(r.parent_task_id)card.append(link('原始任务 →','/tasks#'+r.parent_task_id));
   if(r.state==='PENDING'){
    for(const [decision,remember,text] of [['approve',false,'仅允许本次'],['approve',true,'允许并记住此范围'],['deny',false,'拒绝']]){
     const button=el('button',text);button.onclick=()=>act(async()=>{const highRisk=r.operation==='agent.host_full_access'?'\n高风险：这会移除该轮 Codex 的宿主机文件系统沙箱，请确认申请理由和范围。':'';if(decision==='approve'&&!confirm(`${text}：${names[r.operation]}\n机器：${r.runtime_id}\n仓库：${r.repository}\n${remember?'后续相同机器、仓库和操作无需再次确认。':'仅对应本次执行，不扩展权限。'}\n不会替代方案审批。${highRisk}`))return;button.disabled=true;try{await api('/execution-permissions/requests/'+r.request_id,'POST',{expected_version:r.version,decision,remember});await refresh();}finally{button.disabled=false;}});card.append(button);
    }
   }
   if(!compact&&['PENDING','DENIED'].includes(r.state)){const b=el('button','按最新规则重新检查');b.onclick=()=>act(async()=>{await api('/execution-permissions/requests/'+r.request_id,'POST',{expected_version:r.version,decision:'recheck'});await refresh();});card.append(b);}
   if(!compact&&!['PENDING','DENIED'].includes(r.state)){const history=el('details');history.append(el('summary',(states[r.state]||r.state)+' · '+r.title),card);box.append(history);}else box.append(card);
  }
 }
 function manualCapabilityActions(box,taskID,scope,refresh){
  const card=el('section',null,'inbox-card');card.append(el('strong','主动授权运行能力'),el('p',`原 Agent 未提交结构化权限申请。只有在你已确认是运行能力受限时，才为 ${scope.runtime_id} / ${scope.repository} 授权；系统不会从报错文字自行猜测或扩大权限。`,'small muted'));
  const add=(capability,remember,text)=>{const button=el('button',text);button.onclick=()=>act(async()=>{const label=names['agent.'+capability]||capability,highRisk=capability==='host_full_access'?'\n高风险：这会移除下一轮 Codex 的宿主机文件系统沙箱。请仅在确实需要操作宿主机时授权。':'';if(!confirm(`${text}：${label}\n机器：${scope.runtime_id}\n仓库：${scope.repository}\n${remember?'后续相同机器、仓库和能力会自动通过。':'仅对应当前受阻任务的下一轮继续执行。'}\n授权不改变任务目标、已批准方案或其它系统权限。${highRisk}`))return;button.disabled=true;try{await api('/work/tasks/'+taskID+'/execution-permissions','POST',{expected_version:scope.expected_version,capability,remember});await refresh();}finally{button.disabled=false;}});card.append(button);};
  add('network_access',false,'允许 Agent 联网（仅本次）');add('network_access',true,'允许并记住联网');add('host_full_access',false,'允许完整主机访问（仅本次）');add('host_full_access',true,'允许并记住完整主机访问');box.append(card);
 }
 function mountTask(panel,box,{changed}={}){
  let taskID='',taskState='',scope={},disposed=false;
  async function refresh(){
   if(!taskID||disposed)return;
   const data=await api('/execution-permissions'),items=(data.requests||[]).filter(r=>r.task_id===taskID||r.parent_task_id===taskID);
   const pending=items.some(r=>r.state==='PENDING'),manual=!!scope.available&&taskState==='BLOCKED'&&!pending;
   panel.hidden=!pending&&!manual;
   const heading=document.getElementById('task-permission-heading'),intro=document.getElementById('task-permission-intro');
   if(heading)heading.textContent=pending?'这个任务正在等待执行授权':'为受阻任务补充执行授权';
   if(intro)intro.textContent=pending?'“仅允许本次”只放行当前申请；“允许并记住此范围”会让相同操作、机器和仓库以后自动通过。不会扩大其它权限。':'不确定是否需要权限时，先使用“重新评估 / 继续”。主动授权只允许已登记的运行能力，并始终绑定当前批准的方案、原 Agent 和原 Session。';
   renderRequests(box,items,async()=>{await refresh();if(changed)await changed();},{emptyText:manual?'':''});
   if(manual)manualCapabilityActions(box,taskID,scope,async()=>{await refresh();if(changed)await changed();});
  }
  async function update(id,state,nextScope={}){taskID=id;taskState=state;scope=nextScope||{};await refresh();}
  const timer=setInterval(()=>{if(!disposed&&!busy&&!document.hidden&&!panel.hidden)void refresh().catch(e=>notice(e.message,true));},5000);
  return{update,dispose(){disposed=true;clearInterval(timer);}};
 }
 let rules=[];
 async function refresh(){
  const data=await api('/execution-permissions');
	if(document.body.dataset.page==='permissions'&&document.getElementById('connection'))document.getElementById('connection').textContent='已连接';
  const box=document.getElementById('permission-requests');if(!box)return;
  if(document.body.dataset.page!=='permissions'){renderRequests(box,data.requests,refresh,{compact:true});return;}
  rules=data.policies||[];renderRequests(box,data.requests,refresh);
  const p=document.getElementById('permission-policies');p.replaceChildren();
  for(const r of rules){const row=el('p');row.append(el('span',`${names[r.operation]} · ${r.runtime_id} · ${r.repository} · ${effects[r.effect]} `));const b=el('button','修改');b.onclick=()=>{for(const k of ['operation','runtime','repository','effect'])document.getElementById('policy-'+k).value=r[k==='runtime'?'runtime_id':k];};row.append(b);p.append(row);}
  if(!rules.length)p.append(el('p','尚无自定义规则，使用上述默认策略。','small muted'));
  const list=document.getElementById('permission-runtimes'),caps=document.getElementById('permission-capabilities');list.replaceChildren();caps.replaceChildren();
  for(const runtime of data.runtimes||[]){const o=el('option');o.value=runtime.runtime_id;list.append(o);caps.append(el('h3',runtime.runtime_id+' · '+runtime.state));let c=runtime.capabilities||{};if(typeof c==='string'){try{c=JSON.parse(c);}catch{c={};}}const executors=c.executors||{};if(!Object.keys(executors).length)caps.append(el('p','未上报受控执行器能力','small'));for(const [name,v] of Object.entries(executors))caps.append(el('p',`${names[name]||name}：${v.available?'已配置':v.reason||'不可用'}`,'small'));}
 }
 const form=document.getElementById('policy-form');if(form)form.onsubmit=e=>{e.preventDefault();act(async()=>{const p={operation:document.getElementById('policy-operation').value,runtime_id:document.getElementById('policy-runtime').value.trim(),repository:document.getElementById('policy-repository').value.trim(),effect:document.getElementById('policy-effect').value};p.version=rules.find(x=>x.operation===p.operation&&x.runtime_id===p.runtime_id&&x.repository===p.repository)?.version||0;if(!confirm(`保存策略：${effects[p.effect]}\n${names[p.operation]} · ${p.runtime_id} · ${p.repository}\n只影响后续调度，不会中断已执行操作。`))return;await api('/execution-permissions/policies','PUT',p);await refresh();notice('规则已保存。待授权任务可按最新规则重新检查。');});};
 if(document.getElementById('permission-requests')){void refresh().catch(e=>notice(e.message,true));setInterval(()=>{if(!busy&&!document.hidden)void refresh().catch(e=>notice(e.message,true));},5000);}
 return {renderRequests,mountTask};
})();
