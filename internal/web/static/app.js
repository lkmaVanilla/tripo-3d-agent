import {$,node,fullTime} from './workspace-dom.mjs';
import {api,conversationPath} from './workspace-api.mjs';
import {ConversationConnection,connectionLabels} from './workspace-connection.mjs';
import {createWorkspace,newDraft,activeRun,currentQuestion,composerSlot,canSubmit,changeDraft,startSubmission,submissionFailed,submissionAccepted,mergeSnapshot,mergeMessages,sortedMessages,previewVersion,referenceVersion,parseRoute,statusLabels} from './workspace-state.mjs';
import {ChatView} from './workspace-chat.mjs';
import {VersionsView,versionName} from './workspace-versions.mjs';

// 草稿只保留在当前页面；不将匿名凭证、恢复状态或未发送内容写入持久存储。
const workspaces=new Map(),homeDraft=newDraft();
let current=null,config=null,navigationGeneration=0,listGeneration=0,historyLoading=false;
const currentState=()=>current?workspaces.get(current):null;
function notice(text,error=false){if(!text&&config&&!config.ready){text=config.message||'服务尚未配置生成凭证。';error=true;}else if(!text&&config?.mode?.startsWith('controlled-'))text='受控测试：仅使用测试模型和夹具，不代表真实 Tripo 成果。';$('notice').textContent=text;$('notice').hidden=!text;$('notice').className=error?'error':'';}
function showError(error){notice(error.message||'操作未完成。',true);}
function unavailable(error){const state=currentState();if(!state)return;state.unavailable=true;state.canSend=false;state.refreshing=false;$('connection').textContent='会话不可用';notice('会话不存在、已到期，或当前浏览器没有访问权限。已有输入仍保留在本页。',true);render();}
const connection=new ConversationConnection({
 load:id=>api(conversationPath(id)),
 onSnapshot:(snapshot,status)=>{
  const state=currentState();
  if(!state||snapshot.conversation?.id!==state.id||snapshot.cursor<state.cursor)return false;
  const wasStopping=state.stopPending||activeRun(state)?.status==='stopped';
  state.connection=status;
  if(mergeSnapshot(state,snapshot)){state.refreshing=false;if(wasStopping&&state.canSend)notice('本次本地执行已停止；已经提交的远端任务不一定会取消。');render();return true;}
  return false;
 },
 onStatus:status=>{
  const state=currentState();if(!state||status.conversationID!==state.id)return;
  const changed=state.connection.phase!==status.phase||state.connection.generation!==status.generation;
  state.connection=status;$('connection').textContent=connectionLabels[status.phase];if(changed)render();
 },onUnavailable:unavailable
});
const chat=new ChatView({container:$('messages'),scroll:$('chat-scroll'),activity:$('chat-activity'),onPreview:selectPreview});
const versions=new VersionsView({onPreview:selectPreview,onReference:selectReference});

function setPane(pane){const state=currentState();if(state)state.pane=pane;$('workspace-panes').dataset.pane=pane;$('tab-chat').setAttribute('aria-selected',String(pane==='chat'));$('tab-model').setAttribute('aria-selected',String(pane==='model'));}
function selectPreview(id){const state=currentState();if(state&&previewVersion(state,id)){versions.render(state);setPane('model');$('model-pane').scrollTop=0;}}
function selectReference(id){const state=currentState();if(!state)return;if(referenceVersion(state,id)){renderComposer(state);setPane('chat');$('message').focus();}else notice('这个版本当前不可引用，或上一条提交仍待确认。',true);}

