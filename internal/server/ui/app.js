'use strict';
const $ = id => document.getElementById(id);
const state = {token: WA.token(), draft: null, role: null, dirty: false, busy: false, editingRole: false, runtimes: [], agents: [], roles: [], session: null, poll: null};
const loadModels=WAModels.catalogLoader(api);
const modelControls={builder:WAModels.mount($('builder-model'),loadModels),agent:WAModels.mount($('agent-model'),loadModels)};
function requestKey() { const bytes = new Uint8Array(16); crypto.getRandomValues(bytes); return Array.from(bytes, b=>b.toString(16).padStart(2,'0')).join(''); }
const labels = {DRAFT:'草案', GENERATING:'AI 生成中', FAILED:'生成失败', PUBLISHED:'已发布'};
function node(tag, text, className) { const el = document.createElement(tag); if (text != null) el.textContent = text; if (className) el.className = className; return el; }
function notice(message, error = false) { $('notice').textContent = message; $('notice').className = error ? 'error' : ''; $('notice').hidden = !message; }
async function api(path, method = 'GET', body) {
  const response = await fetch('/api/v1' + path, {method, headers:{'Content-Type':'application/json', ...(state.token ? {Authorization:'Bearer ' + state.token} : {})}, body:body === undefined ? undefined : JSON.stringify(body)});
  const value = await response.json();
  if (!response.ok) throw new Error(response.status === 401 ? '请填写控制端的 API Token 后连接。' : value.error || '请求失败');
  return value;
}
async function action(fn) {
  if (state.busy) return;
  state.busy = true; updateControls(); notice('');
  try { await fn(); } catch (error) { notice(error.message, true); }
  finally { state.busy = false; updateControls(); }
}
function permitSwitch() { return !state.dirty || window.confirm('当前修改尚未保存，确定离开吗？'); }
function updateControls() {
  const draft = state.draft, generating = draft?.state === 'GENERATING', published = !!state.role || draft?.state === 'PUBLISHED';
  $('spec-fields').disabled = state.busy || generating || (published && !state.editingRole);
  $('edit-role').hidden = !published || state.editingRole;
  $('save-role').hidden = $('cancel-role').hidden = !state.editingRole;
  for (const id of ['edit-role','save-role','cancel-role']) $(id).disabled = state.busy;
  for (const id of ['save','publish','send','message']) $(id).disabled = state.busy || !draft || generating || published;
  for (const id of ['builder-runtime','builder-model','builder-adapter']) $(id).disabled = state.busy || !!state.session || generating || published || !$('builder-manual')?.checked;
  modelControls.builder.setDisabled($('builder-model').disabled);
  modelControls.agent.setDisabled(state.busy);
  if($('builder-manual'))$('builder-manual').disabled=state.busy||!!state.session||generating||published;
  for (const id of ['connect','new','welcome-new','clone','stop']) $(id).disabled = state.busy;
  document.querySelectorAll('.nav-item,.chips button,#agent-form button').forEach(el => { el.disabled = state.busy || (el.matches('.chips button') && (generating || published)); });
  $('save').hidden = published; $('publish').hidden = published; $('clone').hidden = !published;
  $('generation').hidden = !generating;
  $('saved').textContent = state.dirty ? '有未保存修改' : (published ? '角色 v'+(state.role?.version || 1)+' · 修改发布新版本，已有 Session 保留旧版本' : '已保存');
}
function readSpec() { return {name:$('name').value.trim(), description:$('description').value.trim(), capabilities:$('capabilities').value.split(',').map(s=>s.trim()).filter(Boolean), instructions:$('instructions').value.trim(), output_contract:$('output-contract').value.trim(), boundaries:$('boundaries').value.split('\n').map(s=>s.trim()).filter(Boolean)}; }
function renderSpec(spec) {
  $('name').value = spec.name || ''; $('description').value = spec.description || ''; $('capabilities').value = (spec.capabilities || []).join(', ');
  $('instructions').value = spec.instructions || ''; $('output-contract').value = spec.output_contract || ''; $('boundaries').value = (spec.boundaries || []).join('\n');
}
function renderCurrent() {
  const draft = state.draft, role = state.role, spec = role || draft?.spec;
  if (!spec) return;
  $('welcome').hidden = true; $('studio').hidden = false;
  history.replaceState(null,'','#'+(state.role?.role_id||state.draft?.draft_id));
  $('heading').textContent = spec.name || '未命名角色'; $('state').textContent = role ? '已发布' : labels[draft.state];
  renderSpec(spec); $('messages').replaceChildren(); $('questions').replaceChildren();
  if (!draft?.messages.length) $('messages').append(node('p', role ? '角色已发布。创建具体 Agent 后，即可按角色或能力接收任务。' : '告诉 AI：这个角色负责什么、如何工作、哪些事情要先问你。也可以直接在右侧手动填写。', 'muted small'));
  for (const message of draft?.messages || []) {
    const bubble = node('div', null, 'bubble ' + message.speaker);
    bubble.append(node('span', message.speaker === 'user' ? '你' : '角色设计助手', 'speaker'), node('div', message.content)); $('messages').append(bubble);
  }
  for (const question of draft?.questions || []) $('questions').append(node('p', '↳ ' + question, 'question'));
  $('builder-error').hidden = !draft?.error; $('builder-error').textContent = draft?.error || '';
  $('agent-section').hidden = !(role || draft?.published_role_id);
  $('messages').scrollTop = $('messages').scrollHeight;
  renderAgents(); updateControls();
  window.WASystemAgents?.refresh();
}
function renderAgents() {
  const roleID = state.role?.role_id || state.draft?.published_role_id;
  const agents = state.agents.filter(a=>a.role_id === roleID); $('agents').replaceChildren();
  if (!agents.length) $('agents').append(node('p', '还没有配置 Agent。需要先启动一台支持角色指令的 Runtime。', 'muted small'));
  for (const agent of agents) {
    const row = node('div', null, 'agent-row');
    const online = state.runtimes.find(r=>r.runtime_id === agent.runtime_id)?.state === 'ONLINE';
    row.append(node('strong', agent.name), node('span', agent.runtime_id + (online ? ' · 在线' : ' · 离线')), node('span', agent.adapter_id==='codex-agent'?'Codex CLI':agent.adapter_id==='cursor-agent'?'Cursor Agent':agent.adapter_id), node('span', agent.model_id || '默认模型'), node('span', `运行中 ${agent.active_runs} / ${agent.max_concurrent}`), node('code', agent.agent_id));
    const settings = node('details'), summary = node('summary', '修改配置 · '+agent.state); settings.append(summary);
    const form = node('form'); const fields = {};
    for (const [key,label,value,type] of [['name','名称',agent.name,'text'],['model_id','模型（只影响新 Session）',agent.model_id||'','text'],['max_concurrent','并发上限',agent.max_concurrent,'number']]) {
      const wrap = node('label',label), input=node('input'); input.type=type; input.value=value;
      if(type==='number'){input.min=1;input.max=32;} if(key==='name')input.required=true;
      fields[key]=input;wrap.append(input);form.append(wrap);
    }
    fields.model_id.id='model-'+agent.agent_id;
    const modelControl=WAModels.mount(fields.model_id,loadModels);
    void modelControl.update({runtimeID:agent.runtime_id,adapterID:agent.adapter_id,runtimes:state.runtimes,agents:state.agents});
    const statusLabel=node('label','状态'),status=node('select');status.append(option('ACTIVE','启用'),option('DISABLED','停用（不打断当前运行）'));status.value=agent.state;statusLabel.append(status);form.append(statusLabel);
    const submit=node('button','保存 Agent');submit.type='submit';form.append(submit);
    form.onsubmit=event=>{event.preventDefault();action(async()=>{await api('/agents/'+agent.agent_id,'PUT',{name:fields.name.value,model_id:fields.model_id.value,max_concurrent:Number(fields.max_concurrent.value),state:status.value,expected_version:agent.version||0});await refreshLibrary();notice('Agent 已更新；已有 Session 继续使用原角色和模型。');});};settings.append(form);row.append(settings);
    $('agents').append(row);
  }
}
function option(value, label) { const el = node('option', label); el.value = value; return el; }
function suggestAdapter(prefix) {
  const field=$(prefix+'-adapter');
  const choices=WAAdapters.options(state.runtimes,$(prefix+'-runtime').value,field.value,field.dataset.edited==='true',prefix==='builder'&&!!state.session);
  field.replaceChildren(...choices.options.map(item=>{const itemOption=option(item.value,item.label);itemOption.disabled=item.disabled;return itemOption;}));
  field.value=choices.value;
  updateModels(prefix);
}
function updateModels(prefix){void modelControls[prefix].update({runtimeID:$(prefix+'-runtime').value,adapterID:$(prefix+'-adapter').value,runtimes:state.runtimes,agents:state.agents});}
for(const prefix of ['builder','agent']) {
  $(prefix+'-adapter').addEventListener('change',()=>{$(prefix+'-adapter').dataset.edited='true';modelControls[prefix].setValue('');updateModels(prefix);});
  $(prefix+'-runtime').addEventListener('change',()=>suggestAdapter(prefix));
}
async function refreshLibrary() {
  const [draftData, roleData, runtimeData, agentData] = await Promise.all([api('/role-drafts'), api('/roles'), api('/runtimes'), api('/agents')]);
  state.runtimes = runtimeData.runtimes; state.agents = agentData.agents; state.roles = roleData.roles;
  renderRoleOverview();
  const drafts = draftData.drafts.filter(d=>d.state !== 'PUBLISHED');
  $('draft-count').textContent = drafts.length; $('role-count').textContent = roleData.roles.length;
  $('draft-list').replaceChildren(); $('role-list').replaceChildren();
  for (const draft of drafts) {
    const button = node('button', null, 'nav-item' + (state.draft?.draft_id === draft.draft_id ? ' active' : ''));
    button.append(node('strong', draft.spec.name || '未命名角色'), node('small', labels[draft.state]));
    button.onclick = () => { if (permitSwitch()) action(()=>loadDraft(draft.draft_id)); }; $('draft-list').append(button);
  }
  for (const role of roleData.roles) {
    const button = node('button', null, 'nav-item' + (state.role?.role_id === role.role_id || state.draft?.published_role_id === role.role_id ? ' active' : ''));
    button.append(node('strong', role.name), node('small', role.capabilities.join(' · ')));
    button.onclick = () => { if (permitSwitch()) action(async()=>{ clearTimeout(state.poll); state.editingRole=false; state.draft=null; state.role=role; state.dirty=false; renderCurrent(); await refreshLibrary(); }); }; $('role-list').append(button);
  }
  for (const id of ['builder-runtime','agent-runtime']) {
    const select = $(id), current = select.value; select.replaceChildren(option('', id === 'builder-runtime' ? '自动选择在线机器' : '选择执行机器'));
    for (const runtime of state.runtimes) select.append(option(runtime.runtime_id, `${runtime.runtime_id} · ${runtime.state === 'ONLINE' ? '在线' : '离线'}`));
    if ([...select.options].some(o=>o.value === current)) select.value = current;
  }
  suggestAdapter('builder'); suggestAdapter('agent');
  $('connection').textContent = `${state.runtimes.filter(r=>r.state === 'ONLINE').length} 台在线机器`;
  renderAgents(); updateControls();
  window.WASystemAgents?.refresh();
}
async function loadDraft(id) {
  clearTimeout(state.poll);
  const draft = await api('/role-drafts/' + id), detail = await api('/tasks/' + draft.task_id);
  state.draft = draft; state.role = draft.published_role_id ? await api('/roles/'+draft.published_role_id) : null; state.editingRole=false; state.session = detail.session || null; state.dirty = false;
  $('message').value = '';
  if (state.session) { $('builder-runtime').value=state.session.runtime_id; $('builder-model').value=state.session.model_id || ''; $('builder-adapter').replaceChildren(option(state.session.adapter_id,state.session.adapter_id)); $('builder-adapter').value=state.session.adapter_id; }
  else { $('builder-runtime').value=''; $('builder-model').value=''; $('builder-adapter').value=''; $('builder-adapter').dataset.edited='false'; }
  $('session-hint').textContent = state.session ? '已绑定原 Agent / Session；继续对话沿用相同运行环境。' : '首次生成后绑定 Session，后续对话复用它。';
  renderCurrent(); await refreshLibrary(); schedulePoll();
}
function schedulePoll() {
  clearTimeout(state.poll);
  if (state.draft?.state !== 'GENERATING') return;
  const id = state.draft.draft_id;
  state.poll = setTimeout(async()=>{
    if (state.busy) { schedulePoll(); return; }
    try {
      const draft = await api('/role-drafts/' + id);
      if (state.draft?.draft_id !== id) return;
      state.draft = draft; renderCurrent();
      if (draft.state !== 'GENERATING') { await refreshLibrary(); notice(draft.state === 'FAILED' ? '生成未完成，原草案已保留。可以修改后重试。' : '草案已更新。请检查配置，也可以继续给 AI 修改意见。', draft.state === 'FAILED'); }
      schedulePoll();
    } catch (error) { notice(error.message + ' 正在重试读取生成状态。', true); schedulePoll(); }
  }, 1500);
}
async function saveDraft() {
  if (!state.dirty) return;
  state.draft = await api('/role-drafts/' + state.draft.draft_id, 'PUT', {expected_version:state.draft.version, spec:readSpec()});
  state.dirty = false; renderCurrent();
}
function newDraft() { if (!permitSwitch()) return; action(async()=>{ const draft = await api('/role-drafts', 'POST', {idempotency_key:requestKey()}); await loadDraft(draft.draft_id); $('message').focus(); }); }
$('new').onclick = newDraft; $('welcome-new').onclick = newDraft;
$('connect').onclick = () => action(async()=>{ WA.saveToken($('token').value);state.token=WA.token(); await refreshLibrary(); notice('已连接。Token 仅保留在此标签页会话中。'); });
$('spec-fields').addEventListener('input', ()=>{ state.dirty=true; updateControls(); });
$('save').onclick = () => action(async()=>{ await saveDraft(); await refreshLibrary(); notice('草案已保存。'); });
$('send').onclick = () => action(async()=>{
  const message = $('message').value.trim(); if (!message) throw new Error('先写下角色需求或修改意见。');
  await saveDraft();
  const request = {expected_version:state.draft.version, message, idempotency_key:requestKey()};
  if (!state.session && $('builder-manual')?.checked) Object.assign(request, {manual_executor:true,runtime_id:$('builder-runtime').value, adapter_id:$('builder-adapter').value, model_id:$('builder-model').value});
  const result = await api('/role-drafts/' + state.draft.draft_id + '/messages', 'POST', request);
  state.draft=result.draft; state.session={runtime_id:result.run.runtime_id,model_id:result.run.model_id,adapter_id:result.run.adapter_id};
  $('builder-runtime').value=result.run.runtime_id; $('builder-model').value=result.run.model_id || ''; $('builder-adapter').value=result.run.adapter_id;
  $('session-hint').textContent='已绑定原 Agent / Session；继续对话沿用相同运行环境。';
  $('message').value=''; renderCurrent(); await refreshLibrary(); schedulePoll();
});
$('publish').onclick = () => action(async()=>{
  await saveDraft();
  const role = await api('/role-drafts/' + state.draft.draft_id + '/publish', 'POST', {expected_version:state.draft.version});
  state.draft = await api('/role-drafts/' + state.draft.draft_id); state.role=role; renderCurrent(); await refreshLibrary(); notice('角色已发布。现在可以为它配置一个或多个具体 Agent。');
});
$('clone').onclick = () => action(async()=>{
  if (!permitSwitch()) return;
  const draft = await api('/role-drafts','POST',{source_role_id:state.role?.role_id || state.draft.published_role_id,idempotency_key:requestKey()});
  await loadDraft(draft.draft_id); notice('已复制为新草案；原角色和已有 Session 不受影响。');
});
$('stop').onclick = () => action(async()=>{ await api('/runs/' + state.draft.last_run_id + '/interrupt','POST',{}); notice('已发送停止请求，正在等待 Runtime 确认。'); });
document.querySelectorAll('[data-prompt]').forEach(button=>button.onclick=()=>{ $('message').value=button.dataset.prompt; $('message').focus(); });
$('agent-form').onsubmit = event => { event.preventDefault(); action(async()=>{
  await api('/agents','POST',{name:$('agent-name').value.trim(),role_id:state.role?.role_id || state.draft.published_role_id,runtime_id:$('agent-runtime').value,adapter_id:$('agent-adapter').value,model_id:$('agent-model').value,max_concurrent:Number($('agent-capacity').value)});
  $('agent-name').value=''; await refreshLibrary(); notice('成员已添加。任务可通过角色或能力标签路由给它。');
}); };
window.addEventListener('beforeunload', event=>{ if (state.dirty||$('message').value) { event.preventDefault(); event.returnValue=''; } });
$('edit-role').onclick = () => {state.editingRole=true;updateControls();$('name').focus();};
$('cancel-role').onclick = () => {state.editingRole=false;state.dirty=false;renderCurrent();};
$('save-role').onclick = () => action(async()=>{state.role=await api('/roles/'+state.role.role_id,'PUT',{expected_version:state.role.version,spec:readSpec()});state.editingRole=false;state.dirty=false;renderCurrent();await refreshLibrary();notice('已发布角色 v'+state.role.version+'。新任务使用新版本，已有 Session 不变。');});
function renderRoleOverview(){const q=$('role-search').value.trim().toLowerCase(),roles=state.roles.filter(r=>(r.name+' '+r.description+' '+r.capabilities.join(' ')).toLowerCase().includes(q));$('role-overview').replaceChildren();for(const role of roles){const c=node('article',null,'card');c.append(node('span','已发布 · v'+role.version,'eyebrow'),node('h2',role.name),node('p',role.description||'暂无展示简介'));const chips=node('div',null,'chips-row');for(const cap of role.capabilities)chips.append(node('span',cap,'cap-chip'));c.append(chips,node('p',state.agents.filter(a=>a.role_id===role.role_id).length+' 位 Agent 成员','small muted'));const b=node('button','浏览与修改 →');b.onclick=()=>{if(permitSwitch())action(()=>openRole(role.role_id));};c.append(b);$('role-overview').append(c);}if(!roles.length)$('role-overview').append(node('p',q?'没有符合条件的角色。':'还没有发布角色。可以使用 AI 创建草稿，也可以手动填写。','empty-state'));}
async function openRole(id){clearTimeout(state.poll);state.editingRole=false;state.draft=null;state.role=await api('/roles/'+id);state.dirty=false;$('message').value='';renderCurrent();await refreshLibrary();}
$('role-search').oninput=renderRoleOverview;
$('back-to-roles').onclick=()=>{if(!permitSwitch())return;clearTimeout(state.poll);state.draft=null;state.role=null;state.dirty=false;state.editingRole=false;$('message').value='';$('studio').hidden=true;$('welcome').hidden=false;history.replaceState(null,'','/members');action(refreshLibrary);};
window.addEventListener('hashchange',()=>{const id=location.hash.slice(1);if(!id||id===(state.role?.role_id||state.draft?.draft_id)||!permitSwitch())return;action(()=>id.startsWith('draft_')?loadDraft(id):openRole(id));});
const builderManual=node('input');builderManual.type='checkbox';builderManual.id='builder-manual';const builderMode=node('label','本次草案手动指定运行环境（覆盖系统岗位）');builderMode.prepend(builderManual);$('builder-runtime').closest('details').querySelector('summary').after(builderMode);const builderNote=node('p','默认使用“系统岗位 → 角色设计”的成员；切换为手动时仅影响当前新草案。','muted small');builderMode.after(builderNote);
builderManual.onchange=updateControls;
action(async()=>{await refreshLibrary();const id=location.hash.slice(1);if(id&&id!=='system-agents')await(id.startsWith('draft_')?loadDraft(id):openRole(id));});
