'use strict';
window.WA=(()=>{
  const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  let apiTokenRequired=true;
  const token=()=>{if(!apiTokenRequired)return '';try{return sessionStorage.getItem('wa.apiToken')||'';}catch{return '';}};
  const saveToken=value=>{try{if(value)sessionStorage.setItem('wa.apiToken',value);else sessionStorage.removeItem('wa.apiToken');}catch{throw Error('浏览器不允许保存标签页会话，请允许会话存储后再连接。');}};
  // getRandomValues also works on HTTP intranet origins; randomUUID requires
  // a secure context. Keep 128 bits of randomness for idempotency keys.
  const key=()=>{const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join('');};
  const notice=(message,error=false)=>{const n=document.getElementById('notice');if(n){n.textContent=message;n.hidden=!message;n.className=error?'error':'';}};
  const api=async(path,method='GET',body)=>{const r=await fetch('/api/v1'+path,{method,headers:{'Content-Type':'application/json',...(token()?{Authorization:'Bearer '+token()}:{})},body:body===undefined?undefined:JSON.stringify(body)});const value=await r.json();if(!r.ok)throw Error(r.status===401?'请在右上角连接设置中填写 API Token。':value.error||`请求失败 (${r.status})`);return value;};
  const download=async(path,name)=>{const r=await fetch('/api/v1'+path,{headers:token()?{Authorization:'Bearer '+token()}:{}});if(!r.ok)throw Error('下载失败：'+r.status);const url=URL.createObjectURL(await r.blob()),a=el('a');a.href=url;a.download=name;document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),10000);};
  const labels={NEW:'待分派',QUEUED:'排队中',IN_PROGRESS:'进行中',WAITING_REVIEW:'待你验收',WAITING_INPUT:'待你回复',BLOCKED:'需要处理',PAUSED:'已暂停',COMPLETED:'已完成',WAITING_SUBTASKS:'等待子任务'};
  const link=(text,href,cls)=>{const a=el('a',text,cls);a.href=href;return a;};
	Object.assign(labels,{WAITING_AUTHORIZATION:'等待授权',WAITING_ENVIRONMENT:'等待环境'});
  const badge=state=>{const n=el('span',labels[state]||state,'status-pill');n.dataset.state=state;return n;};
  const date=ms=>new Date(ms).toLocaleString('zh-CN',{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'});
  // Small safe Markdown renderer for work records. It builds DOM nodes rather
  // than accepting HTML, so Agent/user content cannot inject page markup.
  const markdown=(source,cls='markdown-body')=>{
    const root=el('div',null,cls),lines=String(source||'').replace(/\r\n?/g,'\n').split('\n');
    const inline=(parent,text)=>{
      const re=/(\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)|`([^`\n]+)`|\*\*([^*\n]+)\*\*|__([^_\n]+)__|\*([^*\n]+)\*|_([^_\n]+)_)/g;let at=0,match;
      while((match=re.exec(text))){if(match.index>at)parent.append(document.createTextNode(text.slice(at,match.index)));let item;if(match[2]){item=el('a',match[2]);item.href=match[3];item.target='_blank';item.rel='noopener noreferrer';}else if(match[4])item=el('code',match[4]);else if(match[5]||match[6])item=el('strong',match[5]||match[6]);else item=el('em',match[7]||match[8]);parent.append(item);at=re.lastIndex;}if(at<text.length)parent.append(document.createTextNode(text.slice(at)));
    };
    const cells=line=>line.replace(/^\s*\||\|\s*$/g,'').split('|').map(value=>value.trim());
    let paragraph=[];
    const flush=()=>{if(!paragraph.length)return;const p=el('p');inline(p,paragraph.join('\n'));root.append(p);paragraph=[];};
    for(let i=0;i<lines.length;){const line=lines[i];
      if(/^\s*```/.test(line)){flush();const language=line.trim().slice(3).trim(),body=[];i++;while(i<lines.length&&!/^\s*```/.test(lines[i]))body.push(lines[i++]);if(i<lines.length)i++;const pre=el('pre'),code=el('code',body.join('\n'));if(language)code.dataset.language=language;pre.append(code);root.append(pre);continue;}
      if(line.includes('|')&&i+1<lines.length&&/^\s*\|?\s*:?-{3,}/.test(lines[i+1])){flush();const table=el('table'),head=el('thead'),headRow=el('tr');for(const value of cells(line)){const th=el('th');inline(th,value);headRow.append(th);}head.append(headRow);table.append(head);i+=2;const body=el('tbody');while(i<lines.length&&lines[i].includes('|')&&lines[i].trim()){const row=el('tr');for(const value of cells(lines[i++])){const td=el('td');inline(td,value);row.append(td);}body.append(row);}table.append(body);root.append(table);continue;}
      const heading=line.match(/^\s*(#{1,4})\s+(.+)$/);if(heading){flush();const h=el('h'+heading[1].length);inline(h,heading[2]);root.append(h);i++;continue;}
      if(/^\s*([-*_])(?:\s*\1){2,}\s*$/.test(line)){flush();root.append(el('hr'));i++;continue;}
      const quote=line.match(/^\s*>\s?(.*)$/);if(quote){flush();const block=el('blockquote'),parts=[];while(i<lines.length){const q=lines[i].match(/^\s*>\s?(.*)$/);if(!q)break;parts.push(q[1]);i++;}inline(block,parts.join('\n'));root.append(block);continue;}
      const bullet=line.match(/^\s*[-+*]\s+(.+)$/),ordered=line.match(/^\s*\d+[.)]\s+(.+)$/);if(bullet||ordered){flush();const list=el(ordered?'ol':'ul');while(i<lines.length){const itemLine=lines[i].match(ordered?/^\s*\d+[.)]\s+(.+)$/:/^\s*[-+*]\s+(.+)$/);if(!itemLine)break;let value=itemLine[1],checked=value.match(/^\[([ xX])\]\s+(.*)$/),li=el('li');if(checked){const box=el('input');box.type='checkbox';box.disabled=true;box.checked=checked[1].toLowerCase()==='x';li.append(box);value=checked[2];}inline(li,value);list.append(li);i++;}root.append(list);continue;}
      if(!line.trim()){flush();i++;continue;}paragraph.push(line);i++;
    }flush();return root;
  };
  const syncAccess=async(settings,indicator)=>{
    try{
      const response=await fetch('/api/v1/auth/config',{cache:'no-store'});
      if(!response.ok)return;
      const config=await response.json();
      if(typeof config.api_token_required!=='boolean')return;
      apiTokenRequired=config.api_token_required;
      settings.hidden=!apiTokenRequired;indicator.hidden=apiTokenRequired;
    }catch{/* Keep connection settings available when policy cannot be read. */}
  };
  const header=document.querySelector('[data-app-shell]');
  if(header){
    const page=document.body.dataset.page;header.className='app-header';
    const brand=link('','/','app-brand');brand.append(el('span','W','app-mark'),el('span','Work Assistant'));header.append(brand);
    const nav=el('nav',null,'app-nav');nav.setAttribute('aria-label','主导航');for(const [id,title,url] of [['home','首页','/'],['tasks','工作列表','/tasks'],['members','成员管理','/members'],['team','团队资料','/team'],['sources','任务源','/sources'],['improvements','持续改进','/improvements'],['system','系统状态','/system']]){const a=link(title,url,id===page?'selected':'');if(id===page)a.setAttribute('aria-current','page');nav.append(a);}header.append(nav);
    const connection=el('div',null,'app-connection'),status=el('span','连接中…','connection-label');status.id=page==='tasks'?'health':'connection';connection.append(status);
    const settings=el('details',null,'auth-settings');settings.append(el('summary','连接设置'));const panel=el('div',null,'auth-popover'),label=el('label','API Token'),input=el('input');input.id='token';input.type='password';input.autocomplete='off';input.value=token();label.append(input);panel.append(label,el('p','连接后仅保留在此标签页会话中，支持切换页面；不写入 URL 或长期存储。','muted small'));const button=el('button','连接');button.id='connect';panel.append(button);const clear=el('button','清除 Token');clear.onclick=()=>{saveToken('');input.value='';location.reload();};panel.append(clear);settings.append(panel);connection.append(settings);
    const access=el('span','免登录模式','small muted');access.hidden=true;access.title='所有能连接此地址的设备都可以访问数据并操作任务；仅用于受信任网络。';connection.append(access);void syncAccess(settings,access);
    if(page==='tasks'){const b=el('button','备份');b.id='backup';connection.append(b);}header.append(connection);
  }
  if(header){const a=link('执行权限','/permissions',document.body.dataset.page==='permissions'?'selected':'');if(document.body.dataset.page==='permissions')a.setAttribute('aria-current','page');header.querySelector('.app-nav')?.append(a);}
  return{el,token,saveToken,key,notice,api,download,labels,link,badge,date,markdown,syncAccess};
})();
