// Offline identity correction, not machine/session migration. The deployment
// wrapper must stop the supervisor and runtime and verify a backup first.
// Only control.sqlite is changed, in one transaction. The drained spool and
// historical events/transport messages stay byte-for-byte unchanged.
import assert from 'node:assert/strict';
import {randomUUID} from 'node:crypto';

const quote=name=>'"'+name.replaceAll('"','""')+'"';
const parse=value=>JSON.parse(typeof value==='string'?value:Buffer.from(value).toString());
export function snapshot(db){
  return Object.fromEntries(db.prepare("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name").all().map(({name})=>[name,db.prepare('SELECT * FROM '+quote(name)+' ORDER BY rowid').all().map(row=>({...row}))]));
}

// Used again after restarting the service. Heartbeat fields may change, but
// no task, native session reference, message or user configuration may drift.
export function verifyRename(before,after,from,to,{restarted=false}={}){
  assert.deepEqual(Object.keys(after),Object.keys(before),'database tables changed');
  for(const [table,rows] of Object.entries(before)){
    if(table==='event_log'){
      assert.deepEqual(after[table].slice(0,rows.length),rows,'historical events changed');
      const added=after[table].slice(rows.length);
      assert.ok(added.some(e=>e.event_type==='RuntimeRenamed'&&parse(e.payload_json).from===from&&parse(e.payload_json).to===to),'missing rename audit event');
      if(!restarted)assert.equal(added.length,1);
      continue;
    }
    const expected=rows.map(row=>{
      const copy={...row};
      if(['runtime','agent_profile','session','run'].includes(table)&&copy.runtime_id===from){
        copy.runtime_id=to;
        if(table==='agent_profile'){
          const profile=parse(copy.data_json);assert.equal(profile.runtime_id,from,'member JSON/reference mismatch');
          profile.runtime_id=to;profile.version=(profile.version||0)+1;
          const raw=JSON.stringify(profile);
          copy.data_json=typeof copy.data_json==='string'?raw:new Uint8Array(Buffer.from(raw));
        }
      }
      if(table==='runtime'&&restarted){
        const live=after.runtime.find(r=>r.runtime_id===copy.runtime_id);
        for(const key of ['epoch','state','connected_at_ms','last_seen_at_ms','disconnected_at_ms'])copy[key]=live?.[key];
      }
      return copy;
    });
    if(table==='idempotency_key'){
      const bootstrap=rows.find(r=>r.scope==='role.draft'&&r.key==='local-helper-role:'+from);
      if(bootstrap)expected.push({...bootstrap,key:'local-helper-role:'+to});
    }
    assert.deepEqual(after[table],expected,table+' changed beyond runtime identity');
  }
}

