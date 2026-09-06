const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');

class Element {
  constructor(tag,text){this.tag=tag;this.textContent=text;this.children=[];this.dataset={};}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this.children=nodes;}
  setAttribute(){}
  all(tag){return this.children.flatMap(n=>[...(n.tag===tag?[n]:[]),...n.all(tag)]);}
}

test('Upgrade member selection supports local Cursor and Codex but rejects remote and unsupported agents',async()=>{
  for(const enabled of [true,false]){
    const box=new Element('div'),refresh=new Element('button');
    const agents=[['cursor','dev','cursor-agent'],['codex','dev','codex-agent'],['remote','other','cursor-agent'],['exec','dev','exec-agent']].map(([agent_id,runtime_id,adapter_id])=>({agent_id,name:agent_id,runtime_id,adapter_id,state:'ACTIVE',max_concurrent:1,active_runs:0}));
    const config={upgrade_enabled:enabled,local_runtime_id:'dev',slots:[{slot:'upgrade_builder',name:'升级构建',default_mode:'auto'}],bindings:[{slot:'upgrade_builder',mode:'auto',version:1}]};
    const window={addEventListener(){}};
    const context={window,document:{getElementById:id=>id==='system-agent-cards'?box:refresh},WA:{el:(tag,text)=>new Element(tag,text),notice:message=>{throw Error(message);},api:async endpoint=>({'/system/agents':config,'/agents':{agents},'/runtimes':{runtimes:[]}}[endpoint])}};
    vm.runInNewContext(fs.readFileSync(path.join(__dirname,'system-agents.js'),'utf8'),context);
    await window.WASystemAgents.refresh();
    const options=Object.fromEntries(box.all('option').map(o=>[o.value,o.disabled]));
    assert.equal(options.cursor,!enabled);assert.equal(options.codex,!enabled);
    assert.equal(options.remote,true);assert.equal(options.exec,true);
  }
});
