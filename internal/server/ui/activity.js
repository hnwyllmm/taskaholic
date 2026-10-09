'use strict';
// The wire parser is deliberately independent of the DOM so chunk boundaries,
// reconnect cursors and untrusted output can be regression-tested without a model.
(function(root){
  const active=state=>state==='RUNNING'||state==='PENDING';
  const actionKey=a=>JSON.stringify([a.run_id,a.action_id]);
  const defaultOpen=kind=>kind==='message';
  const oneLine=(value,max=260)=>{const text=String(value||'').replace(/\s+/g,' ').trim();return text.length>max?text.slice(0,max-1)+'…':text;};
  function changedFiles(details){
    try{
      const entries=JSON.parse(details||'[]');
      if(!Array.isArray(entries))return [];
      return entries.map(entry=>entry?.path).filter(Boolean).map(path=>{
        const repository='/repository/',index=path.lastIndexOf(repository);
        return index===-1?path:path.slice(index+repository.length);
      });
    }catch{return [];}
  }
  function compactSummary(a){
    if(a.kind==='command')return{prefix:'Run:',value:oneLine(a.command||a.title)};
    if(a.kind==='file_change'){
      const files=changedFiles(a.details),first=files[0]||oneLine(a.title)||'文件';
      return{prefix:'Changed:',value:first+(files.length>1?'，另 '+(files.length-1)+' 个文件':'')};
    }
    if(a.kind==='search')return{prefix:'Search:',value:oneLine(a.details||a.command||a.title)};
    if(a.kind==='tool')return{prefix:'Tool:',value:oneLine(a.title)};
    return{prefix:'',value:oneLine(a.title)||'行动'};
  }
  const messageText=a=>a.output||a.details||a.title||'';
  const roundMessages=(messages,runID)=>({
    triggers:(messages||[]).filter(m=>m.run_id===runID&&m.delivery==='SENT'),
    responses:(messages||[]).filter(m=>m.run_id===runID&&m.delivery!=='SENT'),
  });
  function parser(onEvent,onHeartbeat=()=>{}){
    let buffer='',data=[],id='',type='',size=0;
    return chunk=>{
      buffer+=chunk;
      let end;
      while((end=buffer.indexOf('\n'))!==-1){
        let line=buffer.slice(0,end);buffer=buffer.slice(end+1);if(line.endsWith('\r'))line=line.slice(0,-1);
        if(line===''){
          if(data.length)onEvent({id,type,data:data.join('\n')});
          data=[];id='';type='';size=0;continue;
        }
        if(line.startsWith(':')){onHeartbeat();continue;}
        size+=line.length;if(size>512*1024)throw Error('行动事件超过读取上限');
        const colon=line.indexOf(':'),field=colon===-1?line:line.slice(0,colon);
        let value=colon===-1?'':line.slice(colon+1);if(value.startsWith(' '))value=value.slice(1);
        if(field==='data')data.push(value);else if(field==='id'&&!value.includes('\0'))id=value;else if(field==='event')type=value;
      }
      if(buffer.length>512*1024)throw Error('行动事件超过读取上限');
    };
  }
  // Output is a provider snapshot, never a delta to append a second time.
  function merge(items,item){
    const key=actionKey(item),old=items.get(key);
    if(old&&old.last_seq>=item.last_seq)return false;
    items.set(key,{...item,first_seq:item.first_seq||old?.first_seq||item.last_seq});return true;
  }
  if(typeof module!=='undefined'&&module.exports){module.exports={parser,merge,active,actionKey,defaultOpen,changedFiles,compactSummary,messageText,roundMessages};return;}
  function mount(container,{onTaskEvent=()=>{}}={}){
    const el=WA.el,items=new Map(),cards=new Map(),rounds=new Map();
    const head=el('div',null,'panel-head activity-head'),heading=el('div',null,'activity-heading'),title=el('h2','任务对话与实时运行'),subtitle=el('span','每轮对话、Agent 行动和正式回复保存在一起','small muted'),connection=el('span','尚未连接','activity-connection');
    heading.append(title,subtitle);connection.setAttribute('role','status');head.append(heading,connection);
    const now=el('div',null,'activity-now'),pulse=el('span',null,'activity-pulse'),nowText=el('div'),current=el('strong'),last=el('p',null,'small muted');nowText.append(current,last);now.append(pulse,nowText);
    const tools=el('div',null,'activity-toolbar'),filter=el('select'),count=el('span',null,'small muted'),reconnect=el('button','重新连接');
    filter.hidden=true;tools.append(count,reconnect);
    const list=el('div',null,'activity-list'),empty=el('p',null,'activity-empty'),older=el('button','加载更早的行动'),note=el('p','只展示 Agent 实际上报的行动；更新粒度取决于执行器，不展示内部思考。常见密钥格式会脱敏，长输出会截断。','activity-note');
    older.className='activity-older';container.classList.add('panel','activity-panel');container.append(head,now,tools,empty,older,list,note);
    let taskID='',detail=null,work=null,agents=[],runtimes=[],generation=0,cursor=0,before=0,hasMore=false,loadingOlder=false,lastRoundID='';
    let stream=null,retry=null,watchdog=null,frame=null,connected=false,disposed=false,lastByte=0,attempt=0,filterKey='',lastLegacy=null;
    // A Run ending successfully only means this invocation returned a valid
    // business result. The root task may still be waiting for input or review.
    const stateNames={RUNNING:'本轮执行中',PENDING:'本轮等待中',COMPLETED:'本轮已结束',FAILED:'本轮执行失败',INTERRUPTED:'本轮已中断',UNKNOWN:'本轮结果未知'};
    const kindNames={command:'命令',tool:'工具',file_change:'文件',search:'检索',plan:'计划',message:'消息'};
    const headers=()=>WA.token()?{Authorization:'Bearer '+WA.token()}:{};
    function connectionState(text,isConnected=false){connected=isConnected;connection.textContent=text;connection.dataset.connected=String(isConnected);}
    function close(){generation++;stream?.abort();stream=null;clearTimeout(retry);clearInterval(watchdog);retry=null;watchdog=null;connected=false;}
    function scheduleRender(){if(frame===null)frame=requestAnimationFrame(()=>{frame=null;render();});}
    function metadata(a){const member=agents.find(x=>x.agent_id===a.agent_id);return(member?.name||a.agent_id||'Agent')+' · '+a.runtime_id+' · '+(a.model_id||'默认模型')+'\nRun '+a.run_id+'\nSession '+a.session_id;}
    function runtimeOnline(id){const rt=runtimes.find(x=>x.runtime_id===id);return !!rt&&rt.state==='ONLINE'&&Date.now()-rt.last_seen_at_ms<30000;}
    function elapsed(a){
      const live=active(a.state)&&connected&&runtimeOnline(a.runtime_id);
      const end=a.finished_at_ms||(live?Date.now():a.updated_at_ms);
      const ms=a.duration_ms??Math.max(0,end-a.started_at_ms),seconds=Math.floor(ms/1000);
      const text=seconds<60?seconds+' 秒':Math.floor(seconds/60)+' 分 '+seconds%60+' 秒';
      return(a.duration_ms==null?'观察耗时 ':'执行耗时 ')+text+(active(a.state)&&!live?'（截至最后记录）':'');
    }
    function card(a){
      const key=actionKey(a);let c=cards.get(key);
      if(!c){
        if(a.kind==='message'){
          const box=el('article',null,'activity-card activity-message'),header=el('div',null,'activity-message-head'),identity=el('strong'),status=el('span',null,'activity-status'),time=el('span',null,'activity-time'),content=el('div',null,'activity-message-content');
          header.append(identity,status,time);box.append(header,content);box.dataset.actionId=a.action_id;
          c={box,identity,status,time,content,messageValue:''};cards.set(key,c);
        }else{
        const box=el('details',null,'activity-card'),summary=el('summary'),marker=el('span',null,'activity-marker'),main=el('span',null,'activity-main'),top=el('span',null,'activity-card-top'),name=el('strong'),kind=el('span',null,'activity-kind'),bottom=el('span',null,'activity-card-bottom'),status=el('span',null,'activity-status'),time=el('span',null,'activity-time');
        const prefix=el('span',null,'activity-summary-prefix'),value=el('code',null,'activity-summary-value');name.append(prefix,value);top.append(name,kind);bottom.append(status,time);main.append(top,bottom);summary.append(marker,main);box.append(summary);
        const body=el('div',null,'activity-body'),meta=el('p',null,'activity-meta'),commandWrap=el('section',null,'activity-block'),commandLabel=el('h3','执行命令'),command=el('pre',null,'activity-command'),detailsWrap=el('section',null,'activity-block'),detailsLabel=el('h3','详细信息'),details=el('pre'),outputWrap=el('section',null,'activity-block'),outputLabel=el('h3','输出'),output=el('pre',null,'activity-output'),errorWrap=el('section',null,'activity-block activity-error-block'),errorLabel=el('h3','错误'),error=el('p',null,'activity-error'),flags=el('p',null,'small muted activity-flags');
        commandWrap.append(commandLabel,command);detailsWrap.append(detailsLabel,details);outputWrap.append(outputLabel,output);errorWrap.append(errorLabel,error);body.append(meta,commandWrap,detailsWrap,outputWrap,errorWrap,flags);box.append(body);box.open=defaultOpen(a.kind);box.dataset.actionId=a.action_id;
        c={box,name,prefix,value,kind,status,time,meta,command,details,output,error,flags,commandWrap,detailsWrap,outputWrap,errorWrap};cards.set(key,c);
        }
      }
      c.box.dataset.state=a.state;c.box.dataset.kind=a.kind||'unknown';c.status.textContent=stateNames[a.state]||a.state;c.time.textContent=elapsed(a);
      if(a.kind==='message'){
        const member=agents.find(x=>x.agent_id===a.agent_id);c.identity.textContent=member?.name||'Agent 消息';
        const value=messageText(a);if(c.messageValue!==value){c.messageValue=value;c.content.replaceChildren(WA.markdown?WA.markdown(value):el('div',value));}
        return c.box;
      }
      const summary=compactSummary(a);c.prefix.textContent=summary.prefix;c.value.textContent=summary.value;c.kind.textContent=kindNames[a.kind]||'行动';c.meta.textContent=metadata(a)+'\n'+new Date(a.updated_at_ms).toLocaleString();
      for(const field of ['command','details','output','error']){c[field+'Wrap'].hidden=!a[field];if(c[field].textContent!==(a[field]||'')){const bottom=c[field].scrollTop+c[field].clientHeight>=c[field].scrollHeight-20;c[field].textContent=a[field]||'';if(bottom)c[field].scrollTop=c[field].scrollHeight;}}
      c.flags.textContent=[a.exit_code!=null?'退出码 '+a.exit_code:'',a.redacted?'已对常见密钥格式脱敏':'',a.truncated?'内容已截断':''].filter(Boolean).join(' · ');
      return c.box;
    }
    function messageContent(message){
      if(root.WATaskReferences&&detail?.task&&work)return root.WATaskReferences.message(detail.task,work,message);
      return message.content||'';
    }
    function messageBubble(message,position){
      const bubble=el('article',null,'round-message '+position+' '+(message.speaker||'system'));
      const label=message.speaker==='assistant'?'Agent 正式回复':message.speaker==='user'?'你发起':'系统发起';
      bubble.append(el('span',label+' · '+new Date(message.created_at_ms).toLocaleString(),'round-message-label'));
      bubble.append(WA.markdown?WA.markdown(messageContent(message)):el('div',messageContent(message)));
      if(message.delivery==='PENDING')bubble.append(el('span','已保存，等待下一轮处理','round-message-delivery'));
      return bubble;
    }
    function roundCard(run,index,isLatest){
      let round=rounds.get(run.run_id);
      if(!round){
        const box=el('details',null,'activity-round'),summary=el('summary'),main=el('span',null,'round-summary-main'),name=el('strong'),meta=el('span',null,'round-summary-meta'),state=el('span',null,'round-summary-state'),body=el('div',null,'round-body'),inputs=el('div',null,'round-inputs'),actions=el('div',null,'round-actions'),replies=el('div',null,'round-replies');
        main.append(name,meta);summary.append(main,state);body.append(inputs,actions,replies);box.append(summary,body);summary.addEventListener('click',()=>{round.touched=true;});
        round={box,name,meta,state,body,inputs,actions,replies,touched:false};rounds.set(run.run_id,round);
      }
      const {triggers,responses}=roundMessages(work?.messages,run.run_id);
      const trigger=triggers.at(-1),member=agents.find(a=>a.agent_id===run.agent_id),label=trigger?oneLine(messageContent(trigger),90):'自动进入本轮处理';
      round.name.textContent=`第 ${index+1} 轮 · ${label}`;
      round.meta.textContent=[member?.name||run.agent_id||'Agent',run.model_id||'默认模型',new Date(run.created_at_ms).toLocaleString()].filter(Boolean).join(' · ');
      round.state.textContent=stateNames[run.state]||run.state;round.box.dataset.state=run.state;
      round.inputs.replaceChildren(...triggers.map(m=>messageBubble(m,'trigger')));
      round.replies.replaceChildren(...responses.map(m=>messageBubble(m,m.speaker==='assistant'?'reply':'result')));
      if(!round.touched)round.box.open=isLatest;
      return round;
    }
    function pendingRound(messages){
      const run={run_id:'pending',state:'PENDING',created_at_ms:messages[0]?.created_at_ms||Date.now()};
      let round=rounds.get(run.run_id);
      if(!round){
        const box=el('details',null,'activity-round pending-round'),summary=el('summary'),main=el('span',null,'round-summary-main'),name=el('strong','等待下一轮'),meta=el('span','消息已经保存，调度后会与新 Run 自动关联','round-summary-meta'),state=el('span','等待中','round-summary-state'),body=el('div',null,'round-body'),inputs=el('div',null,'round-inputs'),actions=el('div',null,'round-actions'),replies=el('div',null,'round-replies');
        main.append(name,meta);summary.append(main,state);body.append(inputs,actions,replies);box.append(summary,body);round={box,name,meta,state,body,inputs,actions,replies,touched:false};rounds.set(run.run_id,round);
      }
      round.inputs.replaceChildren(...messages.map(m=>messageBubble(m,'trigger')));round.box.open=true;return round;
    }
    function render(){
      const followTail=list.scrollTop+list.clientHeight>=list.scrollHeight-36;
      const sorted=[...items.values()].sort((a,b)=>a.first_seq-b.first_seq),runs=detail?.runs||[],pending=(work?.messages||[]).filter(m=>!m.run_id&&m.delivery==='PENDING');
      const newest=runs.at(-1)?.run_id||'',newRound=newest&&newest!==lastRoundID;if(newRound){for(const [id,round] of rounds)if(id!==newest&&!round.touched)round.box.open=false;lastRoundID=newest;}
      const visible=[];for(const [index,run] of runs.entries()){
        const round=roundCard(run,index,run.run_id===newest);round.actions.replaceChildren(...sorted.filter(a=>a.run_id===run.run_id).map(card));visible.push(round.box);
      }
      if(pending.length)visible.push(pendingRound(pending).box);else if(rounds.has('pending')){rounds.get('pending').box.remove();rounds.delete('pending');}
      list.replaceChildren(...visible);if(newRound||followTail)list.scrollTop=list.scrollHeight;
      for(const [key,c] of cards)if(!items.has(key)){c.box.remove();cards.delete(key);}
      count.textContent=runs.length+' 轮对话 · '+sorted.length+' 条行动';empty.hidden=visible.length>0;
      const running=detail?.runs?.some(r=>active(r.state)||r.state==='QUEUED');
      empty.textContent=lastLegacy?'最新日志：'+lastLegacy.message:running?'运行已开始，等待执行器上报行动。没有新事件不代表卡住。':'暂无结构化行动。升级前的历史任务请查看下方「运行与事件记录」。';
      older.hidden=!hasMore;older.disabled=loadingOlder||items.size>=500;
      older.textContent=items.size>=500?'已显示 500 条；更早记录仍保存在数据库':'加载更早的行动';tick();
    }
    function tick(){
      if(!taskID)return;
      const all=[...items.values()],liveRuns=detail?.runs?.filter(r=>active(r.state)||r.state==='QUEUED')||[];
      const doing=all.filter(a=>active(a.state)).sort((a,b)=>b.last_seq-a.last_seq);
      const latest=all.reduce((best,a)=>!best||a.last_seq>best.last_seq?a:best,null);
      const offline=liveRuns.some(r=>!runtimeOnline(r.runtime_id));
      const isWorking=!offline&&(doing.length||liveRuns.length);now.dataset.active=String(!!isWorking);current.textContent=offline?'原执行机器离线或心跳待确认；显示最后收到的行动':doing.length?doing[0].title+(doing.length>1?' · 另有 '+(doing.length-1)+' 项未结束':''):liveRuns.some(r=>r.state==='RUNNING')?'运行中，等待下一条行动事件':liveRuns.length?'已进入等待队列':'当前没有正在运行的 Agent';
      const lastTime=Math.max(latest?.updated_at_ms||0,lastLegacy?.time||0);
      last.textContent=(lastTime?'最后行动：'+new Date(lastTime).toLocaleString():'尚未收到行动记录')+' · '+(connected?'页面事件连接正常；不代表 Agent 始终有输出':'页面未实时连接，正在显示已读取的记录');
      for(const [key,c] of cards){const a=items.get(key);if(a)c.time.textContent=elapsed(a);}
    }
    function accept(e){
      if(!Number.isSafeInteger(e.global_seq)||e.global_seq<=cursor)return;
      if(e.correlation_id!==taskID)throw Error('收到不属于当前任务的事件');
      if(e.event_type==='RunActivity'){
        const a={...e.payload,last_seq:e.global_seq};if(a.task_id!==taskID||!a.action_id||!a.run_id)throw Error('行动归属不完整');
        merge(items,a);
        // Bounded DOM/cache. The SQLite history remains complete and pageable.
        const sorted=[...items.values()].sort((a,b)=>a.first_seq-b.first_seq);
        for(const old of sorted){if(items.size<=500)break;if(!active(old.state)){items.delete(actionKey(old));hasMore=true;}}
        scheduleRender();
      }else if(e.event_type==='RunProgress'){
        lastLegacy={message:e.payload.message||'Agent 已上报进度',time:e.occurred_at_ms};scheduleRender();
      }else onTaskEvent(e);
      cursor=e.global_seq;
    }
    async function connect(gen){
      if(gen!==generation||!taskID||disposed)return;
      const controller=new AbortController();stream=controller;lastByte=Date.now();
      clearInterval(watchdog);watchdog=setInterval(()=>{if(Date.now()-lastByte>20000)controller.abort();},5000);
      try{
        const response=await fetch('/api/v1/work/tasks/'+encodeURIComponent(taskID)+'/events?after='+cursor,{headers:headers(),signal:controller.signal,cache:'no-store'});
        if(gen!==generation){await response.body?.cancel();return;}
        if(!response.ok||!response.body){const err=Error(response.status===401?'请在连接设置中填写 API Token':'事件连接失败 ('+response.status+')');err.fatal=[401,403,404].includes(response.status);throw err;}
        connectionState('实时连接',true);attempt=0;
        const feed=parser(frame=>{const event=JSON.parse(frame.data);if(frame.id&&Number(frame.id)!==event.global_seq)throw Error('事件序号不一致');accept(event);},()=>{lastByte=Date.now();});
        const reader=response.body.getReader(),decoder=new TextDecoder();
        try{while(gen===generation){const result=await reader.read();if(result.done)break;lastByte=Date.now();feed(decoder.decode(result.value,{stream:true}));}}finally{await reader.cancel().catch(()=>{});reader.releaseLock();}
        if(gen===generation)throw Error('事件连接已断开');
      }catch(error){
        if(gen!==generation||disposed)return;
        connectionState(error.fatal?error.message:'连接中断，正在续传…');tick();
        if(!error.fatal)retry=setTimeout(()=>connect(gen),Math.min(15000,1000*2**Math.min(attempt++,4)));
      }finally{if(gen===generation){clearInterval(watchdog);watchdog=null;}}
    }
    async function attach(id,force=false){
      if(id===taskID&&!force)return;
      close();taskID=id;cursor=0;before=0;hasMore=false;lastLegacy=null;lastRoundID='';items.clear();cards.clear();rounds.clear();list.replaceChildren();filter.value='';filterKey='';
      if(!id){connectionState('尚未连接');return;}
      connectionState('读取行动记录…');render();const gen=generation;
      async function snapshot(){
        try{const page=await WA.api('/work/tasks/'+encodeURIComponent(id)+'/activities');if(gen!==generation)return;
          for(const a of page.items)merge(items,a);cursor=page.cursor;before=page.next_before;hasMore=page.has_more;render();connect(gen);
        }catch(error){if(gen!==generation)return;connectionState(error.message);retry=setTimeout(snapshot,5000);}
      }
      await snapshot();
    }
    function update(data,knownAgents=[],knownRuntimes=[]){
      detail=data?.detail||null;work=data?.work||null;agents=knownAgents;runtimes=knownRuntimes;
      if(!detail){attach('');return;}
      attach(detail.task.task_id);
      const signature=detail.runs.map(r=>r.run_id+':'+r.state).join('|');
      if(signature!==filterKey){filterKey=signature;const selected=filter.value;filter.replaceChildren(el('option','全部轮次'));filter.firstChild.value='';for(const r of [...detail.runs].reverse()){const o=el('option',(stateNames[r.state]||r.state)+' · '+r.run_id);o.value=r.run_id;filter.append(o);}filter.value=selected;}
      const legacy=[...(detail.events||[])].reverse().find(e=>e.event_type==='RunProgress'&&e.payload.message);
      if(legacy&&(!lastLegacy||legacy.occurred_at_ms>lastLegacy.time))lastLegacy={message:legacy.payload.message,time:legacy.occurred_at_ms};scheduleRender();
    }
    filter.onchange=render;reconnect.onclick=()=>attach(taskID,true);
    older.onclick=async()=>{if(loadingOlder||!hasMore||items.size>=500)return;loadingOlder=true;render();const gen=generation,oldHeight=list.scrollHeight;
      try{const page=await WA.api('/work/tasks/'+encodeURIComponent(taskID)+'/activities?before='+before+'&limit=50');if(gen!==generation)return;for(const a of page.items)merge(items,a);before=page.next_before;hasMore=page.has_more;render();list.scrollTop+=list.scrollHeight-oldHeight;}
      catch(e){if(gen===generation)connectionState('历史读取失败：'+e.message,connected);}finally{loadingOlder=false;if(gen===generation)render();}
    };
    const ticker=setInterval(tick,1000);
    return{update,reconnect:()=>attach(taskID,true),isConnected:()=>connected,controls:()=>{older.disabled=loadingOlder||items.size>=500;reconnect.disabled=!taskID;},dispose:()=>{disposed=true;close();clearInterval(ticker);if(frame!==null)cancelAnimationFrame(frame);}};
  }
  root.WAActivity={mount};
})(typeof window==='undefined'?globalThis:window);
