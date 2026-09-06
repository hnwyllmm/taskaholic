'use strict';
(()=>{
  const {el,api,notice}=WA,box=document.getElementById('system-agent-cards');
  let dirty=false,loading=false,saving=false;
  async function refresh(force=false){
    if(loading||saving||dirty&&!force)return;
    loading=true;
    try{
      const [config,{agents},{runtimes}]=await Promise.all([api('/system/agents'),api('/agents'),api('/runtimes')]);
      if(dirty&&!force)return;
      dirty=false;box.replaceChildren();
      for(const slot of config.slots){
        const b=config.bindings.find(b=>b.slot===slot.slot),form=el('form',null,'system-agent-card'),heading=el('h3',slot.name);
        form.append(heading,el('p',slot.description,'small muted'));
        const label=el('label','执行成员'),select=el('select');select.setAttribute('aria-label',slot.name+'执行成员');
        const base=el('option',slot.default_mode==='rules'?'规则路由 · 按能力与负载选择':slot.slot==='upgrade_builder'?'自动 · 使用启动配置':'自动 · 选择在线空闲成员');base.value='';select.append(base);
        for(const a of agents){
          const rt=runtimes.find(r=>r.runtime_id===a.runtime_id),features=rt?.capabilities?.adapters?.[a.adapter_id];
          const compatible=slot.slot==='upgrade_builder'?config.upgrade_enabled&&a.runtime_id===config.local_runtime_id&&a.adapter_id==='codex-agent':features?.role_instructions&&features?.structured_output&&features?.read_only_runs;
          const o=el('option',a.name+' · '+(a.model_id||'默认模型')+(a.state!=='ACTIVE'?' · 已停用':!compatible?' · 不兼容':''));o.value=a.agent_id;o.disabled=a.state!=='ACTIVE'||!compatible;select.append(o);
        }
        select.value=b.agent_id||'';label.append(select);form.append(label);
        const detail=el('p',null,'system-executor-detail small'),status=el('p',null,'small'),button=el('button','保存岗位','primary');button.type='submit';button.disabled=true;
        function describe(){const a=agents.find(a=>a.agent_id===select.value),rt=runtimes.find(r=>r.runtime_id===a?.runtime_id);detail.textContent=a?`${a.runtime_id} · ${a.adapter_id} · ${a.model_id||'默认模型'}\n${a.state==='ACTIVE'?'已启用':'已停用'} · ${rt?.state==='ONLINE'?'机器在线':'机器离线'} · ${a.active_runs}/${a.max_concurrent} 运行中`:slot.slot==='task_router'?'不调用推理 Agent；仍保留角色、能力、排除成员和并发约束。':slot.slot==='upgrade_builder'?'使用本机升级守护进程的默认模型；仅准备隔离候选版本。':'新 Session 自动选择成员；已有 Session 不自动改派。';}
        describe();form.append(detail,button,status);
        select.onchange=()=>{form.dataset.dirty=String(select.value!==(b.agent_id||''));button.disabled=form.dataset.dirty!=='true';dirty=!!box.querySelector('[data-dirty="true"]');status.textContent='';describe();};
        form.onsubmit=async e=>{
          e.preventDefault();if(saving)return;saving=true;
          const selected=select.value;box.querySelectorAll('button,select').forEach(n=>n.disabled=true);
          try{const updated=await api('/system/agents/'+slot.slot,'PUT',{mode:selected?'agent':slot.default_mode,agent_id:selected,expected_version:b.version});Object.assign(b,updated);form.dataset.dirty='false';dirty=!!box.querySelector('[data-dirty="true"]');status.textContent='已保存 · v'+updated.version;status.className='small';notice(slot.name+'岗位已更新。已有 Session 和进行中的运行不变。');}
          catch(e){status.textContent=e.message;status.className='small error';}
          finally{saving=false;box.querySelectorAll('select').forEach(n=>n.disabled=false);box.querySelectorAll('form').forEach(f=>f.querySelector('button').disabled=f.dataset.dirty!=='true');}
        };
        box.append(form);
      }
    }catch(e){notice(e.message,true);}finally{loading=false;}
  }
  document.getElementById('refresh-system-agents').onclick=()=>{if(!dirty||confirm('放弃尚未保存的岗位选择，重新读取配置？'))refresh(true);};
  window.WASystemAgents={refresh};
  window.addEventListener('beforeunload',e=>{if(dirty){e.preventDefault();e.returnValue='';}});
})();
