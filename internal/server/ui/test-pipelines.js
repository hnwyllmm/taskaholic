'use strict';
window.WATestPipelines=(()=>{
  const names={QUEUED:'等待发起',SUBMITTING:'正在提交',UNCERTAIN:'提交待确认',RETRY_QUEUED:'等待快重试',RETRY_SUBMITTING:'正在重试失败作业',RETRY_UNCERTAIN:'重试待确认',ERROR:'未能发起',SUPERSEDED:'版本已变化',created:'已创建',pending:'排队中',preparing:'准备中',waiting_for_resource:'等待资源',running:'测试中',success:'通过',failed:'失败',canceled:'已取消',canceling:'正在取消',skipped:'已跳过',manual:'需要人工操作',scheduled:'已计划'};
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
      const status=el('td'),badge=el('span',names[p.state]||p.state,'badge');badge.dataset.state=p.state==='success'?'COMPLETED':['failed','ERROR','UNCERTAIN','RETRY_UNCERTAIN','manual','canceled','skipped'].includes(p.state)?'BLOCKED':'QUEUED';status.append(badge);
      const attempt=el('td',(p.attempt||1)+(p.quick_retries?' · 快重试 '+p.quick_retries:'')),info=el('td'),details=el('details');details.append(el('summary','查看详情'));
      details.append(el('p',p.reason,'prewrap'),el('p','申请：'+p.request_id,'small prewrap'),el('p','发起于 '+new Date(p.created_at_ms).toLocaleString(),'small muted'));
      if(p.config_sha)details.append(el('p','测试配置 master-pipeline @ '+p.config_sha+' · JOBS=all · RUN_PROFILE=1','small prewrap'));
      if(p.retry_of)details.append(el('p','重试自：'+p.retry_of,'small prewrap'));
      if(p.error)details.append(el('p',p.error,'small error'));
      if(p.evidence_error)details.append(el('p','失败日志采集正在自动重试：'+p.evidence_error,'small error'));
      const summary=p.failure_summary;
      if(summary&&('total_failures' in summary||'collection_complete' in summary||'collection_error' in summary)){
        const complete=summary.collection_complete===true;
        details.append(el('p','失败分类：共 '+(summary.total_failures||0)+' 个；mysqltest '+(summary.mysqltest_failures||0)+' 个，非 mysqltest '+(summary.non_mysqltest_failures||0)+' 个；'+(complete?'已完整采集':'采集不完整')+'。','small prewrap'));
        if((summary.mysqltest_failures||0)>0){const mysqlLogs=summary.mysqltest_details_known===true&&summary.mysqltest_details_complete===true?'逐作业日志已完整采集，等待 Agent 统计失败 case。':'逐作业日志尚未完整采集，Agent 不能据此请求重试。';details.append(el('p','mysqltest：'+mysqlLogs,'small '+(summary.mysqltest_details_known===true&&summary.mysqltest_details_complete===true?'muted':'error')));}
        if(summary.detail_truncated)details.append(el('p','非 mysqltest 作业详情可能已截取；失败统计仍来自完整采集。','small muted'));
        if(summary.collection_error)details.append(el('p','采集说明：'+summary.collection_error,'small error'));
      }
      if(p.failure_assessment){
        const relation={related:'与当前修改有关',unrelated:'与当前修改无关',inconclusive:'暂无法判断'}[p.failure_assessment.relation]||p.failure_assessment.relation;
        const decision={repair:'修复后复测',retry:'请求原 Pipeline 重试',hold:'保留说明，等待决定'}[p.failure_assessment.decision]||p.failure_assessment.decision||'已记录';
        details.append(el('p','开发 Agent 结论：'+relation+' · '+decision,'small prewrap'));
        if(Number.isInteger(p.failure_assessment.mysqltest_case_count))details.append(el('p','mysqltest 失败 case：'+p.failure_assessment.mysqltest_case_count+' 个（'+(p.failure_assessment.mysqltest_jobs||[]).length+' 个作业汇总）','small prewrap'));
        for(const job of p.failure_assessment.mysqltest_jobs||[]){const names=(job.failed_case_names||[]).join('、');details.append(el('p','mysqltest 作业 #'+job.job_id+' '+(job.job_name||'')+'：'+(job.failed_case_count||0)+' 个失败 case'+(names?'（'+names+'）':''),'small prewrap'));}
        details.append(el('p','原因：'+p.failure_assessment.reason,'small prewrap'));
        details.append(el('p','证据：'+p.failure_assessment.evidence,'small prewrap'));
      }else if(p.analysis_required)details.append(el('p','等待原开发 Agent 读取失败日志，汇总 mysqltest case 并判断修复、原地重试或保留说明。','small error'));
      if(p.failure_history?.length)details.append(el('p','已记录 '+p.failure_history.length+' 轮失败：'+p.failure_history.map(x=>'#'+x.retry+' '+(x.job_names||[]).join('、')).join('；'),'small prewrap'));
      if(resolve&&['SUBMITTING','UNCERTAIN'].includes(p.state)&&Date.now()-(p.submitted_at_ms||Date.now())>=120000){const button=el('button','已核实未创建，允许重试');button.type='button';button.onclick=()=>resolve(p);details.append(button);}
      if(p.poll_error)details.append(el('p','轮询需要检查：'+p.poll_error,'small error'));
      if(p.last_polled_ms)details.append(el('p','上次检查：'+new Date(p.last_polled_ms).toLocaleString(),'small muted'));
      if(p.next_poll_ms)details.append(el('p','下次检查：'+new Date(p.next_poll_ms).toLocaleString(),'small muted'));
      for(const job of p.failed_jobs||[]){const line=el('p',job.name+' · '+job.status+' · '+(job.failure_reason||''),'small prewrap');if(Number.isSafeInteger(job.id)&&job.id>0)line.append(document.createTextNode(' '),link('作业 #'+job.id,job.id,'jobs'));details.append(line);if(job.log_collected)details.append(el('p',job.log_excerpt?'系统已把脱敏日志摘要交给 Agent'+(job.log_truncated?'（已截取末尾）':''):'系统已检查该作业日志','small muted'));else if(job.log_collect_error)details.append(el('p','系统正在重试日志采集：'+job.log_collect_error,'small error'));}
      info.append(details);row.append(identity,version,status,attempt,info);body.append(row);
    }
    scroll.append(table);root.append(scroll);
  }
  function renderPublications(root,items=[],{resolve}={}){
    root.replaceChildren();root.hidden=!items.length;if(!items.length)return;
    root.append(el('h2','平台回写'),el('p','Manager 负责来源工单状态和里程碑回写；每个评审角色在 PR 上维护一条固定评论，评审通过不等于 GitHub Approve。发送异常会保留记录并核对，不重复刷评论。','small muted'));
    const table=el('table',null,'pipeline-table'),head=el('thead'),titles=el('tr'),body=el('tbody');
    for(const name of ['位置','版本','回写状态','详情'])titles.append(el('th',name));head.append(titles);table.append(head,body);
    const states={QUEUED:'等待回写',SUBMITTING:'正在回写',UNCERTAIN:'结果待核对',SYNCED:'已同步',BLOCKED:'需要处理',SKIPPED:'未执行'};
    for(const p of items){const row=el('tr'),where=el('td'),url=p.remote_url||p.url;let safe=false;try{const u=new URL(url);safe=u.protocol==='https:'&&['github.com','antmultica.alipay.com'].includes(u.hostname)&&!u.username&&!u.password&&!u.port;}catch{}
      const statusAction=!!p.desired_status,label=statusAction?'原工单状态 · 进行中':p.sticky?'PR 固定评审评论':'原工单进展';
      if(safe){const a=el('a',label);a.href=url;a.target='_blank';a.rel='noopener noreferrer';where.append(a);}else where.textContent='关联平台';
      const info=el('td'),details=el('details');details.append(el('summary',p.error||(statusAction?'目标状态：进行中；不会回退已评审或终态工单':'查看回写内容')));if(!statusAction)details.append(el('p',p.body,'prewrap'));
      if(resolve&&p.state==='UNCERTAIN'&&!p.remote_id&&!statusAction){const button=el('button','已核实未发布，重新核对并允许重试');button.type='button';button.onclick=()=>resolve(p);details.append(button);}info.append(details);
      row.append(where,el('td',(p.head_sha||'').slice(0,12)||'—'),el('td',states[p.state]||p.state),info);body.append(row);
    }
    const scroll=el('div',null,'pipeline-scroll');scroll.append(table);root.append(scroll);
  }
  return {render,blocked,renderPublications};
})();
