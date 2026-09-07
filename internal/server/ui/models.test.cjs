const {test}=require('node:test');
const assert=require('node:assert/strict');
const {catalogLoader,options,mount}=require('./models.js');

class Element {
  constructor(tag){this.tag=tag;this.children=[];this.attributes={};this.disabled=false;this.hidden=false;this._value=undefined;}
  append(...nodes){for(const node of nodes){node.parentElement=this;this.children.push(node);}}
  replaceChildren(...nodes){this.children=[];this.append(...nodes);}
  replaceWith(node){const parent=this.parentElement;parent.children[parent.children.indexOf(this)]=node;node.parentElement=parent;}
  setAttribute(key,value){this.attributes[key]=value;}
  get firstChild(){return this.textContent?{textContent:this.textContent}:this.children[0];}
  get value(){return this._value??(this.tag==='select'?this.children[0]?.value||'':'');}
  set value(value){this._value=value;}
  find(id){return this.id===id?this:this.children.map(n=>n.find(id)).find(Boolean);}
}
function setup(t,load,value='',settings={}){
  const old=global.document;global.document={createElement:tag=>new Element(tag)};
  t.after(()=>{global.document=old;});
  const root=new Element('form'),label=new Element('label'),input=new Element('input');
  label.textContent='模型';input.id='agent-model';input.value=value;root.append(label);label.append(input);
  const control=mount(input,load,settings);
  return{root,input,control,mode:root.find('agent-model-mode'),select:root.find('agent-model-select'),hint:root.find('agent-model-hint')};
}
const context=(id='dev',adapter='cursor-agent',state='ONLINE',supported=true)=>({runtimeID:id,adapterID:adapter,runtimes:[{runtime_id:id,state,capabilities:{adapters:{[adapter]:{model_catalog:supported}}}}],agents:[]});

test('Catalog preserves default, deduplicates and retains unknown/custom models',()=>{
  const list=options([{id:'auto',name:'Auto'},{id:'auto',name:'duplicate'}],['auto','previous'],'custom[effort=high]');
  assert.deepEqual(list.map(m=>m.id),['','auto','previous','custom[effort=high]']);
  assert.match(list.at(-1).name,/当前填写/);
});

test('Model list requests are coalesced and scoped to machine + adapter, failures allow manual input',async()=>{
  const calls=[];const load=catalogLoader(async path=>{calls.push(path);return{models:[{id:'auto'}],status:'ready'};});
  await Promise.all([load('dev','cursor-agent'),load('dev','cursor-agent')]);
  await load('mac','cursor-agent');await load('dev','other-agent');
  assert.equal(calls.length,3);assert.match(calls[0],/dev\/models\?adapter_id=cursor-agent/);
  const fail=catalogLoader(async()=>{throw Error('offline');});
  assert.deepEqual(await fail('dev','cursor-agent'),{status:'unavailable',models:[]});
});

test('Create/edit picker supports both modes without losing selected or custom values',async t=>{
  const {control,input,mode,select}=setup(t,async()=>({status:'ready',models:[{id:'auto',name:'Auto'},{id:'listed',name:'Listed'}]}),'retired-model');
  await control.update(context());
  assert.equal(select.value,'retired-model');assert.equal(input.hidden,true);
  select.value='listed';select.onchange();assert.equal(input.value,'listed');
  mode.value='manual';mode.onchange();assert.equal(input.hidden,false);assert.equal(select.hidden,true);assert.equal(input.value,'listed');
  input.value='custom[effort=high]';
  mode.value='list';mode.onchange();assert.equal(select.value,'custom[effort=high]');
  assert.ok(select.children.some(o=>o.value==='custom[effort=high]'));
  select.value='';select.onchange();assert.equal(input.value,'');
  control.setValue('pinned-model');control.setDisabled(true);
  assert.equal(select.value,'pinned-model');assert.equal(mode.disabled,true);assert.equal(select.disabled,true);assert.equal(input.disabled,true);
  control.setDisabled(false);assert.equal(mode.disabled,false);
});

