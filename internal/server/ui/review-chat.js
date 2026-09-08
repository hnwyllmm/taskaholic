'use strict';
window.WAReviewChat=(()=>{
  const {el,api,key,date}=WA;
  function mount(container,{changed,busyChanged=()=>{}}){
    container.classList.add('review-chat');container.hidden=true;
    const heading=el('h3','本次验收沟通'),identity=el('p',null,'review-chat-identity small muted'),scope=el('p','延续交付时的原生 Session。这里只沟通当前成果，不自动修改文件、通过或打回。','small muted'),thread=el('div',null,'review-chat-thread');thread.setAttribute('role','log');thread.setAttribute('aria-label','验收沟通记录');
    const form=el('form'),label=el('label','给交付 Agent 的问题'),input=el('textarea');input.rows=3;input.maxLength=5000;input.setAttribute('aria-label','验收沟通问题');input.placeholder='例如：为什么采用这个方案？这个结果是怎样验证的？';label.append(input);
    const actions=el('div',null,'review-chat-actions'),send=el('button','发送给交付 Agent','primary'),stop=el('button','停止本轮沟通');stop.type='button';send.type='submit';actions.append(send,stop);
    const status=el('p',null,'small review-chat-status');status.setAttribute('role','status');const error=el('p',null,'dialog-error');error.setAttribute('role','alert');error.hidden=true;form.append(label,actions);container.append(heading,identity,scope,thread,form,status,error);
    let context=null,networkBusy=false,externalBusy=false,requestKey='',threadStamp='',pending=null;
    const isWaiting=()=>!!context?.turns.some(t=>['QUEUED','RUNNING'].includes(t.state));
    const hasNative=()=>!!context?.session?.agent_session_ref&&!context.session.agent_session_ref.startsWith('workspace:');
    function controls(disabled=externalBusy){externalBusy=disabled;send.disabled=disabled||networkBusy||!context?.review||context.review.state!=='PENDING'||isWaiting()||!hasNative();input.disabled=disabled||networkBusy||!context?.review||(context.review.state!=='PENDING'&&!pending&&!input.value.trim());stop.hidden=!isWaiting();stop.disabled=disabled||networkBusy;}
    function update(data,reviewID){
      const review=reviewID?data.work.reviews.find(r=>r.review_id===reviewID):(data.work.reviews.find(r=>r.state==='PENDING')||data.work.reviews[0]);
      if(!review){container.hidden=true;context=null;controls();return;}
      const nextKey=data.detail.task.task_id+':'+review.review_id;
      if(context?.key!==nextKey){
        if(input.value.trim()&&context){pending={data,reviewID};error.hidden=false;error.textContent='验收版本已变更。请先复制或清空当前未发送的问题，再查看新版本。';context.review={...context.review,state:'SUPERSEDED'};controls();return;}
        requestKey='';threadStamp='';error.hidden=true;
      }
      pending=null;context={key:nextKey,taskID:data.detail.task.task_id,review,turns:(data.work.review_turns||[]).filter(t=>t.review_id===review.review_id),session:data.detail.session,schedulerError:data.work.config.scheduler_error};container.hidden=false;
      const sourceRun=data.detail.runs.find(r=>r.run_id===review.run_id),session=context.session,agentName=data.agents?.find(a=>a.agent_id===(sourceRun?.agent_id||session?.agent_id))?.name||sourceRun?.agent_id||session?.agent_id||'原交付 Agent';
      heading.textContent=review.state==='PENDING'?'本次验收沟通':'本次验收沟通记录';identity.textContent=agentName+' · '+(sourceRun?.model_id||session?.model_id||'默认模型')+'\nSession：'+(sourceRun?.session_id||session?.session_id||'不可用')+' · '+(sourceRun?.runtime_id||session?.runtime_id||'')+'\n验收单：'+review.review_id;
      const stamp=nextKey+':'+review.discussion_version+':'+context.turns.length;
      if(stamp!==threadStamp){const nearBottom=thread.scrollTop+thread.clientHeight>=thread.scrollHeight-60;threadStamp=stamp;thread.replaceChildren();for(const turn of context.turns){const question=el('div',null,'review-chat-message user');question.append(el('span','你 · '+date(turn.created_at_ms),'small muted'),WA.markdown?WA.markdown(turn.question):el('div',turn.question));thread.append(question);if(turn.answer){const answer=el('div',null,'review-chat-message assistant');answer.append(el('span',agentName+' · '+date(turn.finished_at_ms),'small muted'),WA.markdown?WA.markdown(turn.answer):el('div',turn.answer));thread.append(answer);}if(turn.error)thread.append(el('p',turn.error,'small review-chat-error'));}if(!context.turns.length)thread.append(el('p','可以先问依据、核对细节、讨论修改方案，再决定是否验收。','small muted'));if(nearBottom)thread.scrollTop=thread.scrollHeight;}
      const outstanding=context.turns.find(t=>['QUEUED','RUNNING'].includes(t.state));
      status.textContent=outstanding?(outstanding.state==='QUEUED'?'问题已保存，等待原 Agent / Session。'+(context.schedulerError||''):'交付 Agent 正在原 Session 中回复。回复结束后再决定验收。'):review.state!=='PENDING'?'本次验收已结束或失效，聊天记录保留。':!hasNative()?'原生 Session 引用不可用，不能用新会话代替。':'聊清楚后，再点击验收通过或要求修改。';
      controls();
    }
    async function perform(fn){if(networkBusy||externalBusy)return;networkBusy=true;error.hidden=true;controls();busyChanged();try{await fn();}catch(e){error.textContent=e.message;error.hidden=false;try{await changed();}catch{}}finally{networkBusy=false;controls();busyChanged();}}
    form.onsubmit=e=>{e.preventDefault();if(send.disabled)return;perform(async()=>{const question=input.value.trim();if(!question)throw Error('请先写下要和交付者沟通的问题。');if(!requestKey)requestKey=key();const c=context;await api('/work/tasks/'+c.taskID+'/reviews/'+c.review.review_id+'/messages','POST',{message:question,idempotency_key:requestKey,expected_discussion_version:c.review.discussion_version||0});input.value='';requestKey='';await changed();});};
    stop.onclick=()=>perform(async()=>{const turn=context.turns.find(t=>['QUEUED','RUNNING'].includes(t.state));if(!turn)return;await api('/work/tasks/'+context.taskID+'/reviews/'+context.review.review_id+'/messages/'+turn.turn_id+'/stop','POST',{});await changed();});
    input.oninput=()=>{requestKey='';if(!input.value.trim()&&pending){const next=pending;context=null;update(next.data,next.reviewID);}controls();busyChanged();};
    return{update,controls,isWaiting,isBusy:()=>networkBusy,hasDraft:()=>!!input.value.trim(),clearDraft:()=>{input.value='';requestKey='';if(pending){const next=pending;context=null;update(next.data,next.reviewID);}},reset:()=>{input.value='';requestKey='';context=null;pending=null;threadStamp='';container.hidden=true;}};
  }
  return{mount};
})();
