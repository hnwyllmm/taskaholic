'use strict';
window.WATaskConsultation={mount(dialog){
  const el=WA.el,state={taskID:'',item:null,busy:false,key:'',timer:null};
  dialog.classList.add('consultation-drawer');
  const shell=el('div',null,'consultation-shell'),head=el('header',null,'consultation-head'),title=el('div');
  title.append(el('span','READ-ONLY SIDECAR','eyebrow'),el('h2','任务旁路问答'),el('p','独立 Session · 不打断、不指导工作 Agent','small consultation-safety'));
  const close=el('button','关闭','consultation-close');close.type='button';head.append(title,close);
  const freshness=el('p','尚未开始咨询','consultation-freshness small muted'),thread=el('div',null,'consultation-thread');
  const form=el('form',null,'consultation-form'),label=el('label','询问当前任务','sr'),input=document.createElement('textarea');
  input.rows=3;input.maxLength=16000;input.placeholder='例如：Agent 现在做到哪一步？为什么执行这个命令？当前还有什么风险？';
  const actions=el('div',null,'row consultation-actions'),send=el('button','发送问题','primary'),stop=el('button','停止本次回答');
  send.type='submit';stop.type='button';actions.append(send,el('span','Enter 发送 · Ctrl+Enter 换行','key-hint'),stop);form.append(label,input,actions,el('p','咨询只读取系统已经记录的信息；未上报的编辑和隐藏推理不可见。要改变工作方向，请使用任务页的“补充要求或修改方向”。','small muted'));
  shell.append(head,freshness,thread,form);dialog.append(shell);
  function headers(){const token=WA.token();return token?{Authorization:'Bearer '+token}:{};}
  async function api(path,method='GET',body){const response=await fetch('/api/v1'+path,{method,headers:{...headers(),'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)}),value=await response.json();if(!response.ok)throw Error(value.error||'请求失败');return value;}
  function draw(){
    const item=state.item,nearBottom=thread.scrollTop+thread.clientHeight>=thread.scrollHeight-60;thread.replaceChildren();
    if(!item?.messages?.length)thread.append(el('p','可以询问进度、依据、风险和已记录的执行细节。咨询不会进入工作 Agent 的任务对话。','consultation-empty small muted'));
    for(const message of item?.messages||[]){const bubble=el('div',null,'consultation-message '+message.speaker),speaker=message.speaker==='user'?'你':message.speaker==='assistant'?'咨询 Agent':'系统';bubble.append(el('span',speaker+' · '+WA.date(message.created_at_ms),'small muted'),WA.markdown?WA.markdown(message.content):el('div',message.content));thread.append(bubble);}
    if(item?.state==='GENERATING')thread.append(el('div','咨询 Agent 正在根据最新快照回答…','consultation-thinking small'));
    freshness.textContent=item?.snapshot_at_ms?'读取截至 '+WA.date(item.snapshot_at_ms)+' 已持久化的信息':'尚未读取任务快照';
    if(item?.error)freshness.textContent+=' · '+item.error;
    send.disabled=state.busy||item?.state==='GENERATING';stop.disabled=state.busy||item?.state!=='GENERATING';input.disabled=state.busy||item?.state==='GENERATING';
    if(nearBottom)thread.scrollTop=thread.scrollHeight;
  }
  async function load(){if(!state.taskID)return;const response=await api('/work/tasks/'+encodeURIComponent(state.taskID)+'/consultation');state.item=response.consultation;draw();schedule();}
  function schedule(){clearTimeout(state.timer);state.timer=null;if(state.item?.state==='GENERATING')state.timer=setTimeout(()=>load().catch(error=>WA.notice(error.message,true)),1500);}
  async function ensure(){if(state.item)return;state.item=await api('/work/tasks/'+encodeURIComponent(state.taskID)+'/consultation','POST',{});draw();}
  close.onclick=()=>dialog.close();dialog.addEventListener('close',()=>{clearTimeout(state.timer);state.timer=null;});
  form.onsubmit=async event=>{event.preventDefault();const message=input.value.trim();if(!message)return;state.busy=true;draw();try{await ensure();if(!state.key)state.key=WA.key();state.item=await api('/work/tasks/'+encodeURIComponent(state.taskID)+'/consultation/messages','POST',{message,expected_version:state.item.version,idempotency_key:state.key});state.key='';input.value='';draw();schedule();}catch(error){WA.notice(error.message,true);}finally{state.busy=false;draw();}};
  WA.bindChatInput?.(input,()=>send.click());
  stop.onclick=async()=>{state.busy=true;draw();try{await api('/work/tasks/'+encodeURIComponent(state.taskID)+'/consultation/stop','POST',{});await load();}catch(error){WA.notice(error.message,true);}finally{state.busy=false;draw();}};
  return{
    async open(){if(!state.taskID)return;dialog.showModal();try{await load();await ensure();}catch(error){WA.notice(error.message,true);}},
    async update(taskID){if(state.taskID===taskID)return;clearTimeout(state.timer);state.taskID=taskID;state.item=null;state.key='';draw();try{await load();}catch(error){WA.notice(error.message,true);}},
    reset(){clearTimeout(state.timer);state.taskID='';state.item=null;state.key='';if(dialog.open)dialog.close();draw();}
  };
}};
