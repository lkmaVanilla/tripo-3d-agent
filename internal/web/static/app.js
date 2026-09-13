// 最小前端只负责发送用户命令和展示服务端事实；预算、调度与恢复判断全部在后端。
const $ = (id) => document.getElementById(id);
const labels = {understanding:'理解需求中',awaiting_answer:'等待你的回答',queued:'等待生产名额',queue_full:'队列繁忙',running:'生产进行中',completed:'技术检查通过',failed:'已结束',stopped:'已停止'};
// 事件名用于阅读时间线；尚未配置中文名称的新事件会回退显示原始 kind。
const eventNames = {request:'收到资产需求',model_call:'模型决策',agent_proposal:'Agent 提议',intent_and_plan:'意图与计划',clarification:'请求澄清',user_answer:'用户回答',clarification_resumed:'继续澄清',checkpoint:'保存检查点',skill_loaded:'加载任务 Skill',runtime_accepted:'Runtime 接受动作',runtime_blocked:'Runtime 拦截动作',production_slot:'获得生产名额',tool_submitting:'提交生产请求',tool_submitted:'收到任务 ID',tripo_progress:'Tripo 进度',technical_report:'技术检查报告',tool_failed:'工具失败',agent_finished:'Agent 结束请求',runtime_finished:'Runtime 结束请求',stopped:'用户停止',expired:'请求到期',queued:'重新入队',model_error:'模型调用失败'};
// current/seq 只属于当前页面会话；切换会话时重置游标，避免把另一会话事件混入。
// manualArtifact 记录用户手动选中的候选；后台更新不强制切走其正在查看的模型。
let current = null, socket = null, reconnect = null, seq = 0, selectedArtifact = '', manualArtifact = false, config = null;
// 外部文本统一经 textContent 写入 DOM，不把用户需求或模型结果当作 HTML 执行。
const node = (tag, text, cls) => { const e=document.createElement(tag);if(text!==undefined)e.textContent=text;if(cls)e.className=cls;return e; };
// 提示只影响当前页面，不改变会话的执行状态。
function notice(message,error=false){$('notice').textContent=message;$('notice').hidden=!message;$('notice').className=error?'error':'';}
// 同源 fetch 自动携带浏览器 Cookie，前端不保存或拼接匿名凭证。
// 无 body 表示查询，有 body 表示 JSON 命令；统一把非成功响应转成可展示错误。
async function api(path, body){const res=await fetch(path,{method:body===undefined?'GET':'POST',headers:body===undefined?{}:{'Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});const text=await res.text();let data;try{data=JSON.parse(text);}catch{data={error:text};}if(!res.ok)throw new Error(data.error||'请求失败');return data;}
// 主动切换会话时取消旧连接的重连回调，避免旧 socket 再次接管页面。
function disconnect(){clearTimeout(reconnect);if(socket){socket.onclose=null;socket.close();socket=null;}}
// 会话列表由服务端按匿名归属过滤，不能用本地缓存替代访问校验。
async function list(){const sessions=await api('/api/sessions');$('sessions').replaceChildren();for(const s of sessions){const b=node('button',s.request,s.id===current?'active':'');b.title=s.request;b.onclick=()=>openSession(s.id).catch(showError);$('sessions').append(b);}}
function showError(e){notice(e.message,true);}
// 先读取快照再订阅增量；hash 只保存会话 ID，找回内容仍需浏览器持有有效 Cookie。
async function openSession(id){disconnect();current=id;location.hash='session/'+id;seq=0;selectedArtifact='';manualArtifact=false;$('progress').value=0;$('progress-text').textContent='等待生成计划';$('events').replaceChildren();$('composer').hidden=true;$('workspace').hidden=false;const data=await api('/api/sessions/'+id);render(data);connect(id);await list();}
// WebSocket 只接收进度，用户操作走 HTTP。重连带上最后事件序号，重复事件由 render 去重。
// 重连或关闭页面不改变后台执行；服务端仍会独立检查凭证与会话归属。
function connect(id){if(current!==id)return;const protocol=location.protocol==='https:'?'wss:':'ws:';socket=new WebSocket(`${protocol}//${location.host}/api/sessions/${id}/events?after=${seq}`);socket.onopen=()=>{$('connection').textContent='进度已连接';};socket.onmessage=(e)=>{if(current===id)render(JSON.parse(e.data));};socket.onclose=()=>{if(current===id){$('connection').textContent='连接中断，正在重连';reconnect=setTimeout(()=>connect(id),1500);}};socket.onerror=()=>socket.close();}
// session 是当前公开快照，events 是本次增量；界面不从事件推演出另一份业务状态机。
function render({session:s,events}){
 $('title').textContent=s.request;$('status').textContent=labels[s.status]||s.status;$('budget').textContent=`生产 ${s.production}/${s.max_submissions} · 模型调用 ${s.model_calls}/${s.max_model_calls}`;
 // 可用操作由服务端状态决定；这里只控制展示，实际请求仍会再次经过后端校验。
 const terminal=['completed','failed','stopped'].includes(s.status);$('stop').hidden=terminal;$('retry').hidden=s.status!=='queue_full';$('export').href=`/api/sessions/${s.id}/trace`;
 $('question-panel').hidden=s.status!=='awaiting_answer';$('question').textContent=s.question||'';
 // 意图、默认假设和硬约束分别展示，生成目标和实际验收上限不能混为一谈。
 const intent=$('intent');intent.replaceChildren();if(s.intent){for(const [label,value] of [['资产',s.intent.asset],['用途',s.intent.use],['风格',s.intent.style],['验收上限',`${s.intent.max_triangles} 三角面 / ${(s.intent.max_bytes/1048576).toFixed(2)} MiB`]])intent.append(node('p',`${label}：${value}`));for(const assumption of s.intent.assumptions||[])intent.append(node('p','默认假设：'+assumption));for(const constraint of s.intent.constraints||[])intent.append(node('p','硬约束：'+constraint));const ol=node('ol');for(const step of s.intent.plan||[])ol.append(node('li',step));intent.append(ol);}else{intent.textContent='正在理解需求…';}
 $('deadline').textContent=s.deadline&&!s.deadline.startsWith('0001')?'本地执行截止：'+new Date(s.deadline).toLocaleString():'';
 // 当前会话的 seq 单调前移，快照/重连包含旧事件时不会重复追加时间线。
 for(const event of events||[]){if(event.seq<=seq)continue;seq=event.seq;const detail=node('details',undefined,'event');const summary=node('summary');summary.append(node('time',new Date(event.time).toLocaleTimeString()),node('span',eventNames[event.kind]||event.kind));detail.append(summary,node('pre',JSON.stringify(event.data,null,2)));$('events').append(detail);if(event.kind==='tripo_progress'){$('progress').value=event.data.progress;$('progress-text').textContent=`${event.data.status} · ${event.data.progress}% · ${event.data.task_id}`;}}
 if(s.status==='queued')$('progress-text').textContent='已入队，等待生产名额';if(s.status==='queue_full')$('progress-text').textContent='等待队列已满，需求已保留，可稍后重试';if(s.status==='completed')$('progress').value=100;
 // 默认展示已选交付产物或最新候选，同时保留手动查看失败候选的能力。
 const artifacts=s.artifacts||[];const picker=$('artifact-picker');const old=selectedArtifact;picker.replaceChildren();for(let i=0;i<artifacts.length;i++){const a=artifacts[i];const option=node('option',`候选 ${i+1}${a.report.passed?' · 技术通过':''}`);option.value=a.id;picker.append(option);}selectedArtifact=manualArtifact&&artifacts.some(a=>a.id===old)?old:(s.selected_artifact||artifacts.at(-1)?.id||'');picker.value=selectedArtifact;picker.hidden=artifacts.length<2;picker.onchange=()=>{manualArtifact=true;selectedArtifact=picker.value;showArtifact(artifacts.find(a=>a.id===selectedArtifact));};showArtifact(artifacts.find(a=>a.id===selectedArtifact));
 $('result').textContent=s.final||'请求正在进行。';checks('evaluation',s.evaluation?.checks||[]);$('expiry').textContent=s.expires&&!s.expires.startsWith('0001')?'数据保留至 '+new Date(s.expires).toLocaleString()+'，请提前下载模型和导出记录。':'';
}
// 技术检查和单次执行核验复用展示结构，但保留各自的证据与结论。
function checks(id,items){const target=$(id);target.replaceChildren();for(const c of items){const e=node('div',undefined,'check '+c.status);e.append(node('strong',`${c.name} · ${{passed:'通过',failed:'未通过',unverifiable:'无法验证',pending:'待核验',not_applicable:'不适用'}[c.status]||c.status}`),node('span',c.detail));target.append(e);}}
// valid 决定能否尝试预览；passed 决定是否满足技术上限，两者含义不同。
// 下载仍可用于检查失败产物；仅在 URL 变化时更新 src，避免重载模型和重置相机。
function showArtifact(a){const viewer=$('viewer');$('download').hidden=!a;$('empty-model').hidden=!!a?.report.valid;viewer.hidden=!a?.report.valid;if(!a){viewer.removeAttribute('src');checks('report',[]);return;}$('download').href=a.url+'?download=1';if(a.report.valid&&viewer.getAttribute('src')!==a.url)viewer.setAttribute('src',a.url);else if(!a.report.valid)viewer.removeAttribute('src');checks('report',a.report.checks);}
// 浏览器预览失败不覆盖服务端已经保存的技术报告。
$('viewer').addEventListener('error',()=>notice('3D 预览加载失败，可下载模型查看；技术检查报告保留。',true));
// 提交期间禁用按钮减少重复操作；是否接受动作仍由服务端持久状态决定。
$('request-form').onsubmit=async(e)=>{e.preventDefault();$('submit').disabled=true;try{const s=await api('/api/sessions',{request:$('request').value});if(config.mode!=='controlled-test')notice('');await openSession(s.id);}catch(e){showError(e);}finally{$('submit').disabled=!config?.ready;}};
$('answer-form').onsubmit=async(e)=>{e.preventDefault();const button=e.target.querySelector('button');button.disabled=true;try{await api(`/api/sessions/${current}/answer`,{answer:$('answer').value});$('answer').value='';}catch(e){showError(e);}finally{button.disabled=false;}};
$('stop').onclick=()=>api(`/api/sessions/${current}/stop`,{}).catch(showError);$('retry').onclick=()=>api(`/api/sessions/${current}/retry`,{}).catch(showError);
// 新建仅切换到需求输入界面；原会话后台执行继续，直到完成、停止或到期。
$('new-session').onclick=()=>{disconnect();current=null;history.replaceState(null,'',location.pathname);$('composer').hidden=false;$('workspace').hidden=true;$('title').textContent='让你的下一个道具成形';$('connection').textContent='准备就绪';list().catch(showError);};
// 初始化读取服务可用性与会话列表，再尝试恢复地址栏指定的会话。
try{config=await api('/api/config');if(!config.ready){notice(config.message);$('submit').disabled=true;}else if(config.mode==='controlled-test'){notice('当前为受控测试演示，未调用真实模型或 Tripo。');}await list();if(location.hash.startsWith('#session/'))await openSession(location.hash.slice(9));}catch(e){showError(e);}
