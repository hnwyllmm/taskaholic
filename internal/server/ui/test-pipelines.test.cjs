const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
function fixture(){
  class Element{constructor(tag){this.tag=tag;this.children=[];this.dataset={};}append(...n){this.children.push(...n);}replaceChildren(...n){this.children=n;}}
  const document={createElement:tag=>new Element(tag),createTextNode:text=>({textContent:text})};
  const ctx=vm.createContext({document,URL});ctx.window=ctx;vm.runInContext(fs.readFileSync(path.join(__dirname,'test-pipelines.js'),'utf8'),ctx);
  return {widget:ctx.WATestPipelines,root:new Element('section')};
}
const text=n=>[n.textContent||'',...n.children.map(text)].join(' ');
const find=(n,tag)=>[...(n.tag===tag?[n]:[]),...n.children.flatMap(x=>x.children?find(x,tag):[])];
test('Pipeline history is a compact table with pinned safe links and all attempts',()=>{
  const {widget,root}=fixture(),p={request_id:'request',pr_target_id:'pr',pipeline_id:42,head_sha:'a'.repeat(40),state:'failed',attempt:1,current:true,created_at_ms:1,reason:'<img src=x onerror=alert(1)>',url:'javascript:alert(1)',failed_jobs:[{id:9,name:'recovery',status:'failed',failure_reason:'script_failure'}]};
  widget.render(root,[p,{...p,request_id:'retry',pipeline_id:43,state:'running',attempt:2,retry_of:'request'}]);
  assert.equal(root.hidden,false);assert.equal(find(root,'table').length,1);assert.equal(find(root,'tr').length,3);
  for(const a of find(root,'a')){assert.match(a.href,/^https:\/\/gitlab\.oceanbase-dev\.com\/obqa\/seekdb_test\/-\/(pipelines|jobs)\/\d+$/);assert.equal(a.rel,'noopener noreferrer');}
  assert.ok(find(root,'p').some(n=>n.textContent===p.reason));assert.ok(find(root,'p').some(n=>n.textContent.includes('script_failure')));
  assert.equal(widget.blocked([p]),true);assert.equal(widget.blocked([{...p,state:'success'}]),false);
  assert.equal(widget.blocked([{...p,state:'success',current:false}]),true,'old SHA cannot approve current work');
  assert.equal(widget.blocked([{...p,state:'success'}, {...p,state:'running',attempt:2}]),true,'latest retry must pass');
  widget.render(root,[]);assert.equal(root.hidden,true);
});
test('Pipeline UI uses text nodes and script loads before the task controller',()=>{
  assert.ok(!fs.readFileSync(path.join(__dirname,'test-pipelines.js'),'utf8').includes('innerHTML'));
  const html=fs.readFileSync(path.join(__dirname,'tasks.html'),'utf8');assert.ok(html.indexOf('/assets/test-pipelines.js')<html.indexOf('/tasks/tasks.js'));
});

test('Publications show sticky links, uncertainties and safe text in a compact table',()=>{
 const {widget,root}=fixture(),p={key:'key',task_id:'task',url:'https://github.com/oceanbase/seekdb/pull/123',remote_url:'https://github.com/oceanbase/seekdb/pull/123#issuecomment-101',head_sha:'a'.repeat(40),sticky:true,state:'SYNCED',body:'<script>not html</script>'};let resolved;
 widget.renderPublications(root,[p,{...p,state:'UNCERTAIN',remote_url:'javascript:alert(1)',error:'timeout'}],{resolve:p=>resolved=p});
 assert.equal(find(root,'table').length,1);assert.equal(find(root,'tr').length,3);assert.equal(find(root,'a').length,1);assert.equal(find(root,'a')[0].href,p.remote_url);
 assert.match(text(root),/不是|不等于/);assert.match(text(root),/已同步/);assert.match(text(root),/结果待核对/);assert.match(text(root),/<script>/);
 find(root,'button')[0].onclick();assert.equal(resolved.state,'UNCERTAIN');widget.renderPublications(root,[]);assert.equal(root.hidden,true);
});
