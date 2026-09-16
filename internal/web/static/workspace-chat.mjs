import {node,shortTime,checks} from './workspace-dom.mjs';
import {sortedMessages,statusLabels} from './workspace-state.mjs';
import {versionName,technicalSummary} from './workspace-versions.mjs';

const messageLabels={user:'你',clarification:'Agent · 需要补充',intent_review:'需求变化 · 保存前草案',accepted_plan:'Agent · 已保存的计划',operation_card:'制作记录',answer:'Agent 回答',run_result:'本次执行结果'};
const operationNames={generate:'生成模型',regenerate:'重新生成模型',decimate:'模型减面'};
const intentFields={asset:'资产主体',use:'用途',style:'风格',constraints:'硬约束',max_triangles:'三角面上限',max_bytes:'文件体积上限',assumptions:'默认假设',plan:'制作步骤',action:'制作动作',constraint_sources:'约束来源',reduction_mode:'减面目标',constraint_version:'约束规则版本'};
export const intentFieldLabel=field=>intentFields[field]||field;
export function intentValue(value,field){
  if(value===null&&(field==='max_triangles'||field==='max_bytes'))return '未设置验收上限';
  if(value===null||value===undefined||value===''||(Array.isArray(value)&&!value.length))return '未记录';
  if(Array.isArray(value))return value.map(item=>typeof item==='string'?item:JSON.stringify(item)).join('；');
  if(field==='reduction_mode')return value==='further'?'进一步降低面数':value==='within_limit'?'达到指定上限':'未记录';
  if(field==='constraint_sources'&&typeof value==='object'){const labels={user_request:'本次要求',inherited:'继承所选版本',legacy_inherited:'继承历史验收约束',cleared:'本次明确取消',unset:'未设置'};return Object.entries(value).map(([key,source])=>`${intentFieldLabel(key)}：${labels[source.kind]||'来源未知'}`).join('；');}
  if(typeof value==='number')return value.toLocaleString('zh-CN')+(field==='max_bytes'?' 字节':'');
  return typeof value==='string'?value:JSON.stringify(value);
}
// 只排版后端保存的 before/after，不在浏览器推导约束继承或生产授权。
export function intentReviewRows(draft){return (draft.changes||[]).map(change=>({field:intentFieldLabel(change.field),before:intentValue(change.before,change.field),after:intentValue(change.after,change.field)}));}
export function operationStage(data) {
  if(data.report)return data.report.passed?'远端完成 · 技术检查通过':'远端完成 · 技术检查未通过';
  if(data.status==='success')return '远端完成 · 等待文件检查';
  if(data.status==='failed')return '本次操作失败';
  if(data.stage==='submitting')return '正在提交远端任务';
  if(data.stage==='submitted')return data.status==='queued'?'远端排队中':'远端处理中';
  if(data.status==='queue_full')return '等待队列已满';
  if(data.status==='queued')return '等待制作名额';
  return '准备制作';
}
function details(title,value,explanation='') {
  const element=node('details',undefined,'evidence');element.append(node('summary',title));
  if(explanation)element.append(node('p',explanation,'answer-warning'));
  element.append(node('pre',JSON.stringify(value,null,2)));return element;
}

