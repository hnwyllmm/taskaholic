'use strict';
// The wire parser is deliberately independent of the DOM so chunk boundaries,
// reconnect cursors and untrusted output can be regression-tested without a model.
(function(root){
  const active=state=>state==='RUNNING'||state==='PENDING';
  const actionKey=a=>JSON.stringify([a.run_id,a.action_id]);
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
  if(typeof module!=='undefined'&&module.exports){module.exports={parser,merge,active,actionKey};return;}
  function mount(container,{onTaskEvent=()=>{}}={}){
    const el=WA.el,items=new Map(),cards=new Map();
    const head=el('div',null,'panel-head'),title=el('h2','Agent 实时行动'),connection=el('span','尚未连接','activity-connection');
    connection.setAttribute('role','status');head.append(title,connection);
    const now=el('div',null,'activity-now'),current=el('strong'),last=el('p',null,'small muted');now.append(current,last);
    const tools=el('div',null,'activity-toolbar'),filterLabel=el('label','查看轮次'),filter=el('select'),count=el('span',null,'small muted'),reconnect=el('button','重新连接');
    filter.setAttribute('aria-label','筛选行动轮次');filterLabel.append(filter);tools.append(filterLabel,count,reconnect);
    const list=el('div',null,'activity-list'),empty=el('p',null,'activity-empty'),older=el('button','加载更早的行动'),note=el('p','只展示 Agent 实际上报的行动；更新粒度取决于执行器，不展示内部思考。常见密钥格式会脱敏，长输出会截断。','activity-note');
    older.className='activity-older';container.classList.add('panel','activity-panel');container.append(head,now,tools,empty,list,older,note);
    let taskID='',detail=null,agents=[],runtimes=[],generation=0,cursor=0,before=0,hasMore=false,loadingOlder=false;
    let stream=null,retry=null,watchdog=null,frame=null,connected=false,disposed=false,lastByte=0,attempt=0,filterKey='',lastLegacy=null;
    const stateNames={RUNNING:'执行中',PENDING:'等待中',COMPLETED:'已完成',FAILED:'失败',INTERRUPTED:'已中断',UNKNOWN:'结果未知'};
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
        const box=el('details',null,'activity-card'),summary=el('summary'),name=el('strong'),status=el('span',null,'activity-status'),time=el('span',null,'activity-time');
        summary.append(status,name,time);box.append(summary);
        const body=el('div',null,'activity-body'),meta=el('p',null,'activity-meta'),command=el('pre',null,'activity-command'),details=el('pre'),output=el('pre',null,'activity-output'),error=el('p',null,'activity-error'),flags=el('p',null,'small muted');
        body.append(meta,command,details,output,error,flags);box.append(body);box.open=active(a.state);box.dataset.actionId=a.action_id;
        c={box,name,status,time,meta,command,details,output,error,flags};cards.set(key,c);
      }
      c.box.dataset.state=a.state;c.name.textContent=a.title;c.status.textContent=stateNames[a.state]||a.state;c.time.textContent=elapsed(a);c.meta.textContent=metadata(a)+'\n'+new Date(a.updated_at_ms).toLocaleString();
      for(const field of ['command','details','output','error']){c[field].hidden=!a[field];if(c[field].textContent!==(a[field]||'')){const bottom=c[field].scrollTop+c[field].clientHeight>=c[field].scrollHeight-20;c[field].textContent=a[field]||'';if(bottom)c[field].scrollTop=c[field].scrollHeight;}}
      c.flags.textContent=[a.exit_code!=null?'退出码 '+a.exit_code:'',a.redacted?'已对常见密钥格式脱敏':'',a.truncated?'内容已截断':''].filter(Boolean).join(' · ');
      return c.box;
    }
    function render(){
      const sorted=[...items.values()].sort((a,b)=>b.first_seq-a.first_seq),visible=sorted.filter(a=>!filter.value||a.run_id===filter.value);
      // Insert/move existing nodes only: keep expanded details, selection and scroll.
      let index=0;for(const a of visible){const box=card(a);if(list.children[index]!==box)list.insertBefore(box,list.children[index]||null);index++;}
      while(list.children.length>index)list.lastElementChild.remove();
      for(const [key,c] of cards)if(!items.has(key)){c.box.remove();cards.delete(key);}
      count.textContent=visible.length+' 条行动';empty.hidden=visible.length>0;
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
      current.textContent=offline?'原执行机器离线或心跳待确认；显示最后收到的行动':doing.length?doing[0].title+(doing.length>1?' · 另有 '+(doing.length-1)+' 项未结束':''):liveRuns.some(r=>r.state==='RUNNING')?'运行中，等待下一条行动事件':liveRuns.length?'已进入等待队列':'当前没有正在运行的 Agent';
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
      close();taskID=id;cursor=0;before=0;hasMore=false;lastLegacy=null;items.clear();cards.clear();list.replaceChildren();filter.value='';filterKey='';
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
      detail=data?.detail||null;agents=knownAgents;runtimes=knownRuntimes;
      if(!detail){attach('');return;}
      attach(detail.task.task_id);
      const signature=detail.runs.map(r=>r.run_id+':'+r.state).join('|');
      if(signature!==filterKey){filterKey=signature;const selected=filter.value;filter.replaceChildren(el('option','全部轮次'));filter.firstChild.value='';for(const r of [...detail.runs].reverse()){const o=el('option',(stateNames[r.state]||r.state)+' · '+r.run_id);o.value=r.run_id;filter.append(o);}filter.value=selected;}
      const legacy=[...(detail.events||[])].reverse().find(e=>e.event_type==='RunProgress'&&e.payload.message);
      if(legacy&&(!lastLegacy||legacy.occurred_at_ms>lastLegacy.time))lastLegacy={message:legacy.payload.message,time:legacy.occurred_at_ms};scheduleRender();
    }
    filter.onchange=render;reconnect.onclick=()=>attach(taskID,true);
    older.onclick=async()=>{if(loadingOlder||!hasMore||items.size>=500)return;loadingOlder=true;render();const gen=generation;
      try{const page=await WA.api('/work/tasks/'+encodeURIComponent(taskID)+'/activities?before='+before+'&limit=50');if(gen!==generation)return;for(const a of page.items)merge(items,a);before=page.next_before;hasMore=page.has_more;}
      catch(e){if(gen===generation)connectionState('历史读取失败：'+e.message,connected);}finally{loadingOlder=false;if(gen===generation)render();}
    };
    const ticker=setInterval(tick,1000);
    return{update,reconnect:()=>attach(taskID,true),isConnected:()=>connected,controls:()=>{older.disabled=loadingOlder||items.size>=500;reconnect.disabled=!taskID;},dispose:()=>{disposed=true;close();clearInterval(ticker);if(frame!==null)cancelAnimationFrame(frame);}};
  }
  root.WAActivity={mount};
})(typeof window==='undefined'?globalThis:window);
