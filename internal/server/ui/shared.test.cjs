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

class TextNode{constructor(value){this.nodeType=3;this.textContent=value;}}
class Element{
  constructor(tag){this.tag=tag;this.children=[];this.dataset={};this.className='';this.hidden=false;}
  append(...items){this.children.push(...items);}
  get textContent(){return this.children.map(item=>item.textContent||'').join('');}
  set textContent(value){this.children=value==null?[]:[new TextNode(String(value))];}
  setAttribute(){}
  querySelector(){return null;}
}
function renderer(){
  const document={createElement:tag=>new Element(tag),createTextNode:value=>new TextNode(value),getElementById:()=>null,querySelector:()=>null,body:new Element('body')};
  const ctx=vm.createContext({document,console,fetch:async()=>({ok:false}),sessionStorage:{getItem:()=>'',setItem(){},removeItem(){}},location:{reload(){}},crypto:{getRandomValues:a=>a.fill(1)},URL:{createObjectURL:()=>'',revokeObjectURL(){}},setTimeout(){}});ctx.window=ctx;
  vm.runInContext(fs.readFileSync(path.join(__dirname,'shared.js'),'utf8'),ctx,{filename:'shared.js'});return ctx.WA.markdown;
}
const descendants=(node,tag)=>node.children.flatMap(child=>child instanceof Element?[...(child.tag===tag?[child]:[]),...descendants(child,tag)]:[]);

test('Markdown renders common work records without accepting HTML',()=>{
  const markdown=renderer(),root=markdown('# 方案\n\n- **修改** `main.go`\n- [x] 已验证\n\n| 项目 | 状态 |\n| --- | --- |\n| 构建 | 通过 |\n\n```sh\ngo test ./...\n```\n\n[PR](https://github.com/o/r/pull/1) <script>alert(1)</script>');
  assert.equal(descendants(root,'h1')[0].textContent,'方案');
  assert.equal(descendants(root,'li').length,2);assert.equal(descendants(root,'input')[0].checked,true);
  assert.equal(descendants(root,'table').length,1);assert.match(descendants(root,'pre')[0].textContent,/go test/);
  assert.equal(descendants(root,'a')[0].href,'https://github.com/o/r/pull/1');assert.equal(descendants(root,'script').length,0);
  assert.match(root.textContent,/<script>alert\(1\)<\/script>/);
});

test('Task workspace follows the human reading order',()=>{
  const html=fs.readFileSync(path.join(__dirname,'tasks.html'),'utf8');
  for(const [before,after] of [['id="activity-panel"','id="composer"'],['id="composer"','id="messages"'],['id="messages"','id="artifact"'],['id="artifact"','id="review-chat-slot"'],['id="review-chat-slot"','id="development-phase"'],['id="development-phase"','id="summary"'],['id="summary"','id="publications"']])assert.ok(html.indexOf(before)<html.indexOf(after),`${before} should precede ${after}`);
});
