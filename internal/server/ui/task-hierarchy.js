'use strict';
(()=>{
  const attentionStates=['WAITING_REVIEW','WAITING_INPUT','BLOCKED'];
  const needsAttention=t=>attentionStates.includes(t.state)||(t.subtasks?.needs_attention||0)>0;
  function matchesFilter(t,filter){
    if(filter==='ATTENTION')return needsAttention(t);
    if(WAAssignment.matchesFilter(t,filter))return true;
    const field={BLOCKED:'blocked',WAITING_REVIEW:'waiting_review',WAITING_INPUT:'waiting_input'}[filter];
    return !!field&&(t.subtasks?.[field]||0)>0;
  }
  function bucket(t){
    if(needsAttention(t))return 'attention';
    if(['NEW','QUEUED'].includes(t.state))return 'queued';
    if(t.state==='COMPLETED')return 'completed';
    if(['ASSIGNED','IN_PROGRESS','WAITING_SUBTASKS'].includes(t.state))return 'active';
    return '';
  }
  function progressText(t){
    const p=t.subtasks;if(!p)return '';
    const parts=[];
    if(p.total)parts.push('子任务 '+p.completed+'/'+p.total+' 已完成');
    if(p.needs_attention)parts.push(p.needs_attention+' 项需要你');
    if(p.superseded)parts.push(p.superseded+' 项历史评审');
    return parts.join(' · ');
  }
  function mount(parentsBox,childrenBox,{navigate}){
    let selected='',historyOpen=false;
    const el=WA.el;
    const link=(task,prefix)=>{
      const button=el('button',prefix+task.title,'hierarchy-link');
      button.type='button';button.onclick=()=>navigate(task.task_id);return button;
    };
    function update(data,agents=[],roles=[]){
      const h=data?.hierarchy;
      if(h?.task_id!==selected){selected=h?.task_id||'';historyOpen=false;}
      parentsBox.replaceChildren();childrenBox.replaceChildren();
      parentsBox.hidden=!h?.parents?.length;childrenBox.hidden=!h?.children?.length;
      if(!h)return;
      for(const root of h.roots||[])parentsBox.append(link(root,'原始任务 · '));
      for(const parent of h.parents||[])if(!(h.roots||[]).some(r=>r.task_id===parent.task_id))parentsBox.append(link(parent,'上一级 · '));
      if(!h.children.length)return;
      const current=h.children.filter(t=>t.source_review_state!=='SUPERSEDED');
      const historical=h.children.filter(t=>t.source_review_state==='SUPERSEDED');
      const head=el('div',null,'panel-head');
      head.append(el('h2','子任务'),el('span',current.length+' 项当前子任务'+(historical.length?' · '+historical.length+' 项历史评审':''),'muted'));
      childrenBox.append(head,el('p','展开原任务的执行分工。点击子任务可查看对话、交付物、评审意见及其下级任务。','subtask-note'));
      function row(t){
        const button=el('button',null,'subtask-row');
        button.type='button';button.onclick=()=>navigate(t.task_id);
        const title=el('div',null,'subtask-title'),role=roles.find(r=>r.role_id===t.requirements?.role_id);
        const name=t.review_head_sha?(role?.name||t.title)+' · '+t.review_head_sha.slice(0,12):t.title;
        const owner=agents.find(a=>a.agent_id===(t.assigned_agent_id||t.preferred_agent_id));
        title.append(el('strong',name),el('small',(owner?.name||'待分派')+(progressText(t)?' · '+progressText(t):'')));
        const status=el('div',null,'subtask-status');
        status.append(t.source_review_state==='SUPERSEDED'?el('span','已被新版本替代','muted'):WA.badge(t.state));
        if(t.source_review_state!=='SUPERSEDED'&&t.subtasks?.needs_attention)status.append(el('small','下级任务需要你','hierarchy-attention'));
        button.append(title,status,el('span','查看 →','subtask-open'));
        return button;
      }
      const list=el('div',null,'subtask-list');
      current.sort((a,b)=>Number(needsAttention(b))-Number(needsAttention(a))).forEach(t=>list.append(row(t)));
      if(current.length)childrenBox.append(list);
      if(historical.length){
        const archive=el('details',null,'subtask-history');archive.open=historyOpen;
        archive.ontoggle=()=>{historyOpen=archive.open;};
        archive.append(el('summary','历史评审 · '+historical.length+' 项'));
        const history=el('div',null,'subtask-list');historical.forEach(t=>history.append(row(t)));archive.append(history);childrenBox.append(archive);
      }
    }
    return {update};
  }
  window.WATaskHierarchy={needsAttention,matchesFilter,bucket,progressText,mount};
})();
