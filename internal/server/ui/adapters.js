'use strict';
// Suggest only adapters actually advertised by the selected machine. Never
// silently change a pinned Session or an explicitly typed adapter value.
(()=>{
  function suggested(runtimes, runtimeID, current, edited=false, pinned=false){
    if(edited||pinned)return current;
    const candidates=runtimes.filter(r=>runtimeID?r.runtime_id===runtimeID:r.state==='ONLINE');
    const names=[...new Set(candidates.flatMap(r=>Object.entries(r.capabilities?.adapters||{}).filter(([,caps])=>caps.role_instructions).map(([name])=>name)))];
    if(!names.length||names.includes(current))return current;
    return names.includes('codex-agent')?'codex-agent':names[0];
  }
  if(typeof module!=='undefined'&&module.exports)module.exports={suggested};
  else window.WAAdapters={suggested};
})();
