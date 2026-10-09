const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

// Exercise the complete task page script as well as the small assignment
// widget. No real service, member or task is mutated by these UI tests.
async function page({initialTasks=[],hierarchies={},hash=''}={}){
  const elements=new Map();
  class Element{
    constructor(tag='div'){this.tag=tag;this.children=[];this.dataset={};this.value='';this.scrollTop=0;this.scrollHeight=0;this.clientHeight=1000;this.classList={add(){},remove(){},toggle(){}};}
    append(...nodes){this.children.push(...nodes);}
    replaceChildren(...nodes){this.children=nodes;}
    setAttribute(){} before(){} focus(){} remove(){} click(){this.onclick?.();}
    get options(){return this.children;}
    showModal(){this.open=true;}close(){this.open=false;}
    reset(){for(const id of ['new-title','new-goal','new-role','new-agent','new-capabilities','new-project'])$(id).value='';$('new-assignment-mode').value='later';}
  }
  const $=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
  $('new-assignment-mode').value='later';
  const agent={agent_id:'writer',name:'Writer',state:'ACTIVE',role_id:'docs',role:{name:'Docs',capabilities:['document.write']},runtime_id:'dev',adapter_id:'cursor-agent',model_id:'auto',active_runs:0,max_concurrent:1};
  const runtime={runtime_id:'dev',state:'ONLINE',capabilities:{adapters:{'cursor-agent':{role_instructions:true,structured_output:true,read_only_runs:true}}}};
  const requests=[],tasks=structuredClone(initialTasks),details={};
  for(const task of tasks)details[task.task_id]={detail:{task,runs:[],events:[]},work:{config:{task_version:task.version,project:{},paused:false},messages:[],reviews:[],review_turns:[],artifacts:[]}};
  const fetch=async(url,init={})=>{
    const route=url.replace('/api/v1','').split('?')[0],body=init.body?JSON.parse(init.body):null;
    requests.push({route,url,method:init.method||'GET',body});
    let response;
    if(route==='/work/tasks'&&init.method==='POST'){
      response={task_id:'task-'+tasks.length,title:body.title,goal:body.goal,requirements:body.requirements,state:body.defer_assignment?'NEW':'QUEUED',version:2};tasks.push(response);
      details[response.task_id]={detail:{task:response,runs:[],events:[]},work:{config:{task_version:2,preferred_agent_id:body.agent_id,project:{},paused:body.defer_assignment},messages:[],reviews:[],review_turns:[],artifacts:[]}};
    }else if(route.endsWith('/hierarchy')){
      const id=route.split('/')[3];response={task_id:id,task_version:details[id].detail.task.version,parents:[],roots:[],children:[],...hierarchies[id]};
    }else if(route==='/work/tasks'){
      response={tasks:url.includes('scope=roots')?tasks.filter(t=>!hierarchies[t.task_id]?.parents?.length):tasks};
    }else if(route.endsWith('/assignment')){
      const id=route.split('/')[3],data=details[id];data.detail.task.state='QUEUED';data.detail.task.version++;Object.assign(data.work.config,{paused:false,preferred_agent_id:body.agent_id,task_version:data.detail.task.version});response=data.detail.task;
    }else response={'/roles':{roles:[{role_id:'docs',name:'Docs',version:1}]},'/agents':{agents:[agent]},'/projects':{projects:[]},'/system':{eligible_agents:1,connected_runtimes:1,runtimes:[runtime]},'/work/tasks':{tasks}}[route]||details[route.split('/')[3]];
    if(!response)throw Error('unhandled API '+route);return{ok:true,json:async()=>structuredClone(response)};
  };
  const noop=()=>{},activity={controls:noop,update:noop,reconnect:noop,dispose:noop,isConnected:()=>true};
  const reviewControlCalls=[],review={controls:value=>reviewControlCalls.push(value),update:noop,reset:noop,isBusy:()=>false,isWaiting:()=>false,hasDraft:()=>false};
  const document={getElementById:$,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[],body:new Element('body')};
  const consultation={open:noop,update:async()=>{},reset:noop};
  const taskPermissions={update:async()=>{},dispose:noop};
  const ctx=vm.createContext({document,fetch,console,URLSearchParams,Uint8Array,crypto:{getRandomValues:array=>array.fill(1)},location:{hash,search:''},history:{replaceState:noop},confirm:()=>true,setTimeout:()=>1,clearTimeout:noop,addEventListener:noop,WA:{token:()=>'',el:(tag,text,cls)=>{const n=new Element(tag);n.textContent=text;n.className=cls;return n;},date:()=>'',badge:state=>{const n=new Element('span');n.textContent=state;return n;},labels:{}},WAActivity:{mount:()=>activity},WAReviewChat:{mount:()=>review},WATaskConsultation:{mount:()=>consultation},WAPermissions:{mountTask:()=>taskPermissions}});
  ctx.window=ctx;
  for(const file of ['assignment.js','task-hierarchy.js','test-pipelines.js','task-references.js','tasks.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,file),'utf8'),ctx,{filename:file});
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return{$,requests,ctx,settle,tasks,details,hierarchies,reviewControlCalls};
}

test('Background task refresh does not disable the delivery Agent chat input',async()=>{
 const task={task_id:'delivery',title:'Delivery review',goal:'Discuss the result',state:'WAITING_REVIEW',version:3};
 const p=await page({initialTasks:[task]});
 p.details.delivery.work.reviews=[{review_id:'review',kind:'delivery',state:'PENDING',artifact_ids:[],discussion_version:0}];
 await vm.runInContext('state.loading=true; selectTask("delivery")',p.ctx);await p.settle();
 assert.equal(p.reviewControlCalls.at(-1),false,'passive loading must not disable and blur the active textarea');
 await vm.runInContext('state.loading=false; controls()',p.ctx);
});

test('Failed quota run has a separate retry without new guidance',async()=>{
 const task={task_id:'quota',title:'Quota',goal:'Implement',state:'BLOCKED',version:7};
 const p=await page({initialTasks:[task]});
 p.details.quota.detail.runs=[{run_id:'failed',state:'FAILED',created_at_ms:1,error:'usage limit'}];
 p.details.quota.work.development={phase:'IMPLEMENTING'};
 await vm.runInContext('selectTask("quota")',p.ctx);await p.settle();
 assert.equal(p.$('retry-work').hidden,false);
 p.$('retry-work').onclick();await p.settle();
 const req=p.requests.find(r=>r.route.endsWith('/retry'));
 assert.equal(req.body.expected_version,7);
 assert.equal(p.requests.some(r=>r.method==='POST'&&r.route.endsWith('/messages')),false);
 p.details.quota.detail.task.state='WAITING_AUTHORIZATION';
 await vm.runInContext('selectTask("quota")',p.ctx);
 assert.equal(p.$('retry-work').hidden,true);
});

test('A completed manager-blocked run always offers a generic recovery action',async()=>{
 const task={task_id:'manager-block',title:'Rejected result',goal:'Continue safely',state:'BLOCKED',version:11};
 const p=await page({initialTasks:[task]});
 p.details[task.task_id].detail.runs=[{run_id:'completed',state:'COMPLETED',created_at_ms:2,output:JSON.stringify({outcome:'replan',message:'complete revised plan'})}];
 p.details[task.task_id].work.config.scheduler_error='系统状态冲突';
 p.details[task.task_id].work.development={phase:'PLANNING'};
 await vm.runInContext('selectTask("manager-block")',p.ctx);await p.settle();
 assert.equal(p.$('retry-work').hidden,false);
 p.$('retry-work').onclick();await p.settle();
 const req=p.requests.find(r=>r.route.endsWith('/retry'));
 assert.equal(req.body.expected_version,11);
});

test('A completed Run is labeled as an ended round, not a completed task',async()=>{
 const task={task_id:'waiting',title:'Waiting for evidence',goal:'Continue after logs',state:'WAITING_INPUT',version:8};
 const p=await page({initialTasks:[task]});
 p.details.waiting.detail.runs=[{run_id:'ended',state:'COMPLETED',created_at_ms:1,adapter_id:'codex-agent',model_id:'gpt'}];
 await vm.runInContext('selectTask("waiting")',p.ctx);await p.settle();
 assert.equal(p.$('status').textContent,'待我回复');
 assert.match(textTree(p.$('runs')),/本轮已结束/);
 assert.doesNotMatch(textTree(p.$('runs')),/\nCOMPLETED/);
 assert.match(fs.readFileSync(path.join(__dirname,'activity.js'),'utf8'),/COMPLETED:'本轮已结束'/);
});

test('Running and completed tasks show lifecycle elapsed time',async()=>{
 const now=Date.now(),running={task_id:'running',title:'Running task',goal:'Work',state:'IN_PROGRESS',version:2,updated_at_ms:now,first_run_at_ms:now-90*60000};
 const waiting={task_id:'waiting-review',title:'Waiting review task',goal:'Review',state:'WAITING_REVIEW',version:2,updated_at_ms:now,first_run_at_ms:now-2*3600000};
 const blocked={task_id:'blocked',title:'Blocked task',goal:'Recover',state:'BLOCKED',version:2,updated_at_ms:now,first_run_at_ms:now-3*3600000};
 const completed={task_id:'completed',title:'Completed task',goal:'Done',state:'COMPLETED',version:3,updated_at_ms:now-30*60000,first_run_at_ms:now-4*3600000,completed_at_ms:now-30*60000};
 const fresh={task_id:'fresh',title:'Fresh task',goal:'Wait',state:'NEW',version:1,updated_at_ms:now};
 const p=await page({initialTasks:[running,waiting,blocked,completed,fresh]});
 assert.match(textTree(p.$('task-list')),/Running task[\s\S]*已运行[\s\S]*Waiting review task[\s\S]*已运行[\s\S]*Blocked task[\s\S]*已运行[\s\S]*Completed task[\s\S]*运行了/);
 assert.doesNotMatch(textTree(p.$('task-list').children[4]),/已运行|运行了/);
 await vm.runInContext("selectTask('running')",p.ctx);await p.settle();
 assert.equal(p.$('task-elapsed').hidden,false);assert.match(p.$('task-elapsed').textContent,/^已运行 /);
 await vm.runInContext("selectTask('waiting-review')",p.ctx);await p.settle();
 assert.equal(p.$('task-elapsed').hidden,false);assert.match(p.$('task-elapsed').textContent,/^已运行 /);
 await vm.runInContext("selectTask('completed')",p.ctx);await p.settle();
 assert.equal(p.$('task-elapsed').hidden,false);assert.match(p.$('task-elapsed').textContent,/^运行了 /);
});

test('Waiting input shows the exact Agent question even after a later source message',async()=>{
 const task={task_id:'input',title:'Needs an answer',goal:'Ship it',state:'WAITING_INPUT',version:9};
 const p=await page({initialTasks:[task]});
 p.details.input.work.messages=[
  {speaker:'assistant',content:'请确认是否允许修改 PR 标题。',created_at_ms:1,delivery:'RECORDED'},
  {speaker:'source',content:'PR 已合并。',created_at_ms:2,delivery:'RECORDED'}
 ];
 await vm.runInContext('selectTask("input")',p.ctx);await p.settle();
 assert.equal(p.$('pending-input').hidden,false);
 assert.match(textTree(p.$('pending-input-question')),/请确认是否允许修改 PR 标题/);
 assert.doesNotMatch(textTree(p.$('pending-input-question')),/PR 已合并/);
 assert.equal(typeof p.$('pending-input-reply').onclick,'function');
 p.details.input.detail.task.state='COMPLETED';
 await vm.runInContext('selectTask("input")',p.ctx);await p.settle();
 assert.equal(p.$('pending-input').hidden,true);
});

test('Interrupted paused run offers a dedicated continue action',async()=>{
 const task={task_id:'interrupted',title:'Interrupted',goal:'Continue implementation',state:'PAUSED',version:11};
 const p=await page({initialTasks:[task]});
 p.details.interrupted.detail.runs=[{run_id:'stopped',state:'INTERRUPTED',created_at_ms:2,error:'context canceled'}];
 p.details.interrupted.work.development={phase:'IMPLEMENTING'};
 await vm.runInContext('selectTask("interrupted")',p.ctx);await p.settle();
 assert.equal(p.$('retry-work').hidden,false);
 assert.equal(p.$('retry-work').textContent,'恢复执行');
 p.$('retry-work').onclick();await p.settle();
 const req=p.requests.find(r=>r.route.endsWith('/retry'));
 assert.equal(req.body.expected_version,11);
 assert.equal(p.requests.some(r=>r.method==='POST'&&r.route.endsWith('/messages')),false);
});

test('An explicit pause remains resumable after the latest run completed',async()=>{
 const task={task_id:'manual-pause',title:'Paused after a round',goal:'Continue implementation',state:'PAUSED',version:55};
 const p=await page({initialTasks:[task]});
 p.details[task.task_id].detail.runs=[{run_id:'completed',state:'COMPLETED',created_at_ms:2}];
 p.details[task.task_id].work.config.paused=true;
 p.details[task.task_id].work.development={phase:'IMPLEMENTING'};
 await vm.runInContext('selectTask("manual-pause")',p.ctx);await p.settle();
 assert.equal(p.$('retry-work').hidden,false);
 assert.equal(p.$('retry-work').textContent,'恢复执行');
 p.$('retry-work').onclick();await p.settle();
 const req=p.requests.find(r=>r.route.endsWith('/retry'));
 assert.equal(req.body.expected_version,55);
 assert.match(p.$('notice').textContent,/已恢复调度/);
});

test('Implementation messages default to execution direction and require an explicit plan-change choice',async()=>{
 const task={task_id:'implementing',title:'Implementation',goal:'Ship the approved plan',state:'IN_PROGRESS',version:12};
 const p=await page({initialTasks:[task]});
 p.details.implementing.detail.runs=[{run_id:'active',state:'RUNNING',created_at_ms:3}];
 p.details.implementing.work.development={phase:'IMPLEMENTING',version:4,repository:'oceanbase/seekdb',base_branch:'master',plan_hash:'a'.repeat(64),approved_review_id:'review'};
 await vm.runInContext('selectTask("implementing")',p.ctx);await p.settle();
 assert.equal(p.$('message-mode-label').hidden,false);
 assert.equal(p.$('message-mode').value,'execution_direction');
 p.$('message').value='Stop optional tests and prepare the PR';p.$('send').onclick();await p.settle();
 let request=p.requests.find(r=>r.method==='POST'&&r.route.endsWith('/messages'));
 assert.equal(request.body.mode,'execution_direction');assert.equal(request.body.interrupt,false);
 p.$('message-mode').value='plan_change';p.$('message-mode').onchange();p.$('message').value='Change the approved scope';p.$('steer').onclick();await p.settle();
 request=p.requests.filter(r=>r.method==='POST'&&r.route.endsWith('/messages')).at(-1);
 assert.equal(request.body.mode,'plan_change');assert.equal(request.body.interrupt,true);
 assert.match(p.$('message-mode-hint').textContent,/重新进入方案评审/);
});

test('Plan approval is explicit and different from final delivery acceptance',async()=>{
 const task={task_id:'plan-task',title:'Plan',goal:'Implement',state:'WAITING_REVIEW',version:5};
 const p=await page({initialTasks:[task]});
 p.details[task.task_id].work.development={phase:'HUMAN_REVIEW',version:2,repository:'oceanbase/seekdb',base_branch:'master',plan_hash:'a'.repeat(64),reviewer_task_id:'reviewer',validation_plan:{reuse:[{scenario:'parser',reason:'unchanged',evidence:'baseline abc'}],rerun:[{scenario:'persistence',reason:'changed',evidence:'existing regression'}],add:[],exclude:[],final_gate:['build candidate','run persistence regression']}};
 p.details[task.task_id].work.artifacts=[{artifact_id:'final-plan',run_id:'plan-run',name:'implementation-plan-v2.md',version:1,content:'# Final plan',sha256:'abc',created_at_ms:2}];
 p.details[task.task_id].work.reviews=[{review_id:'plan-review',kind:'plan',state:'PENDING',artifact_ids:['final-plan'],discussion_version:0}];
 p.details[task.task_id].work.test_pipelines=[{request_id:'old-pipeline',pr_target_id:'pr',state:'failed',current:true,attempt:1}];
 await vm.runInContext('selectTask("plan-task")',p.ctx);await p.settle();
 assert.equal(p.$('approve').textContent,'批准方案，开始开发');
 assert.equal(p.$('review-heading').textContent,'方案待你确认');
 assert.equal(p.$('review').hidden,false);
 assert.equal(p.$('pending-review').hidden,false);
 assert.equal(p.$('pending-review-heading').textContent,'方案待你确认');
 assert.match(p.$('pending-review-summary').textContent,/implementation-plan-v2\.md.*批准前不会开始开发/);
 assert.equal(p.$('pending-review-open').textContent,'查看方案并决定');
 assert.equal(p.$('approve').disabled,false,'implementation tests cannot block approval of the plan that precedes implementation');
 assert.equal(p.$('approve').title,'');
 p.$('pending-review-open').onclick();
 assert.equal(p.$('artifact').value,'final-plan');
 assert.match(textTree(p.$('development-phase')),/验证策略.*复用已有证据.*parser.*受影响，必须重跑.*persistence.*稳定候选最终门禁.*build candidate/);
 p.$('approve').onclick();await p.settle();
 const request=p.requests.find(x=>x.method==='POST'&&x.route.endsWith('/reviews/plan-review'));
 assert.equal(request.body.decision,'PLAN_APPROVED');
 assert.equal(p.$('notice').textContent,'方案已批准，原 Agent 将开始隔离开发。');
});

test('Failed current-version tests block final delivery approval but not changes requested',async()=>{
 const task={task_id:'delivery-gate',title:'Delivery',goal:'Ship tested code',state:'WAITING_REVIEW',version:8};
 const p=await page({initialTasks:[task]});
 p.details[task.task_id].work.artifacts=[{artifact_id:'delivery',run_id:'delivery-run',name:'delivery.md',version:1,content:'# Delivery',sha256:'abc',created_at_ms:2}];
 p.details[task.task_id].work.reviews=[{review_id:'delivery-review',kind:'delivery',state:'PENDING',artifact_ids:['delivery'],discussion_version:0}];
 p.details[task.task_id].work.test_pipelines=[{request_id:'current-pipeline',pr_target_id:'pr',state:'failed',current:true,attempt:1}];
 await vm.runInContext('selectTask("delivery-gate")',p.ctx);await p.settle();
 assert.equal(p.$('approve').disabled,true);
 assert.match(p.$('approve').title,/QA 回归尚未通过/);
 assert.equal(p.$('reject').disabled,false);
});

test('A major implementation replan highlights its reason, changes and evidence',async()=>{
 const task={task_id:'replan-task',title:'Replan',goal:'Implement',state:'IN_PROGRESS',version:9};
 const p=await page({initialTasks:[task]});
 p.details[task.task_id].work.development={phase:'IMPLEMENTING',version:3,repository:'oceanbase/seekdb',base_branch:'master'};
 p.details[task.task_id].detail.runs=[{run_id:'old',created_at_ms:1,output:'{}'},{run_id:'replan',created_at_ms:2,finished_at_ms:3,output:JSON.stringify({outcome:'replan',message:'fallback',task_update:{reason:'Unicode watcher failed',approach:'Patch the file boundary',analysis:'Wide file read passed',validation:'ASCII passed; Unicode failed'}})}];
 await vm.runInContext("selectTask('replan-task')",p.ctx);await p.settle();
 assert.equal(p.$('plan-change').hidden,false);
 assert.match(textTree(p.$('plan-change')),/为什么要改.*Unicode watcher failed/);
 assert.match(textTree(p.$('plan-change')),/方案改了什么.*Patch the file boundary/);
 assert.match(textTree(p.$('plan-change')),/Wide file read passed[\s\S]*ASCII passed; Unicode failed/);
});

test('Task page defaults to save first, then submits manual and automatic assignments',async()=>{
  const {$,requests,ctx,settle}=await page();
  $('new').onclick();assert.equal($('new-role').value,'');assert.equal($('new-agent').disabled,true);
  $('new-title').value='Test';$('new-goal').value='Write a guide';
  $('create-form').onsubmit({preventDefault(){}});await settle();
  const create=requests.find(r=>r.method==='POST');assert.equal(create.body.defer_assignment,true);assert.equal(create.body.agent_id,'');
  assert.equal($('status').textContent,'待分派');assert.equal($('send').textContent,'保存补充要求');
  assert.match($('notice').textContent,/执行安排/);
  const form=$('assignment-panel').children.find(n=>n.tag==='form'),select=form.children[0].children[0];
  select.value='writer';select.onchange();form.onsubmit({preventDefault(){}});await settle();
  assert.equal(requests.find(r=>r.route.endsWith('/assignment')).body.agent_id,'writer');
  assert.equal($('status').textContent,'排队中');assert.match($('owner').textContent,/Writer/);
  select.value='';select.onchange();form.onsubmit({preventDefault(){}});await settle();
  const updates=requests.filter(r=>r.route.endsWith('/assignment'));
  assert.equal(updates.length,2);assert.equal(updates[1].body.agent_id,'');assert.equal(updates[1].body.expected_version,3);
  assert.equal(vm.runInContext('state.busy',ctx),false);
});

test('Task creation retains direct auto and explicit-member shortcuts',async()=>{
  for(const mode of ['auto','member']){
    const {$,requests,settle}=await page();$('new').onclick();$('new-assignment-mode').value=mode;$('new-assignment-mode').onchange();
    assert.equal($('new-agent').required,mode==='member');assert.equal($('new-agent').disabled,mode!=='member');
    $('new-title').value='Test';$('new-goal').value='Write';$('new-agent').value='writer';
    $('create-form').onsubmit({preventDefault(){}});await settle();
    const create=requests.find(r=>r.method==='POST');assert.equal(create.body.defer_assignment,false);assert.equal(create.body.agent_id,mode==='member'?'writer':'');
    assert.equal($('status').textContent,'排队中');
  }
});

function hierarchyFixture(){
  const task=(id,title,state)=>({task_id:id,title,goal:'goal',state,version:1,requirements:{},updated_at_ms:1});
  const root=task('root','Original requirement','WAITING_SUBTASKS'),child=task('child','Code review','BLOCKED'),old=task('old','Old review','BLOCKED'),grand=task('grand','Nested test','WAITING_INPUT'),other=task('other','Other work','NEW');
  root.subtasks={total:2,completed:0,needs_attention:2,blocked:1,waiting_input:1,superseded:1};
  child.subtasks={total:1,completed:0,needs_attention:1,waiting_input:1};
  old.source_review_state='SUPERSEDED';
  const rootLink={task_id:root.task_id,title:root.title},childLink={task_id:child.task_id,title:child.title};
  return{initialTasks:[root,child,old,grand,other],hierarchies:{
    root:{roots:[],parents:[],children:[child,old]},
    child:{roots:[rootLink],parents:[rootLink],children:[grand]},
    old:{roots:[rootLink],parents:[rootLink],children:[]},
    grand:{roots:[rootLink],parents:[childLink],children:[]}
  }};
}
const textTree=n=>[n.textContent??'',...(n.children||[]).map(textTree)].join(' ');
const descendants=(node,cls)=>node.children.flatMap(n=>[...(n.className===cls?[n]:[]),...descendants(n,cls)]);

test('Task references show direct links and a compact legacy review without erasing history',async()=>{
  const p=await page(hierarchyFixture()),{$,ctx,details}=p;
  const legacy='LEGACY_PATCH_PAYLOAD @@ -1 +1 @@',sha='a'.repeat(40);
  details.child.detail.task.goal=legacy;
  Object.assign(details.child.work,{
    review_brief:{title:'PR #123 评审 · QA',goal:'请通过 PR 链接读取固定版本 '+sha},
    references:[
      {kind:'github.pr',label:'oceanbase/seekdb #123',url:'https://github.com/oceanbase/seekdb/pull/123',revision:sha},
      {kind:'antmultica.issue',label:'原需求 · SEEK-1',url:'https://antmultica.alipay.com/seekdb/issues/one'},
      {kind:'github.pr',label:'Untrusted',url:'javascript:alert(1)'}
    ],
    messages:[{speaker:'user',content:legacy,created_at_ms:1,run_id:'historical-run'},{speaker:'assistant',content:'Reviewer finding: check file.go:10',created_at_ms:2}]
  });
  await vm.runInContext("selectTask('child')",ctx);
  assert.equal($('title').textContent,'PR #123 评审 · QA');
  assert.ok(!$('goal').textContent.includes('LEGACY_PATCH_PAYLOAD'));
  assert.equal($('task-references').hidden,false);
  assert.equal($('task-references').children.length,2);
  const link=$('task-references').children[0].children[0];
  assert.equal(link.tag,'a');assert.equal(link.href,'https://github.com/oceanbase/seekdb/pull/123');assert.equal(link.rel,'noopener noreferrer');
  assert.match(textTree($('task-references')),new RegExp(sha));
  assert.ok(!textTree($('messages')).includes('LEGACY_PATCH_PAYLOAD'));
  assert.match(textTree($('messages')),/Reviewer finding/);
  assert.equal(details.child.detail.task.goal,legacy);assert.equal(details.child.work.messages[0].content,legacy);
  details.child.work.messages.push({speaker:'user',content:legacy,created_at_ms:3});
  await vm.runInContext("selectTask('child')",ctx);
  assert.ok(textTree($('messages')).includes('LEGACY_PATCH_PAYLOAD'),'a later human message must remain unchanged');
  await vm.runInContext("selectTask('other')",ctx);
  assert.equal($('task-references').hidden,true);assert.equal($('goal').textContent,'goal');
  const html=fs.readFileSync(path.join(__dirname,'tasks.html'),'utf8');
  assert.ok(html.indexOf('/assets/task-references.js')<html.indexOf('/tasks/tasks.js'));
  assert.ok(!fs.readFileSync(path.join(__dirname,'task-references.js'),'utf8').includes('innerHTML'));
});

test('Work overview and sidebar contain roots only; attention counts the original task once',async()=>{
  const {$,requests,ctx,settle}=await page(hierarchyFixture());
  assert.ok(requests.some(r=>r.url==='/api/v1/work/tasks?scope=roots'));
  assert.equal($('task-list').children.length,2);assert.equal($('work-table').children.length,2);
  assert.equal($('work-counts').children[2].children[0].textContent,1);
  assert.match(textTree($('work-table')),/子任务 0\/2 已完成/);assert.ok(!textTree($('work-table')).includes('Code review'));
  $('filter').value='BLOCKED';vm.runInContext('renderList();renderWorkOverview()',ctx);await settle();
  assert.equal($('work-table').children.length,1);assert.match(textTree($('work-table')),/Original requirement/);
  $('filter').value='ATTENTION';vm.runInContext('renderList();renderWorkOverview()',ctx);
  assert.equal($('work-table').children.length,1);
});

test('Root detail holds current and historical subtasks, and nested tasks link back to the original',async()=>{
  const {$,ctx,settle}=await page({...hierarchyFixture(),hash:'#root'});
  assert.equal($('task-hierarchy').hidden,false);assert.equal($('task-parents').hidden,true);
  assert.match(textTree($('task-hierarchy')),/Code review/);
  const archive=$('task-hierarchy').children.find(n=>n.tag==='details');
  assert.equal(archive.open,false);assert.match(textTree(archive),/已被新版本替代/);
  descendants($('task-hierarchy'),'subtask-row')[0].onclick();await settle();
  assert.equal(vm.runInContext('state.id',ctx),'child');
  assert.match(textTree($('task-parents')),/原始任务 · Original requirement/);
  assert.match(textTree($('task-hierarchy')),/Nested test/);
  descendants($('task-hierarchy'),'subtask-row')[0].onclick();await settle();
  assert.equal(vm.runInContext('state.id',ctx),'grand');
  assert.match(textTree($('task-parents')),/原始任务 · Original requirement/);
  assert.match(textTree($('task-parents')),/上一级 · Code review/);
  $('task-parents').children[0].onclick();await settle();assert.equal(vm.runInContext('state.id',ctx),'root');
  assert.equal($('task-list').children.length,2);
});

test('Existing child deep links and unsent conversation survive hierarchy navigation',async()=>{
  const {$,ctx,settle}=await page({...hierarchyFixture(),hash:'#child'});
  assert.equal(vm.runInContext('state.id',ctx),'child');
  $('message').value='Unsent direction';$('review-comment').value='Unsent review';ctx.confirm=()=>false;
  $('task-parents').children[0].onclick();await settle();
  assert.equal(vm.runInContext('state.id',ctx),'child');assert.equal($('message').value,'Unsent direction');assert.equal($('review-comment').value,'Unsent review');
  ctx.confirm=()=>true;$('task-parents').children[0].onclick();await settle();
  assert.equal(vm.runInContext('state.id',ctx),'root');assert.equal($('message').value,'');assert.equal($('review-comment').value,'');
});

test('Child-only state changes refresh even while the original task version stays unchanged',async()=>{
  const data=hierarchyFixture(),{$,ctx,settle,requests}=await page({...data,hash:'#root'});
  const before=requests.filter(r=>r.route==='/work/tasks/root').length;
  data.hierarchies.root.children[0].state='COMPLETED';
  const archive=$('task-hierarchy').children.find(n=>n.tag==='details');archive.open=true;archive.ontoggle();
  await vm.runInContext('poll()',ctx);await settle();
  assert.match(textTree(descendants($('task-hierarchy'),'subtask-row')[0]),/COMPLETED/);
  assert.equal($('task-hierarchy').children.find(n=>n.tag==='details').open,true);
  assert.equal(requests.filter(r=>r.route==='/work/tasks/root').length,before,'do not reload the whole conversation just to update child status');
});

test('Hierarchy rendering treats titles as text and retains queue categorization',async()=>{
  const {ctx}=await page();
  assert.equal(vm.runInContext("WATaskHierarchy.bucket({state:'NEW'})",ctx),'queued');
  assert.equal(vm.runInContext("WATaskHierarchy.bucket({state:'QUEUED'})",ctx),'queued');
  assert.equal(vm.runInContext("WATaskHierarchy.bucket({state:'IN_PROGRESS'})",ctx),'active');
  assert.equal(vm.runInContext("WATaskHierarchy.bucket({state:'WAITING_SUBTASKS',subtasks:{needs_attention:2}})",ctx),'attention');
  assert.equal(vm.runInContext("WATaskHierarchy.bucket({state:'COMPLETED'})",ctx),'completed');
  assert.ok(!fs.readFileSync(path.join(__dirname,'task-hierarchy.js'),'utf8').includes('innerHTML'));
});

test('CI waiting belongs to the root task and never changes reviewer presentation',async()=>{
  const task=(id,title)=>({task_id:id,title,goal:'review the pinned PR',state:'COMPLETED',source_review_state:'COMPLETED',version:2,requirements:{},updated_at_ms:2});
  const root={task_id:'ci-root',title:'SEEK-581',goal:'fix the issue',state:'WAITING_TESTS',version:4,requirements:{},updated_at_ms:1,subtasks:{total:3,completed:3,waiting_tests:0,needs_attention:0}};
  const children=[task('arch','Architecture review'),task('qa','QA review'),task('code','Code review')];
  const rootLink={task_id:root.task_id,title:root.title};
  const hierarchies={'ci-root':{roots:[],parents:[],children}};
  for(const child of children)hierarchies[child.task_id]={roots:[rootLink],parents:[rootLink],children:[]};
  const {$,ctx}=await page({initialTasks:[root,...children],hierarchies,hash:'#ci-root'});
  assert.equal($('status').textContent,'等待 CI');
  assert.equal(vm.runInContext("WATaskHierarchy.displayState(state.tasks[0])",ctx),'WAITING_TESTS');
  assert.equal(vm.runInContext("WATaskHierarchy.bucket(state.tasks[0])",ctx),'active');
  assert.match(textTree($('task-list')),/等待 CI/);
  assert.doesNotMatch(textTree($('work-table')),/子任务等待自身 CI/);
  const statuses=descendants($('task-hierarchy'),'subtask-status');
  assert.equal(statuses.length,3);
  assert.equal(statuses.every(item=>textTree(item).includes('COMPLETED')),true);
  assert.equal(vm.runInContext("WATaskHierarchy.displayState({state:'QUEUED',source_review_state:'WAITING_TESTS'})",ctx),'QUEUED','legacy reviewer metadata must not make CI its UI state');
  $('filter').value='WAITING_TESTS';vm.runInContext('renderList();renderWorkOverview()',ctx);
  assert.equal($('work-table').children.length,1);
});
