'use strict';
// Suggest only adapters actually advertised by the selected machine. Never
// silently change a pinned Session or an explicitly typed adapter value.
(()=>{
  function available(runtimes,runtimeID){
    const candidates=runtimes.filter(r=>runtimeID?r.runtime_id===runtimeID:r.state==='ONLINE');
    return [...new Set(candidates.flatMap(r=>Object.entries(r.capabilities?.adapters||{}).filter(([,caps])=>caps.role_instructions).map(([name])=>name)))];
  }
  function suggested(runtimes, runtimeID, current, edited=false, pinned=false){
    if(edited||pinned)return current;
    const names=available(runtimes,runtimeID);
    if(!names.length||names.includes(current))return current;
    return names.includes('codex-agent')?'codex-agent':names[0];
  }
  function options(runtimes,runtimeID,current,edited=false,pinned=false){
    const value=suggested(runtimes,runtimeID,current,edited,pinned);
    const names=available(runtimes,runtimeID);
    const result=names.map(id=>({value:id,label:id==='codex-agent'?'Codex CLI':id==='cursor-agent'?'Cursor Agent':id,disabled:false}));
    if(value&&!names.includes(value))result.push({value,label:value+' · 当前配置（机器未提供）',disabled:true});
    if(!result.length)result.push({value:'',label:'暂无可用 Agent 类型',disabled:true});
    return {value,options:result};
  }
  if(typeof module!=='undefined'&&module.exports)module.exports={suggested,available,options};
  else window.WAAdapters={suggested,available,options};
})();
