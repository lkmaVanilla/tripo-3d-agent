// 页面状态不推演执行状态机：可发送状态始终以会话活动指针为准。
export const statusLabels = {understanding:'理解需求中',awaiting_answer:'等待你的回答',queued:'等待制作名额',queue_full:'队列繁忙',running:'正在制作',completed:'本次制作已完成',failed:'本次执行已结束',stopped:'本次执行已停止'};

export function newDraft() { return {text:'',versionID:'',submission:null,submitting:false,uncertain:false}; }
export function createWorkspace(id) {
  return {id,conversation:null,runs:new Map(),messages:new Map(),versions:new Map(),events:new Map(),cursor:0,hasMore:false,loaded:false,unavailable:false,canSend:false,
    connection:{phase:'syncing',generation:0,lastSnapshotAt:null},draft:newDraft(),answers:new Map(),composerMode:'auto',questionKey:'',pendingSlot:null,previewID:'',manualPreview:false,pane:'chat',stopPending:false};
}
export function activeRun(state) { return state.runs.get(state.conversation?.active_run_id) || null; }
export function currentQuestion(state) {
  const run=activeRun(state);
  if(!run || run.status!=='awaiting_answer' || !run.wait_id || !run.generation || state.stopPending) return null;
  return {runID:run.id,waitID:run.wait_id,generation:run.generation,text:run.question||'',key:`${run.id}:${run.wait_id}:${run.generation}`};
}
export function sortedMessages(state) { return [...state.messages.values()].sort((a,b)=>a.seq-b.seq || a.id.localeCompare(b.id)); }
export function sortedVersions(state) { return [...state.versions.values()].sort((a,b)=>a.version_number-b.version_number); }

// 分页与实时快照可以重叠；同一张操作卡只接受更晚的持久更新。
export function mergeMessages(state,messages=[]) {
  for(const message of messages) {
    if(message.conversation_id && message.conversation_id!==state.id) continue;
    const prior=state.messages.get(message.id);
    if(!prior || (message.updated_seq||message.seq)>=(prior.updated_seq||prior.seq)) state.messages.set(message.id,message);
  }
}
export function mergeSnapshot(state,snapshot) {
  if(snapshot.conversation?.id!==state.id) return false;
  const fresh=Number(snapshot.cursor||0)>=state.cursor;
  mergeMessages(state,snapshot.messages||[]);
  if(fresh) {
    state.conversation=snapshot.conversation;
    state.canSend=!snapshot.conversation.active_run_id && snapshot.can_send!==false;
    for(const run of snapshot.runs||[]) state.runs.set(run.id,run);
    for(const version of snapshot.versions||[]) {
      if(version.conversation_id && version.conversation_id!==state.id) continue;
      state.versions.set(version.id,version);
    }
    // 初次快照的历史标记不会因 WS 的最近窗口而反复打开已读分页。
    if(!state.loaded) state.hasMore=!!snapshot.has_more;
    if(state.canSend) state.stopPending=false;
  }
  for(const event of snapshot.events||[]) {
    if(event.conversation_id && event.conversation_id!==state.id) continue;
    state.events.set(event.seq,event);
  }
  state.cursor=Math.max(state.cursor,Number(snapshot.cursor||0));
  state.loaded=true;
  state.unavailable=false;
  const question=currentQuestion(state);
  if(question && question.key!==state.questionKey) {
    state.questionKey=question.key;
    if(!state.answers.has(question.key)) state.answers.set(question.key,newDraft());
    state.composerMode='answer';
  } else if(!question) state.composerMode='draft';
  if(!state.manualPreview) {
    const versions=sortedVersions(state);
    // 默认保留最近正式交付；失败候选可手动查看，不替换已有合格模型。
    const delivered=[...state.runs.values()].filter(run=>run.selected_artifact).map(run=>run.selected_artifact);
    const choice=versions.filter(version=>delivered.includes(version.source_operation_id)||delivered.includes(version.id)).at(-1);
    state.previewID=(choice||versions.at(-1))?.id||'';
  }
  return true;
}
export function composerSlot(state) {
  if(state.pendingSlot) {
    const pending=state.pendingSlot==='draft'?state.draft:state.answers.get(state.pendingSlot);
    if(pending && (pending.uncertain||pending.submitting)) return {key:state.pendingSlot,draft:pending,kind:state.pendingSlot==='draft'?'message':'answer'};
  }
  const question=currentQuestion(state);
  if(question && state.composerMode!=='draft') return {key:question.key,draft:state.answers.get(question.key),kind:'answer',question};
  return {key:'draft',draft:state.draft,kind:'message'};
}
export function canSubmit(state,slot=composerSlot(state)) {
  if(!state.loaded || state.unavailable || slot.draft.submitting || !slot.draft.text.trim()) return false;
  if(slot.draft.uncertain) return true; // 仅重试冻结身份，不新增目标。
  if(state.stopPending) return false;
  return slot.kind==='answer' ? !!currentQuestion(state) && currentQuestion(state).key===slot.key : state.canSend;
}
export function changeDraft(draft,text,versionID=draft.versionID) {
  if(draft.uncertain||draft.submitting) return false;
  if(draft.text!==text || draft.versionID!==versionID) draft.submission=null;
  draft.text=text; draft.versionID=versionID;
  return true;
}
export function startSubmission(draft,payload,makeID=()=>crypto.randomUUID()) {
  if(!draft.submission) draft.submission=Object.freeze({...payload,client_message_id:makeID()});
  draft.submitting=true;
  return draft.submission;
}
export function submissionFailed(draft,error) {
  draft.submitting=false;
  // 网络/服务异常可能发生在接受之后，只有确定拒绝才允许编辑为另一目标。
  draft.uncertain=!error?.status || error.status>=500;
}
export function submissionAccepted(draft) { Object.assign(draft,newDraft()); }
export function previewVersion(state,id) { if(state.versions.has(id)){state.previewID=id;state.manualPreview=true;return true;}return false; }
export function referenceVersion(state,id) {
  const version=state.versions.get(id),slot=composerSlot(state);
  if(!version || version.processable!==true || state.unavailable) return false;
  return changeDraft(slot.draft,slot.draft.text,id);
}
export function parseRoute(hash) {
  const match=/^#(?:conversation|session)\/([^/?#]+)$/.exec(hash);
  if(!match) return null;
  try{return decodeURIComponent(match[1]);}catch{return null;}
}
