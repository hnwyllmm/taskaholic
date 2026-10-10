const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');

async function fixture({targets=[]}={}){
  class Element{
    constructor(tag='div'){this.tag=tag;this.children=[];this.value='';this.open=false;}
    append(...nodes){this.children.push(...nodes);}
    replaceChildren(...nodes){this.children=[...nodes];}
    showModal(){this.open=true;}close(){this.open=false;}reset(){}
    querySelectorAll(){return this.children.flatMap(x=>x.children).filter(n=>n.tag==='input'&&n.checked);}
  }
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8'),elements=new Map();
  for(const m of html.matchAll(/<(\w+)[^>]*\bid="([^"]+)"/g))elements.set(m[2],new Element(m[1]));
  elements.set('connect',new Element('button'));elements.set('connection',new Element());elements.set('token',new Element());
  const $=id=>{assert.ok(elements.has(id),'missing HTML element '+id);return elements.get(id);};
  const multica={source_id:'antmultica',kind:'antmultica',name:'My tasks',enabled:true,version:4,interval_seconds:60,config:{workspace_id:'workspace',workspace_slug:'seekdb',assignee_id:'me',iteration_key:'迭代',iteration_value:'1.5.0',role_id:'dev'}};
  const github={source_id:'github',kind:'github',name:'GitHub',enabled:true,version:2,interval_seconds:5,config:{reviewer_role_ids:['reviewer'],ignore_logins:[]}};
  const data={sources:[multica,github],targets,events:[],reviews:[]},calls=[],notices=[];
  const el=(tag,text,cls)=>Object.assign(new Element(tag),{textContent:text,className:cls});
  const api=async(route,method='GET',body)=>{
    calls.push({route,method,body});
    if(method==='PUT'&&route.startsWith('/source-targets/')){
      const target=data.targets.find(t=>t.target_id===decodeURIComponent(route.split('/').at(-1)));
      assert.ok(target,'unknown target');target.enabled=body.enabled;return {};
    }
    if(method!=='GET')return {};
    if(route==='/sources')return structuredClone(data);
    if(route==='/roles')return {roles:[{role_id:'dev',name:'Developer'},{role_id:'reviewer',name:'Reviewer'}]};
    if(route==='/projects')return {projects:[]};
    if(route==='/work/tasks')return {tasks:[{task_id:'owned',assigned_agent_id:'agent',title:'Started',state:'WAITING_REVIEW'},{task_id:'new',title:'Unowned',state:'NEW'}]};
    throw Error('unexpected route '+route);
  };
  const context=vm.createContext({document:{getElementById:$,querySelectorAll:()=>[],hidden:false},WA:{el,api,notice:(...n)=>notices.push(n),link:(text,href)=>Object.assign(el('a',text),{href}),date:String,key:()=> 'key',saveToken:()=>{}},setTimeout:()=>1,location:{search:'?task=owned'},URLSearchParams,console});
  context.window=context;context.addEventListener=()=>{};
  vm.runInContext(fs.readFileSync(path.join(__dirname,'sources.js'),'utf8'),context);
  const settle=async()=>{for(let i=0;i<4;i++)await new Promise(setImmediate);};await settle();
  return {$,calls,settle,notices,data};
}
test('Task sources editor preserves CAS and exact identity while changing iteration',async()=>{
  const {$,calls,settle}=await fixture();
  $('sources').children[1].children.at(-1).onclick();
  assert.equal($('iteration-value').value,'1.5.0');assert.equal($('assignee-id').readOnly,true);
  $('iteration-value').value='1.6.0';
  $('source-form').onsubmit({preventDefault(){}});await settle();
  const request=calls.find(c=>c.method==='PUT');
  assert.equal(request.route,'/sources/antmultica');
  assert.equal(request.body.expected_version,4);assert.equal(request.body.source.config.iteration_value,'1.6.0');
  assert.equal(request.body.source.config.assignee_id,'me');assert.equal(request.body.source.config.role_id,undefined);
  assert.equal(request.body.source.config.defer_assignment,undefined);assert.equal(request.body.source.config.project_id,undefined);
});
test('GitHub source edits only collection settings, never reviewer assignments',async()=>{
  const {$,calls,settle}=await fixture();$('sources').children[2].children.at(-1).onclick();
  $('ignore-logins').value='bot-one, bot-two';$('source-form').onsubmit({preventDefault(){}});await settle();
  const request=calls.find(c=>c.method==='PUT');
  assert.deepEqual(structuredClone(request.body.source.config),{ignore_logins:['bot-one','bot-two']});
  assert.equal(request.body.expected_version,2);assert.ok(!calls.some(c=>c.route==='/projects'));
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8');
  for(const id of ['reviewer-roles','source-role','source-project','defer-assignment'])assert.ok(!html.includes('id="'+id+'"'));
  assert.match(html,/执行成员及评审分工由 Router 决定/);
});
test('PR registration offers only owned tasks and preserves the original task id',async()=>{
  const {$,calls,settle}=await fixture();$('register-pr').onclick();
  assert.deepEqual($('pr-task').children.map(n=>n.value),['','owned']);
  $('pr-url').value='https://github.com/o/r/pull/1';
  $('pr-form').onsubmit({preventDefault(){}});await settle();
  const request=calls.find(c=>c.method==='POST');
  assert.equal(request.route,'/work/tasks/owned/pull-requests');assert.equal(request.body.source_id,'github');
});
test('Sources UI does not render external input as HTML or collect credentials',()=>{
  const js=fs.readFileSync(path.join(__dirname,'sources.js'),'utf8');
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8');
  assert.ok(!js.includes('innerHTML'));
  assert.ok(!html.includes('type="password"'));
  assert.match(js,/expected_version:original.version/);
});

