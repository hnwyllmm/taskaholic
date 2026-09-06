'use strict';
(()=>{
  function canAssign(data){const t=data?.detail?.task;return !!t&&!data.detail.session&&!data.detail.runs?.length&&!t.assigned_agent_id&&['NEW','QUEUED','PAUSED','BLOCKED'].includes(t.state);}
  function matchesFilter(task,filter){return !filter||task.state===filter||(filter==='QUEUED'&&task.state==='NEW');}
  function candidates(requirements={},agents=[],runtimes=[]){
    return agents.map(a=>{
      const rt=runtimes.find(r=>r.runtime_id===a.runtime_id),caps=rt?.capabilities?.adapters?.[a.adapter_id];
      let reason='';
      if(a.state!=='ACTIVE')reason='已停用';
      else if(requirements.role_id&&a.role_id!==requirements.role_id)reason='角色不匹配';
      else if(requirements.excluded_agent_ids?.includes(a.agent_id))reason='任务已排除此成员';
      else if((requirements.capabilities||[]).some(c=>!a.role?.capabilities?.includes(c)))reason='能力不匹配';
      else if(rt&&(!caps?.role_instructions||!caps?.structured_output||!caps?.read_only_runs))reason='运行环境不支持此类任务';
      const availability=!rt||rt.state!=='ONLINE'?'离线 · 等待上线':a.active_runs>=a.max_concurrent?'忙碌 · 等待空闲':'在线 · 可接任务';
      return{value:a.agent_id,disabled:!!reason,label:`${a.name} · ${a.role?.name||'成员'} · ${a.runtime_id} · ${a.model_id||'默认模型'} · ${reason||availability}`};
    });
  }
  function mount(container,{submit}){
    const make=(tag,text,cls)=>{const n=document.createElement(tag);if(text)n.textContent=text;if(cls)n.className=cls;return n;};
    container.className='panel assignment-panel';
    const heading=make('h2','执行安排'),status=make('p',null,'small muted'),form=make('form'),label=make('label','执行成员'),select=make('select');
    select.id='assignment-agent';label.append(select);
    const button=make('button',null,'primary'),reset=make('button','使用当前分派');button.type='submit';reset.type='button';
    const actions=make('div',null,'row'),hint=make('p',null,'small muted');hint.id='assignment-hint';hint.setAttribute('role','status');select.setAttribute('aria-describedby',hint.id);
    actions.append(button,reset);form.append(label,actions);container.append(heading,status,form,hint);
    let data=null,agents=[],runtimes=[],taskID='',choice='',version=0,dirty=false,busy=false,choices=[];
    const currentVersion=()=>data?.work?.config?.task_version||data?.detail?.task?.version||0;
    function controls(disabled=busy){
      busy=disabled;const changeable=canAssign(data),stale=dirty&&version!==currentVersion();
      form.hidden=!changeable;select.disabled=busy;
      button.disabled=busy||!changeable||stale||(!!choice&&!choices.some(c=>c.value===choice&&!c.disabled));
      reset.disabled=busy;reset.hidden=!dirty;
      button.textContent=choice?'交给该成员执行':'自动选择成员并执行';
      if(!changeable)hint.textContent=data?.detail.task.state==='COMPLETED'?'工作已完成，执行与分派记录保留。':'任务已绑定原成员和 Session。后续沟通、继续执行和验收沿用原上下文，不能在这里直接换人。';
      else if(stale)hint.textContent='任务状态或分派已有新变化，请先使用当前分派，再重新确认。';
      else hint.textContent=choice?'指定成员忙碌或离线时留在队列等待，不自动换人。':'自动分派使用当前路由配置，按角色、能力、在线状态和容量选择；没有合适成员时等待。';
    }
    function update(next,knownAgents=agents,knownRuntimes=runtimes){
      data=next;agents=knownAgents;runtimes=knownRuntimes;
      if(!data){container.hidden=true;taskID='';dirty=false;return;}
      container.hidden=false;
      if(taskID!==data.detail.task.task_id){taskID=data.detail.task.task_id;dirty=false;}
      if(!dirty){choice=data.work.config.preferred_agent_id||'';version=currentVersion();}
      const original=data.detail.session?.agent_id||data.detail.task.assigned_agent_id;
      const owner=agents.find(a=>a.agent_id===(original||data.work.config.preferred_agent_id));
      status.textContent=original?'当前执行者：'+(owner?.name||original):data.detail.task.state==='NEW'?'任务已保存，尚未开始；请选择成员或交给系统自动分派。':data.work.config.preferred_agent_id?'已指定：'+(owner?.name||data.work.config.preferred_agent_id)+' · 等待执行':'已请求自动分派 · 等待合适成员';
      choices=candidates(data.detail.task.requirements,agents,runtimes);
      select.replaceChildren();
      for(const item of [{value:'',label:'自动分派 · 由系统选择合适成员'},...choices]){const option=make('option',item.label);option.value=item.value;option.disabled=!!item.disabled;select.append(option);}
      if(choice&&!choices.some(c=>c.value===choice)){const option=make('option',choice+' · 成员不可用');option.value=choice;option.disabled=true;select.append(option);}
      select.value=choice;controls();
    }
    select.onchange=()=>{choice=select.value;dirty=true;controls();};
    reset.onclick=()=>{dirty=false;update(data);};
    form.onsubmit=e=>{e.preventDefault();if(!button.disabled)submit(choice,version);};
    return{update,controls,reset(){dirty=false;if(data)update(data);}};
  }
  const exported={canAssign,matchesFilter,candidates,mount};
  if(typeof module!=='undefined'&&module.exports)module.exports=exported;else window.WAAssignment=exported;
})();
