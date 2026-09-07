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
  function mount(input,load,settings={}){
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
    const effort=make('select'),effortHint=make('small'),effortLabel=make('label','推理强度');
    effort.id=input.id+'-effort';effortHint.id=effort.id+'-hint';effortHint.className='muted model-hint';effortHint.setAttribute('aria-live','polite');effort.setAttribute('aria-describedby',effortHint.id);
    if(settings.reasoning){effortLabel.append(effort);box.append(effortLabel,effortHint);}
    let effortValue=settings.reasoningEffort||'',effortModel=input.value,effortContext='',effortSupported=false;
    let models=[],known=[],sequence=0,contextKey='',pending=Promise.resolve(),disabled=input.disabled,status='idle';
    function renderEffort(){
      if(!settings.reasoning)return;
      if(input.value!==effortModel){effortModel=input.value;effortValue='';}
      const model=models.find(m=>m.id===input.value),choices=effortSupported?(model?.reasoning_efforts||[]):[];
      const available=choices.some(c=>c.id===effortValue),items=[{id:'',name:'使用运行环境默认（不覆盖）'}];
      for(const choice of choices)items.push({id:choice.id,name:choice.id});
      if(effortValue&&!available)items.push({id:effortValue,name:effortValue+' · 原配置（待验证）'});
      effort.replaceChildren();for(const item of items){const option=make('option',item.name);option.value=item.id;effort.append(option);}
      effort.value=effortValue;effort.disabled=disabled||(!choices.length&&!effortValue);
      // An offline page may still save a name/concurrency edit with an unchanged
      // setting. Explicit new effort selections are checked again by the API.
      effort.setCustomValidity?.(status==='ready'&&effortValue&&!available?'所选模型不支持原推理强度，请选择运行环境默认或一个可用档位。':'');
      effortHint.textContent=status==='loading'?'正在读取可用推理档位。':!input.value?'先选择明确的模型；默认项不改变 Agent 原配置。':!effortSupported?'此 Agent 未提供独立推理强度；继续沿用原配置。':status!=='ready'?'档位暂不可用，保留原配置；也可恢复运行环境默认。':choices.length?'仅显示此模型支持的档位。修改后从下一次执行生效，保留原 Session。'+(model.default_reasoning_effort?' 模型报告的默认档位：'+model.default_reasoning_effort+'（运行环境可能覆盖）。':''):'此模型未提供可验证的独立档位；默认项不额外覆盖。';
    }
    function render(){
      select.replaceChildren();
      for(const m of options(models,known,input.value)){const option=make('option',m.name);option.value=m.id;select.append(option);}
      select.value=input.value;const manual=mode.value==='manual';input.hidden=!manual;select.hidden=manual;
      mode.disabled=input.disabled=disabled;select.disabled=disabled||manual;
      const messages={idle:'选择执行机器和 Agent 类型后读取模型；也可手动填写。',loading:'正在读取这台机器的模型列表；仍可手动填写。',ready:'来自所选机器的 Agent；模型权限和额度以实际执行为准。',unsupported:'此 Agent 暂不提供模型列表，可选已有配置或手动填写。',unavailable:'模型列表暂不可用，可用默认模型、已有配置或手动填写。',offline:'执行机器离线，可选已有配置或手动填写。'};
      hint.textContent=messages[status]||messages.unavailable;
      renderEffort();
    }
    mode.onchange=render;
    select.onchange=()=>{input.value=select.value;renderEffort();};
    input.oninput=()=>renderEffort();
    effort.onchange=()=>{effortValue=effort.value;renderEffort();};
    function update({runtimeID,adapterID,runtimes=[],agents=[]}){
      known=agents.filter(a=>a.runtime_id===runtimeID&&a.adapter_id===adapterID).map(a=>a.model_id);
      const runtime=runtimes.find(r=>r.runtime_id===runtimeID),caps=runtime?.capabilities?.adapters?.[adapterID];
      const nextEffortContext=JSON.stringify([runtimeID,adapterID]);
      if(effortContext&&effortContext!==nextEffortContext)effortValue='';
      effortContext=nextEffortContext;effortSupported=!!caps?.reasoning_effort;
      const key=JSON.stringify([runtimeID,adapterID,runtime?.epoch,runtime?.state,!!caps?.model_catalog,effortSupported]);
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
    return {update,setDisabled(value){disabled=!!value;render();},setValue(value){input.value=value||'';render();},getReasoningEffort(){return effortValue;}};
  }
  const exports={catalogLoader,options,mount};
  if(typeof module!=='undefined'&&module.exports)module.exports=exports;
  else window.WAModels=exports;
})();
