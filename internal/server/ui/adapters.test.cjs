const {test}=require('node:test');
const assert=require('node:assert/strict');
const {suggested}=require('./adapters.js');
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
