const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

test('Task workspace renders its pending permission and can remember the exact scope',async()=>{
  class Element{
    constructor(tag='div',text=''){this.tag=tag;this.textContent=text||'';this.children=[];this.hidden=false;this.value='';}
    append(...children){this.children.push(...children);}replaceChildren(...children){this.children=children;}
  }
  const el=(tag,text,cls)=>{const node=new Element(tag,text);node.className=cls||'';return node;};
  const panel=new Element('section'),box=new Element('div'),calls=[];
  const requests=[
    {request_id:'wanted',task_id:'task-1',title:'Current',operation:'agent.network_access',runtime_id:'dev',repository:'oceanbase/seekdb',state:'PENDING',reason:'download',version:1},
    {request_id:'other',task_id:'task-2',title:'Other',operation:'agent.host_full_access',runtime_id:'dev',repository:'other/repo',state:'PENDING',reason:'host',version:1}
  ];
  const api=async(url,method='GET',body)=>{calls.push({url,method,body});if(method==='POST')requests[0].state='APPROVED';return{requests,policies:[],runtimes:[]};};
  const document={body:{dataset:{page:'tasks'}},hidden:false,getElementById:()=>null,querySelector:()=>null};
  let changed=0;
  const context=vm.createContext({window:{},document,confirm:()=>true,setInterval:()=>1,clearInterval(){},WA:{el,api,link:(text,href)=>{const n=el('a',text);n.href=href;return n;},notice(){}}});context.window=context;
  vm.runInContext(fs.readFileSync(path.join(__dirname,'permissions.js'),'utf8'),context);
  const mounted=context.WAPermissions.mountTask(panel,box,{changed:async()=>{changed++;}});
  await mounted.update('task-1','WAITING_AUTHORIZATION');
  const text=node=>node.textContent+' '+node.children.map(text).join(' ');
  assert.match(text(box),/Current/);assert.doesNotMatch(text(box),/Other/);assert.equal(panel.hidden,false);
  const buttons=[];(function walk(node){if(node.tag==='button')buttons.push(node);for(const child of node.children)walk(child);})(box);
  const remember=buttons.find(button=>button.textContent==='允许并记住此范围');assert.ok(remember);
  remember.onclick();for(let i=0;i<4;i++)await new Promise(setImmediate);
  const decision=calls.find(call=>call.method==='POST');
  assert.equal(decision.url,'/execution-permissions/requests/wanted');
  assert.equal(JSON.stringify(decision.body),JSON.stringify({expected_version:1,decision:'approve',remember:true}));
  assert.equal(changed,1);mounted.dispose();
  const html=fs.readFileSync(path.join(__dirname,'tasks.html'),'utf8');
  assert.ok(html.indexOf('/assets/permissions.js')<html.indexOf('/tasks/tasks.js'));
  assert.match(html,/id="task-permission-panel"/);
});
