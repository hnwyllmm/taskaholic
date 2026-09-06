// Local authenticated administration; token is read from the private service
// environment, not exposed through argv, stdout or an HTTP URL.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const [endpoint='/system',method='GET',body]=process.argv.slice(2);
if(!endpoint.startsWith('/')||endpoint.startsWith('//')||endpoint.includes('://'))throw Error('Provide an API-relative path.');
const file=path.join(os.homedir(),'.config','work-assistant','dev.env');
const token=fs.readFileSync(file,'utf8').split('\n').find(x=>x.startsWith('ASSISTANT_API_TOKEN='))?.slice('ASSISTANT_API_TOKEN='.length);
if(!token)throw Error('Missing service API token.');
const response=await fetch('http://127.0.0.1:17343/api/v1'+endpoint,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},body,signal:AbortSignal.timeout(15000)});
const text=await response.text();console.log(text);if(!response.ok)process.exitCode=1;