test('Late catalog responses cannot replace another machine, or erase edits during loading',async t=>{
  let finishDev,finishMac;
  const {control,input,mode,select,hint}=setup(t,id=>new Promise(resolve=>{if(id==='dev')finishDev=resolve;else finishMac=resolve;}));
  const dev=control.update(context('dev')),mac=control.update(context('mac'));
  mode.value='manual';mode.onchange();input.value='my-model';
  finishMac({status:'ready',models:[{id:'mac-model'}]});await mac;
  finishDev({status:'ready',models:[{id:'dev-model'}]});await dev;
  assert.ok(select.children.some(o=>o.value==='mac-model'));assert.ok(!select.children.some(o=>o.value==='dev-model'));
  assert.equal(input.value,'my-model');assert.equal(mode.value,'manual');assert.equal(input.hidden,false);
  await control.update(context('dev','cursor-agent','OFFLINE'));assert.match(hint.textContent,/离线/);assert.equal(input.value,'my-model');
  await control.update(context('dev','other-agent','ONLINE',false));assert.match(hint.textContent,/暂不提供/);assert.equal(input.value,'my-model');
});

test('Unavailable catalogs retain configured models and keep manual entry available',async t=>{
  const {control,input,mode,select,hint}=setup(t,async()=>{throw Error('network error');});
  const config=context();config.agents=[{runtime_id:'dev',adapter_id:'cursor-agent',model_id:'configured'},{runtime_id:'elsewhere',adapter_id:'cursor-agent',model_id:'wrong-machine'}];
  await control.update(config);assert.match(hint.textContent,/暂不可用/);
  assert.ok(select.children.some(o=>o.value==='configured'));assert.ok(!select.children.some(o=>o.value==='wrong-machine'));
  mode.value='manual';mode.onchange();input.value='new-model';assert.equal(input.disabled,false);
});

test('Reasoning picker follows exact model capabilities and resets only on model/provider edits',async t=>{
  const {control,input,root,select}=setup(t,async()=>({status:'ready',models:[{id:'smart',reasoning_efforts:[{id:'low'},{id:'ultra'}],default_reasoning_effort:'low'},{id:'plain'}]}),'smart',{reasoning:true,reasoningEffort:'ultra'});
  const config=context();config.runtimes[0].capabilities.adapters['cursor-agent'].reasoning_effort=true;
  await control.update(config);
  const effort=root.find('agent-model-effort');
  assert.deepEqual(effort.children.map(o=>o.value),['','low','ultra']);assert.equal(effort.value,'ultra');
  effort.value='low';effort.onchange();assert.equal(control.getReasoningEffort(),'low');
  await control.update(config);assert.equal(control.getReasoningEffort(),'low');
  control.setDisabled(true);assert.equal(effort.disabled,true);control.setDisabled(false);assert.equal(effort.disabled,false);
  select.value='plain';select.onchange();assert.equal(effort.value,'');assert.equal(effort.disabled,true);
  input.value='custom[effort=high]';input.oninput();assert.equal(control.getReasoningEffort(),'');assert.equal(effort.children.length,1);
});

test('Offline or stale catalogs never erase a saved reasoning configuration',async t=>{
  let finish;
  const {control,root}=setup(t,()=>new Promise(resolve=>finish=resolve),'smart',{reasoning:true,reasoningEffort:'high'});
  const config=context();config.runtimes[0].capabilities.adapters['cursor-agent'].reasoning_effort=true;
  const pending=control.update(config);
  const effort=root.find('agent-model-effort');assert.equal(effort.value,'high');
  finish({status:'unavailable',models:[]});await pending;
  assert.equal(control.getReasoningEffort(),'high');assert.equal(effort.disabled,false);
  effort.value='';effort.onchange();assert.equal(control.getReasoningEffort(),'');
});
