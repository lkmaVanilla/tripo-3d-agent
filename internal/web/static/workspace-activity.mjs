import {activeRun,composerSlot,currentQuestion} from './workspace-state.mjs';

const feedback=(kind,text,animated=false)=>({kind,text,animated});
const ended=new Set(['completed','answered','failed','stopped']);
const operationEvents=new Set(['runtime_accepted','input_preparing','input_prepared','tool_submitting','tool_submitted','tripo_progress','technical_report','tool_failed']);

// 缺失进度不是 0%；只展示服务端明确给出的合法数值。
export function progressValue(value) {
  return typeof value==='number' && Number.isFinite(value) && value>=0 && value<=100 ? value : null;
}
export function submissionActivity(draft) {
  if(draft?.submitting)return feedback('sending','正在发送…',true);
  if(draft?.uncertain)return feedback('uncertain','消息送达情况待确认');
  return null;
}

// 只解释当前会话的事实，不修改消息、发送门禁或运行状态。
export function workspaceActivity(state) {
  if(state.unavailable)return null;
  const sending=submissionActivity(composerSlot(state).draft);
  if(sending)return sending;
  if(state.refreshing||!state.loaded)return feedback('loading','正在读取创作进度…');
  const run=activeRun(state);
  if(state.stopPending||run?.status==='stopped')return feedback('stopping','正在停止本次执行…');
  if(!run)return state.conversation?.active_run_id?feedback('loading','正在同步创作进度…'):null;
  if(ended.has(run.status))return feedback('ending','正在结束本次执行…');
  if(state.connection?.phase!=='connected')return state.connection?.phase==='reconnecting'
    ?feedback('reconnecting','进度连接中断，正在恢复…'):feedback('syncing','正在同步创作进度…');
  if(currentQuestion(state))return feedback('awaiting_answer','等待你的回答');
  if(run.status==='awaiting_answer')return feedback('processing','正在同步当前问题…');
  if(run.status==='queued')return feedback('queued','排队中…',true);
  if(run.status==='queue_full')return feedback('queue_full','当前繁忙，请稍后重试');

  const op=run.current_operation;
  const events=[...state.events.values()].filter(event=>event.run_id===run.id && (!event.conversation_id||event.conversation_id===state.id)).sort((a,b)=>a.seq-b.seq);
  const currentEvents=op?events.filter(event=>event.data?.operation_id===op.id && operationEvents.has(event.kind)):[];
  // 正在执行的当前操作优先于滞后的模型调用事件；历史候选不能决定当前阶段。
  if(op?.stage==='ready'||op?.stage==='submitting')return feedback('preparing','正在准备模型制作…',true);
  if(op?.stage==='submitted') {
    const card=[...state.messages.values()].find(message=>message.run_id===run.id && message.kind==='operation_card' && message.data?.operation_id===op.id && (!message.conversation_id||message.conversation_id===state.id));
    const event=currentEvents.findLast(event=>event.kind==='tripo_progress');
    // 卡片的 queued/running 可能继承自本地入队，只有供应商事件能证明远端阶段。
    // success 不由本地队列产生；历史缺事件时可用它补充文件准备状态。
    const progress=event?.data || (card?.data.status==='success'?card.data:null);
    if(progress?.status==='success')return feedback('files','正在准备模型文件…',true);
    if(['queued','pending','waiting'].includes(progress?.status))return feedback('provider_queued','模型任务排队中…',true);
    if(op.task_id && ['running','processing'].includes(progress?.status)) {
      if(op.kind==='decimate')return feedback('decimating','正在优化模型…',true);
      if(op.kind==='generate')return feedback('generating','正在生成模型…',true);
    }
    return feedback('processing','正在处理模型任务…',true);
  }
  const modelStart=events.findLast(event=>event.kind==='model_call')?.seq||0;
  const modelEnd=events.findLast(event=>event.kind==='agent_proposal'||event.kind==='model_error')?.seq||0;
  if(modelStart>modelEnd && modelStart>(currentEvents.at(-1)?.seq||0))return feedback('thinking','思考中…',true);
  if(!op && run.status==='understanding')return feedback('thinking','思考中…',true);
  return feedback('processing','正在处理…',true);
}
