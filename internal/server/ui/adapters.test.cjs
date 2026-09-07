const {test}=require('node:test');
const assert=require('node:assert/strict');
const {suggested,available,options}=require('./adapters.js');
const machine=(id,adapter,state='ONLINE')=>({runtime_id:id,state,capabilities:{adapters:{[adapter]:{role_instructions:true}}}});
test('Cursor-only runtime suggests Cursor, retaining compatible and typed choices',()=>{
  const runtimes=[machine('dev','cursor-agent'),machine('mac','codex-agent','OFFLINE')];
  assert.equal(suggested(runtimes,'dev','codex-agent'),'cursor-agent');
  assert.equal(suggested(runtimes,'','codex-agent'),'cursor-agent');
  assert.equal(suggested(runtimes,'mac','cursor-agent'),'codex-agent');
  assert.equal(suggested(runtimes,'dev','cursor-agent'),'cursor-agent');
  assert.equal(suggested(runtimes,'dev','custom',true),'custom');
  assert.equal(suggested(runtimes,'dev','codex-agent',false,true),'codex-agent');
});
test('Missing capabilities do not invent a compatible adapter',()=>{
  assert.equal(suggested([{runtime_id:'dev',state:'ONLINE',capabilities:{adapters:{exec:{}}}}],'dev','custom'),'custom');
  assert.equal(suggested([],'','codex-agent'),'codex-agent');
});
test('A dual-adapter runtime exposes both choices without replacing a current Cursor choice',()=>{
  const r=machine('dev','cursor-agent');r.capabilities.adapters['codex-agent']={role_instructions:true};
  assert.deepEqual(available([r],'dev'),['cursor-agent','codex-agent']);
  assert.equal(suggested([r],'dev','cursor-agent'),'cursor-agent');
  assert.equal(suggested([r],'dev','custom',true),'custom');
  assert.deepEqual(available([r],'other'),[]);
});
test('Dropdown always lists both adapters, even with Codex selected; plugins are discovered',()=>{
  const r=machine('dev','cursor-agent');r.capabilities.adapters['codex-agent']={role_instructions:true};
  r.capabilities.adapters['plugin-agent']={role_instructions:true};
  const list=options([r],'dev','codex-agent');
  assert.deepEqual(list.options.map(o=>o.value),['cursor-agent','codex-agent','plugin-agent']);
  assert.equal(list.value,'codex-agent');assert.ok(list.options.every(o=>!o.disabled));
  assert.equal(options([r],'dev','cursor-agent',true).value,'cursor-agent');
  const pinned=options([r],'dev','old-agent',false,true);
  assert.equal(pinned.value,'old-agent');assert.equal(pinned.options.at(-1).disabled,true);
  assert.deepEqual(options([],'',''),{value:'',options:[{value:'',label:'暂无可用 Agent 类型',disabled:true}]});
});
