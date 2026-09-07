const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

async function fixture(){
  class Element{
    constructor(tag='div'){this.tag=tag;this.children=[];this.value='';this.open=false;}
    append(...nodes){this.children.push(...nodes);}
    replaceChildren(...nodes){this.children=[...nodes];}
    showModal(){this.open=true;}close(){this.open=false;}reset(){}
    querySelectorAll(){return this.children.flatMap(x=>x.children).filter(n=>n.tag==='input'&&n.checked);}
  }
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8'),elements=new Map();
  for(const m of html.matchAll(/<(\w+)[^>]*\bid="([^"]+)"/g))elements.set(m[2],new Element(m[1]));
  elements.set('connect',new Element('button'));elements.set('connection',new Element());elements.set('token',new Element());
  const $=id=>{assert.ok(elements.has(id),'missing HTML element '+id);return elements.get(id);};
  const multica={source_id:'antmultica',kind:'antmultica',name:'My tasks',enabled:true,version:4,interval_seconds:60,config:{workspace_id:'workspace',workspace_slug:'seekdb',assignee_id:'me',iteration_key:'迭代',iteration_value:'1.5.0',role_id:'dev'}};
  const github={source_id:'github',kind:'github',name:'GitHub',enabled:true,version:2,interval_seconds:5,config:{reviewer_role_ids:['reviewer'],ignore_logins:[]}};
  const data={sources:[multica,github],targets:[],events:[],reviews:[]},calls=[],notices=[];
  const el=(tag,text,cls)=>Object.assign(new Element(tag),{textContent:text,className:cls});
  const api=async(route,method='GET',body)=>{
    calls.push({route,method,body});
    if(method!=='GET')return {};
    if(route==='/sources')return structuredClone(data);
    if(route==='/roles')return {roles:[{role_id:'dev',name:'Developer'},{role_id:'reviewer',name:'Reviewer'}]};
    if(route==='/projects')return {projects:[]};
    if(route==='/work/tasks')return {tasks:[{task_id:'owned',assigned_agent_id:'agent',title:'Started',state:'WAITING_REVIEW'},{task_id:'new',title:'Unowned',state:'NEW'}]};
    throw Error('unexpected route '+route);
  };
  const context=vm.createContext({document:{getElementById:$,querySelectorAll:()=>[],hidden:false},WA:{el,api,notice:(...n)=>notices.push(n),link:(text,href)=>Object.assign(el('a',text),{href}),date:String,key:()=> 'key',saveToken:()=>{}},setTimeout:()=>1,location:{search:'?task=owned'},URLSearchParams,console});
  context.window=context;context.addEventListener=()=>{};
  vm.runInContext(fs.readFileSync(path.join(__dirname,'sources.js'),'utf8'),context);
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return {$,calls,settle,notices};
}
test('Task sources editor preserves CAS and exact identity while changing iteration',async()=>{
  const {$,calls,settle}=await fixture();
  $('sources').children[1].children.at(-1).onclick();
  assert.equal($('iteration-value').value,'1.5.0');assert.equal($('assignee-id').readOnly,true);
  $('iteration-value').value='1.6.0';
  $('source-form').onsubmit({preventDefault(){}});await settle();
  const request=calls.find(c=>c.method==='PUT');
  assert.equal(request.route,'/sources/antmultica');
  assert.equal(request.body.expected_version,4);assert.equal(request.body.source.config.iteration_value,'1.6.0');
  assert.equal(request.body.source.config.assignee_id,'me');assert.equal(request.body.source.config.role_id,'dev');
});
test('PR registration offers only owned tasks and preserves the original task id',async()=>{
  const {$,calls,settle}=await fixture();$('register-pr').onclick();
  assert.deepEqual($('pr-task').children.map(n=>n.value),['','owned']);
  $('pr-url').value='https://github.com/o/r/pull/1';
  $('pr-form').onsubmit({preventDefault(){}});await settle();
  const request=calls.find(c=>c.method==='POST');
  assert.equal(request.route,'/work/tasks/owned/pull-requests');assert.equal(request.body.source_id,'github');
});
test('Sources UI does not render external input as HTML or collect credentials',()=>{
  const js=fs.readFileSync(path.join(__dirname,'sources.js'),'utf8');
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8');
  assert.ok(!js.includes('innerHTML'));
  assert.ok(!html.includes('type="password"'));
  assert.match(js,/expected_version:original.version/);
});
