// 一切外部正文使用 textContent，避免将用户输入或 Agent 文字解释成 HTML。
export const $=id=>document.getElementById(id);
export function node(tag,text,cls) {const element=document.createElement(tag);if(text!==undefined)element.textContent=text;if(cls)element.className=cls;return element;}
export function shortTime(value) {const date=new Date(value);return Number.isNaN(date.getTime())?'':date.toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit'});}
export function fullTime(value) {if(!value||value.startsWith('0001'))return '';const date=new Date(value);return Number.isNaN(date.getTime())?'':date.toLocaleString('zh-CN');}
export const checkLabels={passed:'通过',failed:'未通过',unverifiable:'无法验证',pending:'待核验',not_applicable:'不适用'};
export function checks(target,items=[]) {
  target.replaceChildren();
  for(const check of items){const item=node('div',undefined,'check '+(checkLabels[check.status]?check.status:'unverifiable'));item.append(node('strong',`${check.name} · ${checkLabels[check.status]||'无法验证'}`),node('span',check.detail));target.append(item);}
}