export function renameRuntime(control,spool,from,to){
  if(!/^[a-zA-Z0-9._-]{1,100}$/.test(from)||!/^[a-zA-Z0-9._-]{1,100}$/.test(to)||from===to)throw Error('distinct valid runtime IDs are required');
  control.exec('PRAGMA foreign_keys=ON; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000');
  spool.exec('PRAGMA busy_timeout=5000');
  // Lock the spool too: it must remain drained until the control transaction
  // commits. No distributed transaction or database restore is needed.
  spool.exec('BEGIN IMMEDIATE');
  let active=false;
  try{
    control.exec('BEGIN IMMEDIATE');active=true;
    control.exec('PRAGMA defer_foreign_keys=ON');
    if(![11,12,13,14,15,16].includes(control.prepare('SELECT MAX(version) AS version FROM schema_version').get().version))throw Error('unreviewed schema version');
    const original=control.prepare('SELECT * FROM runtime WHERE runtime_id=?').get(from);
    if(!original)throw Error('source runtime does not exist');
    if(original.state!=='OFFLINE')throw Error('stop the runtime before renaming');
    if(control.prepare('SELECT 1 FROM runtime WHERE runtime_id=?').get(to))throw Error('destination runtime already exists');
    const reject=(db,sql,message)=>{if(db.prepare(sql).get())throw Error(message);};
    reject(control,"SELECT 1 FROM run WHERE state NOT IN ('COMPLETED','FAILED','INTERRUPTED') LIMIT 1",'active control runs');
    reject(control,"SELECT 1 FROM outbox_message WHERE status!='DELIVERED' LIMIT 1",'undelivered control commands');
    reject(control,"SELECT 1 FROM upgrade_job WHERE state NOT IN ('SUCCEEDED','FAILED','ROLLED_BACK','CANCELLED') LIMIT 1",'unfinished upgrade');
    reject(control,"SELECT 1 FROM maintenance WHERE upgrade_id!='' LIMIT 1",'upgrade maintenance lock');
    reject(spool,"SELECT 1 FROM local_run WHERE state NOT IN ('COMPLETED','FAILED','INTERRUPTED','LOST') LIMIT 1",'active runtime runs');
    reject(spool,"SELECT 1 FROM outbound_message WHERE status!='DELIVERED' LIMIT 1",'undelivered runtime events');
    reject(spool,"SELECT 1 FROM inbound_message WHERE status NOT IN ('COMPLETED','FAILED','INTERRUPTED','APPLIED','REJECTED','LOST') LIMIT 1",'unfinished runtime commands');
    for(const db of [control,spool]){
      assert.ok(db.prepare('PRAGMA quick_check').all().every(r=>Object.values(r)[0]==='ok'),'database integrity failed');
      assert.equal(db.prepare('PRAGMA foreign_key_check').all().length,0,'foreign key errors');
    }
    const before=snapshot(control),spoolBefore=snapshot(spool);
    const bootstrap=before.idempotency_key.find(r=>r.scope==='role.draft'&&r.key==='local-helper-role:'+from);
    if(bootstrap){
      if(before.idempotency_key.some(r=>r.scope==='role.draft'&&r.key==='local-helper-role:'+to))throw Error('destination bootstrap identity already exists');
      control.prepare('INSERT INTO idempotency_key(scope,key,resource_id,created_at_ms) VALUES(?,?,?,?)').run(bootstrap.scope,'local-helper-role:'+to,bootstrap.resource_id,bootstrap.created_at_ms);
    }
    for(const table of Object.keys(before)){
      if(control.prepare('PRAGMA table_info('+quote(table)+')').all().some(c=>c.name==='runtime_id')&&!['runtime','agent_profile','session','run','runtime_event_dedupe'].includes(table))throw Error('unreviewed runtime reference table: '+table);
    }
    const updateProfile=control.prepare('UPDATE agent_profile SET runtime_id=?, data_json=? WHERE agent_id=?');
    for(const row of before.agent_profile.filter(a=>a.runtime_id===from)){
      const profile=parse(row.data_json);assert.equal(profile.runtime_id,from,'member JSON/reference mismatch');
      profile.runtime_id=to;profile.version=(profile.version||0)+1;
      const raw=JSON.stringify(profile);
      updateProfile.run(to,typeof row.data_json==='string'?raw:Buffer.from(raw),row.agent_id);
    }
    for(const table of ['runtime','session','run'])control.prepare('UPDATE '+quote(table)+' SET runtime_id=? WHERE runtime_id=?').run(to,from);
    const now=Date.now(),seq=control.prepare("SELECT COALESCE(MAX(aggregate_seq),0)+1 AS seq FROM event_log WHERE aggregate_type='runtime' AND aggregate_id=?").get(to).seq;
    control.prepare(`INSERT INTO event_log(event_id,aggregate_type,aggregate_id,aggregate_seq,event_type,schema_version,occurred_at_ms,recorded_at_ms,producer_actor_id,payload_json)
      VALUES(?,'runtime',?,?,'RuntimeRenamed',1,?,?,'manual-runtime-rename',?)`).run('evt_'+randomUUID(),to,seq,now,now,JSON.stringify({from,to,reason:'User requested machine-neutral runtime ID; same host, workspace and native sessions'}));
    assert.equal(control.prepare('PRAGMA foreign_key_check').all().length,0);
    verifyRename(before,snapshot(control),from,to);
    assert.deepEqual(snapshot(spool),spoolBefore,'runtime spool changed');
    control.exec('COMMIT');active=false;
    return {from,to,members:before.agent_profile.filter(a=>a.runtime_id===from).length,sessions:before.session.filter(s=>s.runtime_id===from).length,runs:before.run.filter(r=>r.runtime_id===from).length};
  }finally{
    try{if(active)control.exec('ROLLBACK');}finally{spool.exec('ROLLBACK');}
  }
}
