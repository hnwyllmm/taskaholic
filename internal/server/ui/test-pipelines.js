'use strict';
window.WATestPipelines=(()=>{
  const names={QUEUED:'等待发起',SUBMITTING:'正在提交',UNCERTAIN:'提交待确认',ERROR:'未能发起',SUPERSEDED:'版本已变化',created:'已创建',pending:'排队中',preparing:'准备中',waiting_for_resource:'等待资源',running:'测试中',success:'通过',failed:'失败',canceled:'已取消',canceling:'正在取消',skipped:'已跳过',manual:'需要人工操作',scheduled:'已计划'};
  const base='https://gitlab.oceanbase-dev.com/obqa/seekdb_test/-/';
  const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  function link(text,id,kind){const n=el('a',text);n.href=base+kind+'/'+id;n.target='_blank';n.rel='noopener noreferrer';return n;}
  function blocked(items=[]){
    const groups=new Map();
    for(const p of items){if(!groups.has(p.pr_target_id))groups.set(p.pr_target_id,[]);groups.get(p.pr_target_id).push(p);}
    return [...groups.values()].some(group=>group.filter(p=>p.current).sort((a,b)=>b.attempt-a.attempt)[0]?.state!=='success');
  }
  function render(root,items=[],{resolve}={}){
    root.replaceChildren();root.hidden=!items.length;if(!items.length)return;
    const header=el('div',null,'panel-head');header.append(el('h2','回归测试'),el('span',blocked(items)?'最新版本尚未通过':'最新版本已通过','muted'));root.append(header);
    root.append(el('p','QA 决定是否需要测试；原任务 Agent 处理失败。旧版本结果不作为当前版本的验收证据。','small muted pipeline-note'));
    const scroll=el('div',null,'pipeline-scroll'),table=el('table',null,'pipeline-table'),head=el('thead'),titles=el('tr'),body=el('tbody');
    for(const name of ['Pipeline / 申请','被测版本','状态','尝试','记录']){const th=el('th',name);th.scope='col';titles.append(th);}head.append(titles);table.append(head,body);
    for(const p of items){
      const row=el('tr'),identity=el('th');identity.scope='row';
      identity.append(Number.isSafeInteger(p.pipeline_id)&&p.pipeline_id>0?link('#'+p.pipeline_id,p.pipeline_id,'pipelines'):el('span','待取得编号'));
      const version=el('td'),sha=el('code',(p.head_sha||'').slice(0,12));sha.title=p.head_sha;version.append(sha,el('small',p.current?'当前 PR 版本':'历史版本','pipeline-version'));
      const status=el('td'),badge=el('span',names[p.state]||p.state,'badge');badge.dataset.state=p.state==='success'?'COMPLETED':['failed','ERROR','UNCERTAIN','manual','canceled','skipped'].includes(p.state)?'BLOCKED':'QUEUED';status.append(badge);
      const attempt=el('td',String(p.attempt||1)),info=el('td'),details=el('details');details.append(el('summary','查看详情'));
      details.append(el('p',p.reason,'prewrap'),el('p','申请：'+p.request_id,'small prewrap'),el('p','发起于 '+new Date(p.created_at_ms).toLocaleString(),'small muted'));
      if(p.config_sha)details.append(el('p','测试配置 master-pipeline @ '+p.config_sha+' · JOBS=all · RUN_PROFILE=1','small prewrap'));
      if(p.retry_of)details.append(el('p','重试自：'+p.retry_of,'small prewrap'));
      if(p.error)details.append(el('p',p.error,'small error'));
      if(resolve&&['SUBMITTING','UNCERTAIN'].includes(p.state)&&Date.now()-(p.submitted_at_ms||Date.now())>=120000){const button=el('button','已核实未创建，允许重试');button.type='button';button.onclick=()=>resolve(p);details.append(button);}
      if(p.poll_error)details.append(el('p','轮询需要检查：'+p.poll_error,'small error'));
      if(p.last_polled_ms)details.append(el('p','上次检查：'+new Date(p.last_polled_ms).toLocaleString(),'small muted'));
      if(p.next_poll_ms)details.append(el('p','下次检查：'+new Date(p.next_poll_ms).toLocaleString(),'small muted'));
      for(const job of p.failed_jobs||[]){const line=el('p',job.name+' · '+job.status+' · '+(job.failure_reason||''),'small prewrap');if(Number.isSafeInteger(job.id)&&job.id>0)line.append(document.createTextNode(' '),link('作业 #'+job.id,job.id,'jobs'));details.append(line);}
      info.append(details);row.append(identity,version,status,attempt,info);body.append(row);
    }
    scroll.append(table);root.append(scroll);
  }
  return {render,blocked};
})();