const content=n=>[n.textContent??'',...(n.children||[]).map(content)].join(' ');
const target=(fields={})=>({target_id:'pr-target',source_id:'github',entity:'https://github.com/oceanbase/seekdb/pull/1358',task_id:'owned',enabled:true,last_success_ms:1000,next_poll_ms:2000,head_sha:'abcdef0123456789012345678901234567890123ab',...fields});

test('Polling targets render compact semantic rows while retaining versions, timestamps and links',async()=>{
  const {$}=await fixture({targets:[target(),target({target_id:'multica-target',source_id:'antmultica',entity:'workspace',task_id:undefined,head_sha:undefined})]});
  assert.equal($('targets').tag,'tbody');assert.equal($('targets-count').textContent,'2 个目标');assert.equal($('targets').children.length,2);
  const row=$('targets').children[0];assert.equal(row.tag,'tr');assert.equal(row.children.length,7);
  assert.equal(row.children[0].tag,'th');assert.equal(row.children[0].scope,'row');
  const pr=row.children[0].children[0];assert.equal(pr.textContent,'oceanbase/seekdb #1358');assert.equal(pr.href,target().entity);assert.equal(pr.rel,'noopener noreferrer');
  assert.equal(row.children[3].textContent,'1000');assert.equal(row.children[4].textContent,'2000');
  assert.equal(row.children[5].children[0].textContent,'abcdef012345');assert.equal(row.children[5].children[0].title,target().head_sha);
  assert.equal(row.children[6].children[0].children[0].href,'/tasks#owned');
  assert.equal($('targets').children[1].children[0].children[0].textContent,'seekdb');
  assert.ok(!$('targets').children.some(n=>n.tag==='article'));
});

test('Table pause and resume actions update only the selected target',async()=>{
  const {$,calls,settle,data}=await fixture({targets:[target({target_id:'pr/one'}),target({target_id:'second',enabled:false})]});
  let buttons=$('targets').children[0].children[6].children[0].children;
  buttons.at(-1).onclick();await settle();
  assert.deepEqual(structuredClone(calls.find(c=>c.method==='PUT')),{route:'/source-targets/pr%2Fone',method:'PUT',body:{enabled:false}});
  assert.equal(data.targets[1].enabled,false);assert.match(content($('targets').children[0]),/已停止跟踪/);
  assert.equal($('targets').children[0].children[4].textContent,'—');
  buttons=$('targets').children[0].children[6].children[0].children;assert.equal(buttons.at(-1).textContent,'恢复轮询');
  buttons.at(-1).onclick();await settle();assert.equal(data.targets[0].enabled,true);
  assert.equal(calls.filter(c=>c.method==='PUT').length,2);assert.equal(data.targets[1].enabled,false);
});

