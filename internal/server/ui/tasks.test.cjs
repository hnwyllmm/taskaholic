const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

// Exercise the complete task page script as well as the small assignment
// widget. No real service, member or task is mutated by these UI tests.
async function page(){
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
  const requests=[],tasks=[],details={};
  const fetch=async(url,init={})=>{
    const route=url.replace('/api/v1','').split('?')[0],body=init.body?JSON.parse(init.body):null;
    requests.push({route,method:init.method||'GET',body});
    let response;
    if(route==='/work/tasks'&&init.method==='POST'){
      response={task_id:'task-'+tasks.length,title:body.title,goal:body.goal,requirements:body.requirements,state:body.defer_assignment?'NEW':'QUEUED',version:2};tasks.push(response);
      details[response.task_id]={detail:{task:response,runs:[],events:[]},work:{config:{task_version:2,preferred_agent_id:body.agent_id,project:{},paused:body.defer_assignment},messages:[],reviews:[],review_turns:[],artifacts:[]}};
    }else if(route.endsWith('/assignment')){
      const id=route.split('/')[3],data=details[id];data.detail.task.state='QUEUED';data.detail.task.version++;Object.assign(data.work.config,{paused:false,preferred_agent_id:body.agent_id,task_version:data.detail.task.version});response=data.detail.task;
    }else response={'/roles':{roles:[{role_id:'docs',name:'Docs',version:1}]},'/agents':{agents:[agent]},'/projects':{projects:[]},'/system':{eligible_agents:1,connected_runtimes:1,runtimes:[runtime]},'/work/tasks':{tasks}}[route]||details[route.split('/')[3]];
    if(!response)throw Error('unhandled API '+route);return{ok:true,json:async()=>structuredClone(response)};
  };
  const noop=()=>{},activity={controls:noop,update:noop,reconnect:noop,dispose:noop,isConnected:()=>true};
  const review={controls:noop,update:noop,reset:noop,isBusy:()=>false,isWaiting:()=>false,hasDraft:()=>false};
  const document={getElementById:$,createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text}),querySelectorAll:()=>[],body:new Element('body')};
  const ctx=vm.createContext({document,fetch,console,URLSearchParams,Uint8Array,crypto:{getRandomValues:array=>array.fill(1)},location:{hash:'',search:''},history:{replaceState:noop},confirm:()=>true,setTimeout:()=>1,clearTimeout:noop,addEventListener:noop,WA:{token:()=>'',el:(tag,text)=>{const n=new Element(tag);n.textContent=text;return n;},date:()=>'',badge:()=>new Element('span'),labels:{}},WAActivity:{mount:()=>activity},WAReviewChat:{mount:()=>review}});
  ctx.window=ctx;
  for(const file of ['assignment.js','tasks.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,file),'utf8'),ctx,{filename:file});
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return{$,requests,ctx,settle};
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
