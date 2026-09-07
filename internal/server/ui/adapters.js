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
  if(typeof module!=='undefined'&&module.exports)module.exports={suggested,available};
  else window.WAAdapters={suggested,available};
})();
