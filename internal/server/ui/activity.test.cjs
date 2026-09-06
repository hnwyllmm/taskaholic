const {test}=require('node:test');
const assert=require('node:assert/strict');
const {parser,merge,actionKey}=require('./activity.js');

test('SSE arbitrary UTF-8 network chunks, CRLF, comments and multiline data',()=>{
  const wire=': connected\r\n\r\nid: 81\r\nevent: RunActivity\r\ndata: {"message":\r\ndata: "中文输出"}\r\n\r\n: heartbeat\n\nid: 82\ndata: {"done":true}\n\n';
  const bytes=new TextEncoder().encode(wire);
  for(const step of [1,2,3,7,4096]){const received=[];let beats=0;const feed=parser(e=>received.push(e),()=>beats++),decode=new TextDecoder();for(let i=0;i<bytes.length;i+=step)feed(decode.decode(bytes.subarray(i,i+step),{stream:true}));assert.equal(beats,2);assert.equal(received.length,2);assert.equal(received[0].id,'81');assert.equal(received[0].type,'RunActivity');assert.deepEqual(JSON.parse(received[0].data),{message:'中文输出'});assert.deepEqual(JSON.parse(received[1].data),{done:true});}
});
test('Incomplete and oversized frames are not acknowledged',()=>{
  const events=[],feed=parser(e=>events.push(e));feed('id: 7\ndata: {"unfinished":');assert.equal(events.length,0);assert.throws(()=>feed('x'.repeat(600000)),/上限/);
});
test('Snapshots replace output, deduplicate, retain first-seq ordering and separate runs',()=>{
  const items=new Map();const a={run_id:'run-1',action_id:'item_0',first_seq:11,last_seq:11,output:'one',state:'RUNNING'};
  assert.equal(merge(items,a),true);assert.equal(merge(items,a),false);
  assert.equal(merge(items,{...a,first_seq:0,last_seq:14,output:'one\ntwo',state:'COMPLETED'}),true);
  assert.equal(merge(items,{...a,last_seq:12,output:'stale'}),false);
  assert.equal(items.get(actionKey(a)).first_seq,11);assert.equal(items.get(actionKey(a)).output,'one\ntwo');
  merge(items,{...a,run_id:'run-2',last_seq:20});assert.equal(items.size,2);
});
