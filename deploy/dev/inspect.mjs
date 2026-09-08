// Read-only deployment audit: print counts/digests, never chat text, native
// session IDs or credentials. Useful before/after a controlled restart.
import {DatabaseSync} from 'node:sqlite';
import {createHash} from 'node:crypto';
import path from 'node:path';
const directory=path.resolve(process.argv[2]||'data/dev');
const result={directory,databases:{}};
for(const name of ['control','runtime']){
  const db=new DatabaseSync(path.join(directory,name+'.sqlite'),{readOnly:true});
  try{
    const tables=new Set(db.prepare("SELECT name FROM sqlite_master WHERE type='table'").all().map(x=>x.name));
    const counts={},digests={};
    for(const table of ['task','run','session','task_session','agent_profile','home_chat','task_consultation','review_turn','upgrade_job','local_run','outbound_message','inbound_message']){
      if(!tables.has(table))continue;
      counts[table]=db.prepare('SELECT COUNT(*) AS n FROM '+table).get().n;
      if(['task','run','session','task_session','agent_profile','home_chat'].includes(table)){
        const rows=db.prepare('SELECT * FROM '+table+' ORDER BY 1').all();
        digests[table]=createHash('sha256').update(JSON.stringify(rows)).digest('hex');
      }
    }
    const integrity=db.prepare('PRAGMA quick_check').all(),foreignKeyErrors=db.prepare('PRAGMA foreign_key_check').all().length;
    const activeRuns=tables.has('run')?db.prepare("SELECT COUNT(*) AS n FROM run WHERE state IN ('QUEUED','RUNNING')").get().n:undefined;
    result.databases[name]={counts,digests,integrity,foreignKeyErrors,activeRuns};
    if(integrity.some(row=>Object.values(row)[0]!=='ok')||foreignKeyErrors)process.exitCode=1;
  }finally{db.close();}
}
console.log(JSON.stringify(result,null,2));
