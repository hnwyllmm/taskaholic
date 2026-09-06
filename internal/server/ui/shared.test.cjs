const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

test('HTTP intranet initialization and request keys do not require randomUUID',()=>{
  let calls=0;
  const context={window:{},document:{querySelector:()=>null},crypto:{getRandomValues(bytes){assert.equal(bytes.length,16);bytes.fill(++calls);return bytes;}}};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname,'shared.js'),'utf8'),context);
  assert.equal(context.window.WA.key(),'01'.repeat(16));
  assert.equal(context.window.WA.key(),'02'.repeat(16));
  assert.equal(calls,2);
});

test('Anonymous mode hides token settings without exposing or deleting saved credentials',async()=>{
  const settings={hidden:false},indicator={hidden:true};let required=false;
  const context={window:{},document:{querySelector:()=>null},sessionStorage:{getItem:()=> 'existing-test-token'},fetch:async()=>({ok:true,json:async()=>({api_token_required:required})})};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname,'shared.js'),'utf8'),context);
  await context.window.WA.syncAccess(settings,indicator);
  assert.equal(settings.hidden,true);assert.equal(indicator.hidden,false);assert.equal(context.window.WA.token(),'');
  required=true;await context.window.WA.syncAccess(settings,indicator);
  assert.equal(settings.hidden,false);assert.equal(indicator.hidden,true);assert.equal(context.window.WA.token(),'existing-test-token');
});

test('Unknown or unavailable auth policy leaves connection settings available',async()=>{
  for(const fetch of [async()=>({ok:false}),async()=>({ok:true,json:async()=>({})}),async()=>{throw Error('offline');}]){
    const settings={hidden:false},indicator={hidden:true};
    const context={window:{},document:{querySelector:()=>null},fetch};
    vm.runInNewContext(fs.readFileSync(path.join(__dirname,'shared.js'),'utf8'),context);
    await context.window.WA.syncAccess(settings,indicator);
    assert.equal(settings.hidden,false);assert.equal(indicator.hidden,true);
  }
});