test('Long error details remain collapsed by default and retain manual expansion on refresh',async()=>{
  const error='<script>do not execute</script>\n'+'Network failure '.repeat(30),{$,settle,data}=await fixture({targets:[target({error})]});
  let cell=$('targets').children[0].children[2],details=cell.children[1];
  assert.match(content(cell),/连接需要检查/);assert.equal(details.open,false);assert.equal(details.children[1].textContent,error);
  details.open=true;details.ontoggle();$('refresh').onclick();await settle();
  details=$('targets').children[0].children[2].children[1];assert.equal(details.open,true);
  data.sources.find(s=>s.source_id==='github').enabled=false;$('refresh').onclick();await settle();
  cell=$('targets').children[0].children[2];assert.equal(cell.children[0].textContent,'任务源已停用');assert.equal($('targets').children[0].children[4].textContent,'—');
});

test('Empty targets keep table structure and narrow screens scroll instead of reverting to large cards',async()=>{
  const {$}=await fixture();const row=$('targets').children[0];
  assert.equal(row.tag,'tr');assert.equal(row.children[0].colSpan,7);assert.match(row.children[0].textContent,/尚无轮询目标/);
  const html=fs.readFileSync(path.join(__dirname,'sources.html'),'utf8'),css=fs.readFileSync(path.join(__dirname,'sources.css'),'utf8');
  assert.equal([...html.matchAll(/<th scope="col">/g)].length,7);
  assert.match(html,/role="region" aria-label="轮询目标表格，可横向滚动" tabindex="0"/);
  assert.match(css,/\.source-target-scroll\{[^}]*overflow-x:auto/);
  assert.match(css,/\.source-target-table\{[^}]*table-layout:fixed/);
});

test('Tracking targets precede stopped targets and disabled sources, including retrying connections',async()=>{
  const rows=[target({target_id:'paused',enabled:false,entity:'https://github.com/o/r/pull/1'}),target({target_id:'disabled-source',source_id:'off',entity:'https://github.com/o/r/pull/2'}),target({target_id:'retrying',error:'temporary connection error',entity:'https://github.com/o/r/pull/3'}),target({target_id:'active',entity:'https://github.com/o/r/pull/4'})];
  const {$,data,settle}=await fixture({targets:rows});
  assert.deepEqual($('targets').children.map(r=>r.children[0].children[0].textContent),['o/r #3','o/r #4','o/r #1','o/r #2']);
  assert.equal(data.targets[0].target_id,'paused');
  $('refresh').onclick();await settle();
  assert.deepEqual($('targets').children.map(r=>r.children[0].children[0].textContent),['o/r #3','o/r #4','o/r #1','o/r #2']);
});

test('Pagination retains the current page on refresh and clamps after targets are removed',async()=>{
  const rows=Array.from({length:45},(_,i)=>target({target_id:'pr-'+i,entity:'https://github.com/o/r/pull/'+(i+1)}));
  const originalRows=[...rows];
  const {$,data,settle}=await fixture({targets:rows});
  assert.equal($('targets').children.length,20);assert.equal($('targets-prev').disabled,true);assert.equal($('targets-next').disabled,false);
  $('targets-next').onclick();assert.equal($('targets').children[0].children[0].children[0].textContent,'o/r #21');
  $('refresh').onclick();await settle();
  assert.match($('targets-page-info').textContent,/第 2 \/ 3 页/);assert.equal($('targets-prev').disabled,false);
  $('targets-next').onclick();assert.equal($('targets').children.length,5);assert.equal($('targets-next').disabled,true);
  data.targets.splice(0,40);$('refresh').onclick();await settle();
  assert.match($('targets-page-info').textContent,/第 1 \/ 1 页/);assert.equal($('targets').children.length,5);assert.equal($('targets-next').disabled,true);assert.equal($('targets-prev').disabled,true);
  data.targets.push(...originalRows.slice(0,40));$('refresh').onclick();await settle();
  $('targets-next').onclick();$('targets-page-size').value='50';$('targets-page-size').onchange();
  assert.equal($('targets').children.length,45);assert.match($('targets-page-info').textContent,/第 1 \/ 1 页/);
});