// 按持久消息身份复用 DOM，轮询只更新对应操作卡，不追加重复聊天行。
export class ChatView {
  constructor({container,scroll,onPreview}) {Object.assign(this,{container,scroll,onPreview});this.items=new Map();this.conversationID='';}
  render(state,{history=false}={}) {
    if(this.conversationID!==state.id){this.items.clear();this.container.replaceChildren();this.conversationID=state.id;}
    const nearBottom=this.scroll.scrollHeight-this.scroll.scrollTop-this.scroll.clientHeight<100;
    const oldHeight=this.scroll.scrollHeight;
    const ordered=sortedMessages(state);
    const children=[];
    for(const message of ordered) {
      const run=state.runs.get(message.run_id)||{};
      const evidence=[...state.events.values()].filter(event=>event.run_id===message.run_id).sort((a,b)=>a.seq-b.seq);
      const isResult=message.kind==='run_result'||message.kind==='answer';
      const signature=JSON.stringify([message,isResult?run:null,isResult?evidence.length:0,isResult?evidence.at(-1)?.seq:0,[...state.versions.values()].map(version=>[version.id,version.version_number])]);
      let entry=this.items.get(message.id);
      if(!entry){entry={element:node('article',undefined,'message'+(message.kind==='user'?' user':''))};entry.element.dataset.messageId=message.id;entry.element.dataset.runId=message.run_id;this.items.set(message.id,entry);}
      if(entry.signature!==signature) {
        const openDetails=[...entry.element.querySelectorAll('details')].map(item=>item.open);
        entry.element.replaceChildren(...this.contents(state,message,run,evidence));
        [...entry.element.querySelectorAll('details')].forEach((item,index)=>{item.open=!!openDetails[index];});
        entry.signature=signature;
      }
      children.push(entry.element);
    }
    // 不移动顺序已经正确的节点，以免轮询夺走证据展开项的键盘焦点。
    children.forEach((element,index)=>{if(this.container.children[index]!==element)this.container.insertBefore(element,this.container.children[index]||null);});
    if(history)this.scroll.scrollTop+=this.scroll.scrollHeight-oldHeight;
    else if(nearBottom)this.scroll.scrollTop=this.scroll.scrollHeight;
  }
  contents(state,message,run,evidence) {
    const label=message.kind==='user'&&message.wait_id?'你 · 澄清回答':messageLabels[message.kind]||'历史记录';
    const header=node('div',undefined,'message-header');header.append(node('span',label),node('time',shortTime(message.created)));
    const content=node('div',undefined,['operation_card','intent_review','accepted_plan','run_result'].includes(message.kind)?'message-card':'message-content');
    const data=message.data||{};
    if(message.kind==='operation_card')this.operation(content,state,message);
    else if(message.kind==='intent_review')this.intentReview(content,state,message);
    else if(message.kind==='accepted_plan') {
      const intent=data.intent||{};
      if(intent.asset)content.append(node('h3',intent.asset));
      if(intent.use)content.append(node('p','用途：'+intent.use));
      if(intent.constraint_version==='optional-v1'){for(const field of ['max_triangles','max_bytes'])content.append(node('p',`${intentFieldLabel(field)}：${intentValue(intent[field],field)}`));}
      if(intent.plan?.length){const list=node('ol');for(const step of intent.plan)list.append(node('li',step));content.append(list);}
      if(message.text)content.append(node('p',message.text));
      content.append(details('需求、约束与本次验收上限',intent,'计划是 Agent 提议并已保存，尚不代表制作完成。'));
    } else if(message.kind==='run_result') {
      content.append(node('h3',run.outcome?.kind==='delivery'?'已交付本次模型':statusLabels[run.status]||'本次执行已结束'),node('p',message.text||run.final||'结果记录中没有可展示的正文。'));
      // 新资产交付只链接所属 Run 的实际输出，不把输入版本显示成新交付。
      const delivered=[...state.versions.values()].find(version=>version.source_run_id===message.run_id && (version.id===run.selected_artifact || version.source_operation_id===run.selected_artifact));
      if(delivered)this.versionButton(content,delivered,'查看本次交付 '+versionName(delivered));
      this.runEvidence(content,run,evidence);
    } else if(message.kind==='answer') {
      content.append(node('div',message.text||run.outcome?.text||'', 'message-content'),node('p','Agent 的说明与建议；技术事实以版本报告和下方核验记录为准。','answer-warning'));
      if(run.final)content.append(details('本次交互的程序事实',run.final));
      this.runEvidence(content,run,evidence);
    } else content.textContent=message.text||'';
    if(message.version_id && message.kind!=='operation_card') {
      const version=state.versions.get(message.version_id);
      const reference=node('button',`引用 ${versionName(version)}`,'message-reference');reference.type='button';reference.disabled=!version;reference.onclick=()=>this.onPreview(version.id);content.append(reference);
    }
    return [header,content];
  }
  intentReview(content,state,message){
    const draft=message.data||{},source=state.versions.get(draft.version_id);
    content.classList.add('intent-review');
    content.append(node('h3','拟采用的需求'),node('p',message.text||'这是正式保存之前的需求草案，展示时尚未接受。'));
    const context=node('p',`来源：${versionName(source)} · 本次动作：${operationNames[draft.action]||'待明确'}`,'muted');content.append(context);
    const rows=intentReviewRows(draft);
    if(rows.length){
      const table=node('table',undefined,'intent-diff'),head=node('thead'),header=node('tr');
      for(const title of ['要求','来源版本','本次拟采用']){const cell=node('th',title);cell.scope='col';header.append(cell);}head.append(header);table.append(head);
      const body=node('tbody');for(const row of rows){const item=node('tr'),field=node('th',row.field);field.scope='row';item.append(field,node('td',row.before),node('td',row.after));body.append(item);}table.append(body);content.append(table);
    }else content.append(node('p','草案未记录与来源版本的字段差异。'));
    if(draft.inherited?.length)content.append(node('p','保持的要求：'+draft.inherited.map(intentFieldLabel).join('、'),'intent-inherited'));
    content.append(node('p','是否已接受，以后续“已保存的计划”为准。此草案不代表制作已经开始。','answer-warning'),details('查看完整需求草案',draft.intent||{}));
  }
  operation(content,state,message) {
    const data=message.data||{};
    const meta=node('div',undefined,'card-meta');meta.append(node('span',operationNames[data.operation_kind]||'模型制作'),node('span',operationStage(data)));content.append(meta);
    if(data.stage==='submitted'||data.report) {
      const progress=node('progress');progress.max=100;
      if(Number.isFinite(Number(data.progress)))progress.value=Math.min(100,Math.max(0,Number(data.progress)));
      else if(data.report)progress.value=100;
      content.append(progress);
      if(data.progress!==undefined)content.append(node('div',`远端进度 ${data.progress}%`,'operation-facts'));
    }
    if(data.task_id)content.append(node('div','远端任务：'+data.task_id,'operation-facts'));
    if(data.report)content.append(node('div',technicalSummary(data.report),'report-summary'));
    const version=state.versions.get(message.version_id);
    if(version)this.versionButton(content,version,'查看 '+versionName(version));
    content.append(details('查看操作证据',data,'供应商状态与本次技术检查分别记录。'));
  }
  versionButton(content,version,label) {
    const button=node('button',label,'result-version');button.type='button';button.onclick=()=>this.onPreview(version.id);content.append(button);
  }
  runEvidence(content,run,evidence) {
    const footer=node('div',undefined,'run-footer');
    footer.append(node('span',`生产 ${run.production??'—'} / ${run.max_submissions??'—'}`),node('span',`模型调用 ${run.model_calls??'—'} / ${run.max_model_calls??'—'}`));
    if(run.id){const link=node('a','导出本次记录');link.href='/api/sessions/'+encodeURIComponent(run.id)+'/trace';footer.append(link);}
    content.append(footer);
    if(run.evaluation?.checks?.length) {
      const audit=node('details',undefined,'evidence');audit.append(node('summary','本次执行的程序核验'));const report=node('div');checks(report,run.evaluation.checks);audit.append(report,node('p','仅核验本次执行规则，不代表独立 Agent 案例集评测通过。','answer-warning'));content.append(audit);
    }
    content.append(details('展开执行证据',{run_id:run.id,created:run.created,ended:run.ended,deadline:run.deadline,model:run.model,prompt_version:run.prompt_version,result:run.result,outcome:run.outcome,input_assessment:run.input_assessment,events:evidence},'原始 Agent 提议、工具内容保留各自来源，不等同于已经核验的结论。'));
  }
}
