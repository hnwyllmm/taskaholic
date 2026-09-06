// One-time private configuration generation. Never print tokens or put them in
// Git, URLs or service arguments. Existing credentials are not overwritten.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';

const root='/data/wangyunlai.wyl/workspace/work-assistant';
if(process.platform!=='linux'||fs.realpathSync(process.cwd())!==root)throw Error('Run only from the dev deployment checkout.');
const config=path.join(os.homedir(),'.config','work-assistant');
const backup='/data/wangyunlai.wyl/work-assistant-backups/dev';
for(const dir of [config,path.join(root,'data','dev'),backup]){
  fs.mkdirSync(dir,{recursive:true,mode:0o700});
  if(fs.lstatSync(dir).isSymbolicLink()||(fs.statSync(dir).mode&0o077)!==0)throw Error('Expected a private real directory: '+dir);
}
const envFile=path.join(config,'dev.env');
if(!fs.existsSync(envFile)){
  const fd=fs.openSync(envFile,'wx',0o600);
  try{fs.writeFileSync(fd,`ASSISTANT_API_TOKEN=${crypto.randomBytes(32).toString('hex')}\nASSISTANT_RUNTIME_TOKEN=${crypto.randomBytes(32).toString('hex')}\nASSISTANT_BACKUP_DIR=${backup}\n`);fs.fsyncSync(fd);}finally{fs.closeSync(fd);}
}
const info=fs.lstatSync(envFile);
if(!info.isFile()||(info.mode&0o077)!==0)throw Error('Configuration is not a private regular file.');
const names=fs.readFileSync(envFile,'utf8').split('\n').map(line=>line.split('=')[0]);
for(const key of ['ASSISTANT_API_TOKEN','ASSISTANT_RUNTIME_TOKEN','ASSISTANT_BACKUP_DIR'])if(!names.includes(key))throw Error('Existing configuration is incomplete: '+key);
console.log(JSON.stringify({root,environment_file:envFile,backup_directory:backup,secrets:'not displayed'},null,2));
