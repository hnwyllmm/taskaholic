const {test}=require('node:test');
const assert=require('node:assert/strict');
const {canAssign,matchesFilter,candidates,mount}=require('./assignment.js');
const machine={runtime_id:'dev',state:'ONLINE',capabilities:{adapters:{'cursor-agent':{role_instructions:true,structured_output:true,read_only_runs:true}}}};
const member=(id,extra={})=>({agent_id:id,name:id,state:'ACTIVE',role_id:'writer',role:{name:'文档作者',capabilities:['document.write']},runtime_id:'dev',adapter_id:'cursor-agent',model_id:'auto',active_runs:0,max_concurrent:1,...extra});
const work=(version=2,state='NEW')=>({detail:{task:{task_id:'work',state,version,requirements:{role_id:'writer'}},runs:[],session:null},work:{config:{task_version:version,preferred_agent_id:''}}});

test('Saved and queued work can be assigned; past or current Sessions cannot be replaced',()=>{
  for(const state of ['NEW','QUEUED','PAUSED','BLOCKED'])assert.equal(canAssign(work(2,state)),true);
  for(const state of ['COMPLETED','WAITING_REVIEW','IN_PROGRESS'])assert.equal(canAssign(work(2,state)),false);
  const original=work();original.detail.session={session_id:'original'};assert.equal(canAssign(original),false);
  original.detail.session=null;original.detail.runs=[{state:'FAILED'}];assert.equal(canAssign(original),false);
  assert.equal(matchesFilter({state:'NEW'},'QUEUED'),true);assert.equal(matchesFilter({state:'IN_PROGRESS'},'QUEUED'),false);
});
test('Manual candidates keep busy/offline members but reject incompatible and disabled choices',()=>{
  const agents=[member('ready'),member('busy',{active_runs:1}),member('offline',{runtime_id:'other'}),member('wrong-role',{role_id:'security'}),member('disabled',{state:'DISABLED'}),member('excluded'),member('missing-capability',{role:{capabilities:[]}})];
  const values=candidates({role_id:'writer',capabilities:['document.write'],excluded_agent_ids:['excluded']},agents,[machine]);
  assert.deepEqual(values.map(v=>v.disabled),[false,false,false,true,true,true,true]);
  assert.match(values[1].label,/忙碌/);assert.match(values[2].label,/离线/);
});

class Element{
  constructor(tag){this.tag=tag;this.children=[];this.disabled=false;this.attributes={};}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=nodes;}
  setAttribute(key,value){this.attributes[key]=value;}
  all(tag){return this.children.flatMap(n=>[...(n.tag===tag?[n]:[]),...n.all(tag)]);}
}
test('Assignment UI submits manual or automatic choice and preserves edits through refresh',t=>{
  const old=global.document;global.document={createElement:tag=>new Element(tag)};t.after(()=>{global.document=old;});
  const root=new Element('section'),calls=[],control=mount(root,{submit:(...args)=>calls.push(args)}),data=work();
  control.update(data,[member('a'),member('b')],[machine]);
  const select=root.all('select')[0],form=root.all('form')[0],[send,reset]=root.all('button');
  assert.equal(select.value,'');form.onsubmit({preventDefault(){}});assert.deepEqual(calls.pop(),['',2]);
  select.value='b';select.onchange();control.update(data,[member('a'),member('b')],[machine]);
  assert.equal(select.value,'b');form.onsubmit({preventDefault(){}});assert.deepEqual(calls.pop(),['b',2]);
  control.update(work(3),[member('a'),member('b')],[machine]);
  assert.equal(select.value,'b');assert.equal(send.disabled,true);form.onsubmit({preventDefault(){}});assert.equal(calls.length,0);
  reset.onclick();assert.equal(select.value,'');assert.equal(send.disabled,false);form.onsubmit({preventDefault(){}});assert.deepEqual(calls.pop(),['',3]);
  control.controls(true);assert.equal(send.disabled,true);assert.equal(select.disabled,true);
  control.controls(false);data.detail.session={agent_id:'a'};control.update(data);assert.equal(form.hidden,true);assert.equal(send.disabled,true);
});
