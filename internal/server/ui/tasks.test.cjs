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
  const review={controls:noop,update:noop,reset:noop,isBusy:()=>false,isWaiting:()=>false,hasDraft:()=>false};
  const document={getElementById:$,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[],body:new Element('body')};
  const ctx=vm.createContext({document,fetch,console,URLSearchParams,Uint8Array,crypto:{getRandomValues:array=>array.fill(1)},location:{hash,search:''},history:{replaceState:noop},confirm:()=>true,setTimeout:()=>1,clearTimeout:noop,addEventListener:noop,WA:{token:()=>'',el:(tag,text,cls)=>{const n=new Element(tag);n.textContent=text;n.className=cls;return n;},date:()=>'',badge:state=>{const n=new Element('span');n.textContent=state;return n;},labels:{}},WAActivity:{mount:()=>activity},WAReviewChat:{mount:()=>review}});
  ctx.window=ctx;
  for(const file of ['assignment.js','task-hierarchy.js','test-pipelines.js','tasks.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,file),'utf8'),ctx,{filename:file});
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return{$,requests,ctx,settle,tasks,details,hierarchies};
}

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
