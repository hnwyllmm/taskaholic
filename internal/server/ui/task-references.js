'use strict';
(function(root){
  const el=(tag,text,cls)=>{const node=document.createElement(tag);if(text!=null)node.textContent=text;if(cls)node.className=cls;return node;};
  function safeURL(ref){
    if(ref.kind==='github.pr'&&/^https:\/\/github\.com\/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}\/[A-Za-z0-9][A-Za-z0-9_.-]{0,99}\/pull\/[1-9][0-9]{0,8}$/.test(ref.url))return ref.url;
    if(ref.kind==='antmultica.issue'&&/^https:\/\/antmultica\.alipay\.com\/[A-Za-z0-9][A-Za-z0-9_.-]{0,199}\/issues\/[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$/.test(ref.url))return ref.url;
    return null;
  }
  function render(container,refs){
    container.replaceChildren();const seen=new Set();
    for(const ref of refs||[]){
      const url=safeURL(ref);if(!url||seen.has(url))continue;seen.add(url);
      const row=el('div',null,'task-reference'),link=el('a',ref.label||url);
      link.href=url;link.target='_blank';link.rel='noopener noreferrer';link.title=url;
      row.append(link);
      if(ref.revision)row.append(el('span','commit '+ref.revision,'reference-revision'));
      container.append(row);
    }
    container.hidden=seen.size===0;
  }
  function presentation(task,work){return {title:work.review_brief?.title||task.title,goal:work.review_brief?.goal||task.goal};}
  function message(task,work,m){
    return work.review_brief&&m===work.messages?.[0]&&m.speaker==='user'&&m.content===task.goal?work.review_brief.goal:m.content;
  }
  root.WATaskReferences={render,presentation,message,safeURL};
})(window);
