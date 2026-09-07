const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

async function page({pinned=false}={}){
  const elements=new Map();
  class Element{
    constructor(tag='div'){this.tag=tag;this.children=[];this.dataset={};this.listeners={};this._value='';}
    append(...nodes){for(const n of nodes){n.parentElement=this;this.children.push(n);}}
    replaceChildren(...nodes){this.children=[];this.append(...nodes);}
    prepend(...nodes){this.children.unshift(...nodes);}
    addEventListener(name,fn){this.listeners[name]=fn;}
    setAttribute(){} after(){} focus(){} matches(){return false;}
    closest(){return new Element('details');}querySelector(){return new Element('summary');}
    get options(){return this.children;}
    get value(){return this.tag==='select'&&!this.children.some(o=>o.value===this._value)?'':this._value;}
    set value(value){this._value=value;}
  }
  const html=fs.readFileSync(path.join(__dirname,'index.html'),'utf8');
  for(const m of html.matchAll(/<(\w+)[^>]*\bid="([^"]+)"/g))elements.set(m[2],new Element(m[1]));
  const $=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id);};
  $('agent-capacity').value='1';
  const runtime={runtime_id:'dev',state:'ONLINE',capabilities:{adapters:{'cursor-agent':{role_instructions:true,model_catalog:true},'codex-agent':{role_instructions:true,model_catalog:true}}}};
  const role={role_id:'role-docs',name:'Docs',capabilities:['document.write'],version:1};
  const session={runtime_id:'dev',adapter_id:'retired-plugin',model_id:'pinned-model'};
  const draft={draft_id:'draft_test',task_id:'task-test',state:'DRAFT',spec:role,messages:[]};
  const requests=[],modelUpdates=[];
  const fetch=async(url,init={})=>{
    const route=url.replace('/api/v1',''),body=init.body?JSON.parse(init.body):undefined;requests.push({route,body,method:init.method||'GET'});
    const response={'/roles':{roles:[role]},'/role-drafts':{drafts:[]},'/agents':{agents:[]},'/runtimes':{runtimes:[runtime]},'/roles/role-docs':role,'/role-drafts/draft_test':draft,'/tasks/task-test':{session}}[route];
    if(!response)throw Error('unexpected API '+route);return{ok:true,json:async()=>structuredClone(response)};
  };
  const noop=()=>{},context=vm.createContext({console,document:{getElementById:$,createElement:tag=>new Element(tag),querySelectorAll:()=>[]},fetch,history:{replaceState:noop},location:{hash:pinned?'#draft_test':'#role-docs'},setTimeout:()=>1,clearTimeout:noop,Uint8Array,crypto:{getRandomValues:a=>a.fill(1)},WA:{token:()=>''},WAModels:{catalogLoader:()=>noop,mount:input=>({update:config=>{modelUpdates.push({id:input.id,...config});return Promise.resolve();},setDisabled:noop,setValue:value=>{input.value=value;},getReasoningEffort:()=>''})},addEventListener:noop});
  context.window=context;
  for(const file of ['adapters.js','app.js'])vm.runInContext(fs.readFileSync(path.join(__dirname,file),'utf8'),context,{filename:file});
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return{$,requests,modelUpdates,settle,context};
}
test('Member page uses a real dropdown and submits both Cursor and Codex, with provider-scoped model refresh',async()=>{
  const {$,requests,modelUpdates,settle}=await page();
  const selector=$('agent-adapter');assert.equal(selector.tag,'select');
  assert.deepEqual(selector.children.map(o=>o.value),['cursor-agent','codex-agent']);
  $('agent-runtime').value='dev';$('agent-runtime').listeners.change();
  for(const adapter of ['cursor-agent','codex-agent']){
    $('agent-model').value='previous-provider-model';selector.value=adapter;selector.listeners.change();
    assert.equal($('agent-model').value,'');assert.equal(modelUpdates.at(-1).adapterID,adapter);assert.equal(modelUpdates.at(-1).runtimeID,'dev');
    $('agent-name').value=adapter;$('agent-form').onsubmit({preventDefault(){}});await settle();
    const request=requests.filter(r=>r.method==='POST').at(-1);
    assert.equal(request.body.adapter_id,adapter);assert.equal(request.body.runtime_id,'dev');assert.equal(request.body.model_id,'');
    assert.equal(request.body.reasoning_effort,'');
    assert.equal(selector.value,adapter);
  }
});
test('Role designer preserves and locks an existing session even if its adapter is no longer advertised',async()=>{
  const {$}=await page({pinned:true});
  assert.equal($('builder-adapter').tag,'select');assert.equal($('builder-adapter').value,'retired-plugin');
  assert.equal($('builder-adapter').disabled,true);assert.equal($('builder-model').value,'pinned-model');
  assert.ok($('builder-adapter').children.find(o=>o.value==='retired-plugin').disabled);
});
