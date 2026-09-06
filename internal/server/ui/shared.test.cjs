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
