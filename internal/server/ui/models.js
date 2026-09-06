'use strict';
(()=>{
  // Scope every catalog to the machine AND adapter. Share concurrent reads, but
  // never share a user-entered model value between controls or machines.
  function catalogLoader(api){
    const cache=new Map();
    return (runtime,adapter)=>{
      const key=JSON.stringify([runtime,adapter]),old=cache.get(key);
      if(old&&Date.now()<old.expires)return old.promise;
      const entry={expires:Date.now()+30000};
      entry.promise=api('/runtimes/'+encodeURIComponent(runtime)+'/models?adapter_id='+encodeURIComponent(adapter))
        .then(result=>{entry.expires=Date.now()+(result.status==='ready'?300000:30000);return result;})
        .catch(()=>({status:'unavailable',models:[]}));
      cache.set(key,entry);return entry.promise;
    };
  }
  function options(models,known,current){
    const result=[{id:'',name:'使用运行环境默认模型'}],seen=new Set(['']);
    for(const m of models||[]){
      if(typeof m?.id!=='string'||!m.id||m.id.length>200||seen.has(m.id))continue;
      seen.add(m.id);result.push({id:m.id,name:(m.name||m.id)+(m.name&&m.name!==m.id?' · '+m.id:'')});
    }
    for(const id of known||[]){if(id&&!seen.has(id)){seen.add(id);result.push({id,name:id+' · 已有成员配置'});}}
    if(current&&!seen.has(current))result.push({id:current,name:current+' · 当前填写（未在列表中）'});
    return result;
  }
  function mount(input,load){
    const make=(tag,text)=>{const n=document.createElement(tag);if(text)n.textContent=text;return n;};
    const label=input.parentElement,box=make('div'),title=make('span',label.firstChild?.textContent||'模型');
    box.className='model-field';title.id=input.id+'-label';
    const controls=make('div'),mode=make('select'),select=make('select'),hint=make('small');
    controls.className='model-controls';hint.className='muted model-hint';hint.id=input.id+'-hint';hint.setAttribute('aria-live','polite');
    mode.id=input.id+'-mode';select.id=input.id+'-select';mode.setAttribute('aria-label',title.textContent+'填写方式');
    for(const [value,text] of [['list','下拉选择'],['manual','手动填写']]){const option=make('option',text);option.value=value;mode.append(option);}
    for(const field of [input,select]){field.setAttribute('aria-labelledby',title.id);field.setAttribute('aria-describedby',hint.id);}
    input.maxLength=200;input.placeholder='输入模型 ID，留空使用默认模型';
    label.replaceWith(box);controls.append(mode,select,input);box.append(title,controls,hint);
    let models=[],known=[],sequence=0,contextKey='',pending=Promise.resolve(),disabled=input.disabled,status='idle';
    function render(){
      select.replaceChildren();
      for(const m of options(models,known,input.value)){const option=make('option',m.name);option.value=m.id;select.append(option);}
      select.value=input.value;const manual=mode.value==='manual';input.hidden=!manual;select.hidden=manual;
      mode.disabled=input.disabled=disabled;select.disabled=disabled||manual;
      const messages={idle:'选择执行机器和 Agent 类型后读取模型；也可手动填写。',loading:'正在读取这台机器的模型列表；仍可手动填写。',ready:'来自所选机器的 Agent；模型权限和额度以实际执行为准。',unsupported:'此 Agent 暂不提供模型列表，可选已有配置或手动填写。',unavailable:'模型列表暂不可用，可用默认模型、已有配置或手动填写。',offline:'执行机器离线，可选已有配置或手动填写。'};
      hint.textContent=messages[status]||messages.unavailable;
    }
    mode.onchange=render;
    select.onchange=()=>{input.value=select.value;};
    function update({runtimeID,adapterID,runtimes=[],agents=[]}){
      known=agents.filter(a=>a.runtime_id===runtimeID&&a.adapter_id===adapterID).map(a=>a.model_id);
      const runtime=runtimes.find(r=>r.runtime_id===runtimeID),caps=runtime?.capabilities?.adapters?.[adapterID];
      const key=JSON.stringify([runtimeID,adapterID,runtime?.epoch,runtime?.state,!!caps?.model_catalog]);
      if(key===contextKey){render();return pending;}
      contextKey=key;const ticket=++sequence;models=[];
      status=!runtimeID||!adapterID?'idle':!runtime||runtime.state!=='ONLINE'?'offline':!caps?.model_catalog?'unsupported':'loading';
      render();
      if(status!=='loading')return pending=Promise.resolve();
      pending=load(runtimeID,adapterID).then(catalog=>{
        if(ticket!==sequence)return;
        models=catalog.models||[];status=catalog.status;render();
      }).catch(()=>{if(ticket===sequence){status='unavailable';render();}});
      return pending;
    }
    render();
    return {update,setDisabled(value){disabled=!!value;render();},setValue(value){input.value=value||'';render();}};
  }
  const exports={catalogLoader,options,mount};
  if(typeof module!=='undefined'&&module.exports)module.exports=exports;
  else window.WAModels=exports;
})();
