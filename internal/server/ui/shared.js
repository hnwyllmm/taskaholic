'use strict';
window.WA=(()=>{
  const el=(tag,text,cls)=>{const n=document.createElement(tag);if(text!=null)n.textContent=text;if(cls)n.className=cls;return n;};
  const token=()=>{try{return sessionStorage.getItem('wa.apiToken')||'';}catch{return '';}};
  const saveToken=value=>{try{if(value)sessionStorage.setItem('wa.apiToken',value);else sessionStorage.removeItem('wa.apiToken');}catch{throw Error('浏览器不允许保存标签页会话，请允许会话存储后再连接。');}};
  // getRandomValues also works on HTTP intranet origins; randomUUID requires
  // a secure context. Keep 128 bits of randomness for idempotency keys.
  const key=()=>{const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join('');};
  const notice=(message,error=false)=>{const n=document.getElementById('notice');if(n){n.textContent=message;n.hidden=!message;n.className=error?'error':'';}};
  const api=async(path,method='GET',body)=>{const r=await fetch('/api/v1'+path,{method,headers:{'Content-Type':'application/json',...(token()?{Authorization:'Bearer '+token()}:{})},body:body===undefined?undefined:JSON.stringify(body)});const value=await r.json();if(!r.ok)throw Error(r.status===401?'请在右上角连接设置中填写 API Token。':value.error||`请求失败 (${r.status})`);return value;};
  const download=async(path,name)=>{const r=await fetch('/api/v1'+path,{headers:token()?{Authorization:'Bearer '+token()}:{}});if(!r.ok)throw Error('下载失败：'+r.status);const url=URL.createObjectURL(await r.blob()),a=el('a');a.href=url;a.download=name;document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),10000);};
  const labels={NEW:'新建',QUEUED:'排队中',IN_PROGRESS:'进行中',WAITING_REVIEW:'待你验收',WAITING_INPUT:'待你回复',BLOCKED:'需要处理',PAUSED:'已暂停',COMPLETED:'已完成',WAITING_SUBTASKS:'等待子任务'};
  const link=(text,href,cls)=>{const a=el('a',text,cls);a.href=href;return a;};
  const badge=state=>{const n=el('span',labels[state]||state,'status-pill');n.dataset.state=state;return n;};
  const date=ms=>new Date(ms).toLocaleString('zh-CN',{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit'});
  const header=document.querySelector('[data-app-shell]');
  if(header){
    const page=document.body.dataset.page;header.className='app-header';
    const brand=link('','/','app-brand');brand.append(el('span','W','app-mark'),el('span','Work Assistant'));header.append(brand);
    const nav=el('nav',null,'app-nav');nav.setAttribute('aria-label','主导航');for(const [id,title,url] of [['home','首页','/'],['tasks','工作列表','/tasks'],['members','成员管理','/members'],['team','团队资料','/team'],['system','系统状态','/system']]){const a=link(title,url,id===page?'selected':'');if(id===page)a.setAttribute('aria-current','page');nav.append(a);}header.append(nav);
    const connection=el('div',null,'app-connection'),status=el('span','连接中…','connection-label');status.id=page==='tasks'?'health':'connection';connection.append(status);
    const settings=el('details',null,'auth-settings');settings.append(el('summary','连接设置'));const panel=el('div',null,'auth-popover'),label=el('label','API Token'),input=el('input');input.id='token';input.type='password';input.autocomplete='off';input.value=token();label.append(input);panel.append(label,el('p','连接后仅保留在此标签页会话中，支持切换页面；不写入 URL 或长期存储。','muted small'));const button=el('button','连接');button.id='connect';panel.append(button);const clear=el('button','清除 Token');clear.onclick=()=>{saveToken('');input.value='';location.reload();};panel.append(clear);settings.append(panel);connection.append(settings);
    if(page==='tasks'){const b=el('button','备份');b.id='backup';connection.append(b);}header.append(connection);
  }
  return{el,token,saveToken,key,notice,api,download,labels,link,badge,date};
})();
