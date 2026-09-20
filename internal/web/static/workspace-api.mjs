export class APIError extends Error {
  constructor(message,status=0,code='') {super(message);this.status=status;this.code=code;}
}
// 命令通过同源 HTTP；Cookie 由浏览器管理，公开状态不包含访问凭证。
export async function api(path,body) {
  let response;
  try {
    response=await fetch(path,{method:body===undefined?'GET':'POST',credentials:'same-origin',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
  } catch {throw new APIError('连接中断，尚不能确认本次提交是否已接受。请保留原内容，使用同一提交重试。');}
  let data;
  try{data=await response.json();}catch{throw new APIError('未能读取服务端回复，请使用同一提交重试。',response.ok?0:response.status);}
  if(!response.ok) throw new APIError(data.error||'请求未被接受，请稍后重试。',response.status,data.code||'');
  return data;
}
export const conversationPath=id=>'/api/conversations/'+encodeURIComponent(id);
// 只接受同源受保护版本地址；UI 不消费供应商地址或任意脚本 URL。
export function fileURL(version,conversationID) {
  const expected=conversationPath(conversationID)+'/versions/'+encodeURIComponent(version.id)+'/file';
  return version.url===expected?version.url:expected;
}
export function downloadURL(version,conversationID) {return fileURL(version,conversationID)+'?download=1';}