async function listConversations(){
 const generation=++listGeneration;
 const conversations=await api('/api/conversations');
 if(generation!==listGeneration)return;
 $('sessions').replaceChildren();
 for(const conversation of conversations){const button=node('button',conversation.title,conversation.id===current?'active':'');button.title=conversation.title;button.dataset.conversationId=conversation.id;button.onclick=()=>openConversation(conversation.id).catch(showError);$('sessions').append(button);}
 if(!conversations.length)$('sessions').append(node('p','还没有资产会话','muted'));
}
function markListSelection(){for(const button of $('sessions').querySelectorAll('button'))button.classList.toggle('active',button.dataset.conversationId===current);}
function showHome(){navigationGeneration++;connection.close();chat.clearActivity();current=null;history.replaceState(null,'',location.pathname+location.search);$('home').hidden=false;$('workspace').hidden=true;$('title').textContent='从想法开始创作';$('page-eyebrow').textContent='3D CREATION';$('connection').textContent='准备就绪';notice('');renderHome();markListSelection();}
async function openConversation(id,initial){
 navigationGeneration++;current=id;
 if(!workspaces.has(id))workspaces.set(id,createWorkspace(id));
 const state=workspaces.get(id);state.refreshing=true;state.unavailable=false;
 history.replaceState(null,'','#conversation/'+encodeURIComponent(id));
 $('home').hidden=true;$('workspace').hidden=false;notice('');
 if(initial){mergeSnapshot(state,initial);state.refreshing=false;}
 render();markListSelection();
 await connection.open(id,initial);
}
function renderHome(){
 if($('request').value!==homeDraft.text)$('request').value=homeDraft.text;$('request').readOnly=homeDraft.submitting||homeDraft.uncertain;
 $('submit').disabled=homeDraft.submitting||!config?.ready||!homeDraft.text.trim();
 $('submit').textContent=homeDraft.submitting?'正在发送…':homeDraft.uncertain?'确认原提交':'开始创作 ↑';
 $('home-hint').textContent=homeDraft.submitting?'正在发送…':homeDraft.uncertain?'尚不能确认是否已经接受。请保持原文，用相同提交身份确认。':'一个会话围绕一个资产，保留每次制作的历史版本。';
}
function render(options){
 const state=currentState();if(!state)return;
 const run=activeRun(state),stopping=state.stopPending||(run?.status==='stopped'&&!!state.conversation?.active_run_id);
 $('title').textContent=state.conversation?.title||'正在读取资产会话…';$('page-eyebrow').textContent='ASSET WORKSPACE';
 $('status').textContent=state.unavailable?'会话不可用':state.refreshing?'正在读取':stopping?'正在停止':run?(statusLabels[run.status]||'正在处理'):'可以继续创作';
 $('budget').textContent=run?`本次：生产 ${run.production??0}/${run.max_submissions??3} · 模型调用 ${run.model_calls??0}/${run.max_model_calls??20}`:'同一资产 · 每次要求独立记录';
 $('stop').hidden=!run||state.unavailable||['completed','failed','stopped','answered'].includes(run.status);$('stop').disabled=state.stopPending;
 $('retry').hidden=run?.status!=='queue_full'||state.unavailable||stopping;
 $('export').href=conversationPath(state.id)+'/trace';$('export').hidden=state.unavailable;
 $('empty-chat').hidden=state.messages.size>0;$('empty-chat').textContent=state.unavailable?'当前无法读取会话记录。':state.loaded?'尚无可展示的历史消息。':'正在读取创作记录…';
 $('load-history').hidden=!state.hasMore||state.unavailable;$('load-history').disabled=historyLoading;
 chat.render(state,options);versions.render(state);renderComposer(state);setPane(state.pane);
 const expires=fullTime(state.conversation?.expires);
 $('expiry').textContent=state.conversation?.active_run_id?'制作期间保留本会话的历史版本；最后一次执行结束后保留 7 天。':expires?`保留至 ${expires}。查看、下载和草稿不会续期。`:'';
}
function renderComposer(state){
 const slot=composerSlot(state),draft=slot.draft,question=currentQuestion(state);
 $('composer-modes').hidden=!question||draft.uncertain||draft.submitting;
 $('answer-mode').classList.toggle('active',slot.kind==='answer');$('draft-mode').classList.toggle('active',slot.kind==='message');
 $('question-context').hidden=slot.kind!=='answer'||!question;$('question-context').textContent=question?'正在回答：'+question.text:'';
 if($('message').value!==draft.text)$('message').value=draft.text;$('message').readOnly=draft.submitting||draft.uncertain;
 $('message-label').textContent=slot.kind==='answer'?'回答当前问题':'后续资产要求';
 $('message').placeholder=slot.kind==='answer'?'回答这个问题，继续本次制作…':state.canSend?'继续描述这件资产的下一步…':'可以先记下下一条要求，当前执行结束后再发送…';
 const version=state.versions.get(draft.versionID);
 $('input-reference').hidden=!draft.versionID;$('reference-label').textContent='本条引用：'+versionName(version);$('remove-reference').disabled=draft.submitting||draft.uncertain;
 $('send').disabled=state.refreshing||!canSubmit(state,slot)||!config?.ready;
 $('send').textContent=draft.submitting?'正在发送…':draft.uncertain?'确认原提交':slot.kind==='answer'?'发送回答 ↑':'发送 ↑';
 $('composer-hint').textContent=draft.uncertain?'接受情况待确认；重试沿用原内容和提交身份。':state.unavailable?'会话不可用，草稿仍保留。':state.stopPending||activeRun(state)?.status==='stopped'?'正在停止，确认执行退出后才能发送。':slot.kind==='answer'?'回答属于当前执行，沿用原预算。':!state.canSend?'草稿尚未发送，执行结束后需要手动发送。':draft.versionID?'明确引用此版本；预览切换不会改变输入。':'需要加工已有模型时，请先从版本列表引用。';
}
function validateText(text){if(!text.trim())throw new Error('请输入你的要求。');if(new TextEncoder().encode(text.trim()).length>8000)throw new Error('输入超过 8,000 字节，请缩短后发送。');}
$('request').addEventListener('input',()=>{changeDraft(homeDraft,$('request').value);renderHome();});
$('request-form').onsubmit=async event=>{
 event.preventDefault();if(homeDraft.submitting||!config?.ready)return;
 try{validateText(homeDraft.text);}catch(error){showError(error);return;}
 const generation=navigationGeneration,payload=startSubmission(homeDraft,{request:homeDraft.text.trim()});renderHome();
 let snapshot;
 try{snapshot=await api('/api/conversations',payload);if(!snapshot.conversation?.id)throw new Error('回复中缺少会话身份，请使用原提交重试。');}
 catch(error){submissionFailed(homeDraft,error);if(generation===navigationGeneration)showError(error);renderHome();return;}
 submissionAccepted(homeDraft);renderHome();
 if(generation===navigationGeneration&&!current)await openConversation(snapshot.conversation.id,snapshot);
 listConversations().catch(showError);
};
$('message').addEventListener('input',()=>{const state=currentState();if(!state)return;changeDraft(composerSlot(state).draft,$('message').value);renderComposer(state);});
$('message-form').onsubmit=async event=>{
 event.preventDefault();const state=currentState();if(!state||state.refreshing||!config?.ready)return;
 const slot=composerSlot(state);if(!canSubmit(state,slot))return;
 try{validateText(slot.draft.text);}catch(error){showError(error);return;}
 const payload={kind:slot.kind,text:slot.draft.text.trim()};
 if(slot.draft.versionID)payload.version_id=slot.draft.versionID;
 if(slot.kind==='answer'&&!slot.draft.uncertain){const question=currentQuestion(state);if(!question||question.key!==slot.key)return;Object.assign(payload,{run_id:question.runID,wait_id:question.waitID,generation:question.generation});}
 const submission=startSubmission(slot.draft,payload);state.pendingSlot=slot.key;render();notice('');
 try{const snapshot=await api(conversationPath(state.id)+'/messages',submission);if(snapshot.conversation?.id!==state.id)throw new Error('回复中缺少对应会话，请使用原提交重试。');submissionAccepted(slot.draft);state.pendingSlot=null;mergeSnapshot(state,snapshot);if(current===state.id){if(slot.kind==='answer'&&currentQuestion(state)?.key===slot.key)state.composerMode='draft';render();}listConversations().catch(showError);}
 catch(error){submissionFailed(slot.draft,error);if(!slot.draft.uncertain)state.pendingSlot=null;if(current===state.id){showError(error);render();}}
};
$('answer-mode').onclick=()=>{const state=currentState();if(state){state.composerMode='answer';renderComposer(state);}};
$('draft-mode').onclick=()=>{const state=currentState();if(state){state.composerMode='draft';renderComposer(state);}};
$('remove-reference').onclick=()=>{const state=currentState();if(state){const draft=composerSlot(state).draft;changeDraft(draft,draft.text,'');renderComposer(state);}};
$('stop').onclick=async()=>{
 const state=currentState(),run=state&&activeRun(state);if(!run||state.stopPending)return;
 state.stopPending=true;render();notice('正在停止本次本地执行。已经提交的远端任务不一定会取消。');
 try{const snapshot=await api(conversationPath(state.id)+'/runs/'+encodeURIComponent(run.id)+'/stop',{});mergeSnapshot(state,snapshot);if(current===state.id){if(state.canSend)notice('本次本地执行已停止；已经提交的远端任务不一定会取消。');render();}}
 catch(error){if(error.status&&error.status<500)state.stopPending=false;if(current===state.id){showError(error);render();}}
};
$('retry').onclick=async()=>{
 const state=currentState(),run=state&&activeRun(state);if(!run||run.status!=='queue_full')return;
 $('retry').disabled=true;
 try{const snapshot=await api(conversationPath(state.id)+'/runs/'+encodeURIComponent(run.id)+'/retry',{});mergeSnapshot(state,snapshot);if(current===state.id)render();}
 catch(error){if(current===state.id)showError(error);}finally{$('retry').disabled=false;}
};
$('load-history').onclick=async()=>{
 const state=currentState();if(!state||historyLoading)return;
 const before=sortedMessages(state)[0]?.seq;if(!before)return;historyLoading=true;$('load-history').disabled=true;
 try{const page=await api(conversationPath(state.id)+'/messages?before='+before);mergeMessages(state,page.messages||[]);state.hasMore=!!page.has_more;if(current===state.id)render({history:true});}
 catch(error){if(current===state.id)showError(error);}finally{historyLoading=false;$('load-history').disabled=false;}
};
$('new-session').onclick=showHome;
$('tab-chat').onclick=()=>setPane('chat');$('tab-model').onclick=()=>setPane('model');
$('viewer').addEventListener('error',()=>notice('此版本的 3D 预览未能加载，可下载文件查看；服务器保存的技术报告不因此改变。',true));
document.addEventListener('visibilitychange',()=>{if(document.visibilityState==='visible')connection.checkFreshness();});
window.addEventListener('pagehide',()=>connection.close());
window.addEventListener('pageshow',event=>{if(event.persisted&&current)openConversation(current).catch(showError);});
window.addEventListener('hashchange',()=>{const id=parseRoute(location.hash);if(id&&id!==current)openConversation(id).catch(showError);else if(!id)showHome();else if(location.hash.startsWith('#session/'))history.replaceState(null,'','#conversation/'+encodeURIComponent(id));});

try{config=await api('/api/config');if(!config.ready)notice(config.message||'服务尚未配置生成凭证。',true);else if(config.mode?.startsWith('controlled-'))notice('');renderHome();await listConversations();const id=parseRoute(location.hash);if(id)await openConversation(id);}catch(error){showError(error);renderHome();}
