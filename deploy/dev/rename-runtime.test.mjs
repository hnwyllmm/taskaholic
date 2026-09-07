import {test} from 'node:test';
import assert from 'node:assert/strict';
import {DatabaseSync} from 'node:sqlite';
import {renameRuntime,snapshot,verifyRename} from './rename-runtime.mjs';

function fixture(t){
  const control=new DatabaseSync(':memory:'),spool=new DatabaseSync(':memory:');
  t.after(()=>{control.close();spool.close();});
  control.exec(`PRAGMA foreign_keys=ON;
    CREATE TABLE schema_version(version INTEGER); INSERT INTO schema_version VALUES(11);
    CREATE TABLE runtime(runtime_id TEXT PRIMARY KEY,state TEXT); INSERT INTO runtime VALUES('dev-cursor','OFFLINE');
    CREATE TABLE agent_profile(agent_id TEXT PRIMARY KEY,runtime_id TEXT REFERENCES runtime(runtime_id),data_json TEXT);
    CREATE TABLE session(session_id TEXT PRIMARY KEY,runtime_id TEXT,agent_session_ref TEXT);
    INSERT INTO session VALUES('same-session','dev-cursor','original-native-context');
    CREATE TABLE run(run_id TEXT PRIMARY KEY,runtime_id TEXT,state TEXT); INSERT INTO run VALUES('old-run','dev-cursor','COMPLETED');
    CREATE TABLE outbox_message(destination_id TEXT,status TEXT); INSERT INTO outbox_message VALUES('dev-cursor','DELIVERED');
    CREATE TABLE runtime_event_dedupe(runtime_id TEXT); INSERT INTO runtime_event_dedupe VALUES('dev-cursor');
    CREATE TABLE idempotency_key(scope TEXT,key TEXT,resource_id TEXT,created_at_ms INTEGER,PRIMARY KEY(scope,key));
    INSERT INTO idempotency_key VALUES('role.draft','local-helper-role:dev-cursor','original-helper-draft',1);
    CREATE TABLE upgrade_job(state TEXT); INSERT INTO upgrade_job VALUES('CANCELLED');
    CREATE TABLE maintenance(upgrade_id TEXT); INSERT INTO maintenance VALUES('');
    CREATE TABLE event_log(event_id TEXT,aggregate_type TEXT,aggregate_id TEXT,aggregate_seq INTEGER,event_type TEXT,schema_version INTEGER,occurred_at_ms INTEGER,recorded_at_ms INTEGER,producer_actor_id TEXT,payload_json TEXT);
    CREATE TABLE home_chat(data_json TEXT); INSERT INTO home_chat VALUES('untouched chat');`);
  const profile={agent_id:'same-agent',runtime_id:'dev-cursor',adapter_id:'cursor-agent',model_id:'user-model',max_concurrent:6,version:2};
  control.prepare('INSERT INTO agent_profile VALUES(?,?,?)').run(profile.agent_id,profile.runtime_id,Buffer.from(JSON.stringify(profile)));
  spool.exec(`CREATE TABLE local_run(state TEXT); INSERT INTO local_run VALUES('COMPLETED');
    CREATE TABLE inbound_message(status TEXT); INSERT INTO inbound_message VALUES('COMPLETED');
    CREATE TABLE outbound_message(status TEXT,params_json TEXT); INSERT INTO outbound_message VALUES('DELIVERED','{"runtime_id":"dev-cursor"}');`);
  return{control,spool};
}
test('Rename updates references atomically while retaining native sessions, model, capacity and history',t=>{
  const {control,spool}=fixture(t),before=snapshot(control),spoolBefore=snapshot(spool);
  assert.deepEqual(renameRuntime(control,spool,'dev-cursor','dev'),{from:'dev-cursor',to:'dev',members:1,sessions:1,runs:1});
  verifyRename(before,snapshot(control),'dev-cursor','dev');
  const profile=JSON.parse(Buffer.from(control.prepare('SELECT data_json FROM agent_profile').get().data_json).toString());
  assert.equal(profile.runtime_id,'dev');assert.equal(profile.model_id,'user-model');assert.equal(profile.max_concurrent,6);assert.equal(profile.version,3);
  assert.equal(control.prepare('SELECT agent_session_ref FROM session').get().agent_session_ref,'original-native-context');
  assert.equal(control.prepare("SELECT resource_id FROM idempotency_key WHERE key='local-helper-role:dev'").get().resource_id,'original-helper-draft');
  assert.deepEqual(snapshot(spool),spoolBefore);
  assert.throws(()=>renameRuntime(control,spool,'dev-cursor','dev'),/source runtime/);
});
test('Online, active, pending, conflicting and unknown-schema renames fail without any mutations',async t=>{
  for(const [target,sql,reason] of [
    ['control',"UPDATE runtime SET state='ONLINE'",/stop the runtime/],
    ['control',"UPDATE run SET state='RUNNING'",/active control/],
    ['control',"UPDATE outbox_message SET status='RETRY'",/undelivered control/],
    ['spool',"UPDATE outbound_message SET status='PENDING'",/undelivered runtime/],
    ['spool',"UPDATE inbound_message SET status='ACCEPTED'",/unfinished runtime/],
    ['spool',"UPDATE local_run SET state='RUNNING'",/active runtime/],
    ['control',"INSERT INTO runtime VALUES('dev','OFFLINE')",/destination/],
    ['control',"INSERT INTO idempotency_key VALUES('role.draft','local-helper-role:dev','other-draft',2)",/bootstrap identity/],
    ['control',"INSERT INTO upgrade_job VALUES('READY')",/unfinished upgrade/],
    ['control',"INSERT INTO maintenance VALUES('upgrade')",/maintenance/],
    ['control','UPDATE schema_version SET version=14',/schema version/],
    ['control','CREATE TABLE unknown_binding(runtime_id TEXT)',/unreviewed runtime reference/],
  ])await t.test(sql,t=>{
    const f=fixture(t);f[target].exec(sql);const before=snapshot(f.control),spoolBefore=snapshot(f.spool);
    assert.throws(()=>renameRuntime(f.control,f.spool,'dev-cursor','dev'),reason);
    assert.deepEqual(snapshot(f.control),before);assert.deepEqual(snapshot(f.spool),spoolBefore);
  });
});
test('Mid-transaction failure rolls back parent and member updates too',t=>{
  const {control,spool}=fixture(t),before=snapshot(control);
  control.exec("CREATE TRIGGER fail_rename BEFORE UPDATE ON run BEGIN SELECT RAISE(ABORT,'injected failure'); END");
  assert.throws(()=>renameRuntime(control,spool,'dev-cursor','dev'),/injected failure/);
  assert.deepEqual(snapshot(control),before);assert.equal(control.prepare('PRAGMA foreign_key_check').all().length,0);
});
