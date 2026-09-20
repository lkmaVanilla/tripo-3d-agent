import test from 'node:test';
import assert from 'node:assert/strict';
import {createWorkspace,mergeSnapshot,mergeMessages,sortedMessages,composerSlot,currentQuestion,canSubmit,changeDraft,startSubmission,submissionFailed,submissionAccepted,previewVersion,referenceVersion,parseRoute} from '../static/workspace-state.mjs';
import {ConversationConnection} from '../static/workspace-connection.mjs';
import {operationStage,operationError,intentReviewRows,intentFieldLabel} from '../static/workspace-chat.mjs';
import {fileURL} from '../static/workspace-api.mjs';

const snapshot=(id,cursor,active='',extra={})=>({conversation:{id,title:'木箱',active_run_id:active},messages:[],runs:[],versions:[],events:[],cursor,...extra});
const version=(id,number)=>({id,conversation_id:'c1',version_number:number,source_run_id:'r1',source_operation_id:id,processable:true,report:{valid:true,passed:true,triangles:4500,bytes:2048}});
const questionRun=(id,waitID,generation)=>({id,status:'awaiting_answer',question:'请明确用途',wait_id:waitID,generation});

test('unknown production shows backend evidence without becoming a retry action',()=>{
 const state=createWorkspace('c1'),data={stage:'submitting',status:'submission_unknown',diagnostic:{phase:'submit',category:'timeout',message:'提交 Tripo：请求超时。'}};
 const card={id:'operation-1',conversation_id:'c1',run_id:'r1',kind:'operation_card',seq:4,updated_seq:8,data};
 mergeSnapshot(state,snapshot('c1',8,'',{messages:[card],runs:[{id:'r1',status:'failed'}]}));
 mergeSnapshot(state,snapshot('c1',8,'',{messages:[card]}));
 assert.equal(state.messages.size,1);assert.equal(operationStage(data),'提交结果未知 · 未自动重试');
 assert.match(operationError(data),/请求超时/);assert.match(operationError(data),/无法确认/);assert.match(operationError(data),/没有自动重试/);
 assert.equal(state.draft.submission,null);
 assert.match(operationError({status:'submission_unknown'}),/没有记录更详细的调用诊断/);
 assert.match(operationError({status:'failed',error_summary:'历史执行失败。'}),/历史执行失败。没有记录/);
});
test('resolved intermediate errors cannot override delivery or a user stop',()=>{
 const prior={diagnostic:{message:'下载模型文件：网络连接失败。'},error_summary:'旧错误'};
 assert.equal(operationError({...prior,status:'checked',report:{passed:true}}),'');
 assert.equal(operationStage({...prior,report:{passed:true}}),'远端完成 · 技术检查通过');
 assert.equal(operationStage({stage:'submitted',run_status:'stopped'}),'本地执行已停止');
 assert.equal(operationError({stage:'done',status:'failed',diagnostic:prior.diagnostic,error_summary:'远端任务已成功；下载失败。'}),'远端任务已成功；下载失败。');
});

test('stopped Run retains busy gate until its active pointer is released',()=>{
 const state=createWorkspace('c1');changeDraft(state.draft,'下一次减面');
 mergeSnapshot(state,snapshot('c1',4,'r1',{runs:[{id:'r1',status:'stopped'}]}));
 assert.equal(canSubmit(state),false);
 state.stopPending=true;
 mergeSnapshot(state,snapshot('c1',5,''));
 assert.equal(state.stopPending,false);assert.equal(canSubmit(state),true);assert.equal(state.draft.text,'下一次减面');assert.equal(state.draft.submission,null);
});
test('question drafts belong to Run, WaitID and generation and cannot become new goals',()=>{
 const state=createWorkspace('c1');changeDraft(state.draft,'以后再减面');
 mergeSnapshot(state,snapshot('c1',1,'r1',{runs:[questionRun('r1','w1',1)]}));
 const first=composerSlot(state);assert.equal(first.kind,'answer');changeDraft(first.draft,'用于产品展示');
 state.composerMode='draft';assert.equal(composerSlot(state).draft.text,'以后再减面');assert.equal(canSubmit(state),false);
 mergeSnapshot(state,snapshot('c1',2,'r1',{runs:[questionRun('r1','w2',2)]}));
 assert.equal(composerSlot(state).draft.text,'');assert.equal(state.answers.get('r1:w1:1').text,'用于产品展示');
 mergeSnapshot(state,snapshot('c1',4,'r2',{runs:[questionRun('r2','w1',1)]}));
 assert.equal(composerSlot(state).key,'r2:w1:1');assert.equal(composerSlot(state).draft.text,'');assert.equal(state.draft.text,'以后再减面');
});
test('explicit input stays v1 while preview moves and a later version arrives',()=>{
 const state=createWorkspace('c1');mergeSnapshot(state,snapshot('c1',1,'',{versions:[version('v1',1),version('v2',2)]}));
 assert.equal(referenceVersion(state,'v1'),true);assert.equal(previewVersion(state,'v2'),true);
 assert.equal(state.draft.versionID,'v1');
 mergeSnapshot(state,snapshot('c1',2,'',{versions:[version('v3',3)]}));
 assert.equal(state.previewID,'v2');assert.equal(state.draft.versionID,'v1');
 assert.equal(referenceVersion(state,'other-conversation-version'),false);
 state.versions.set('missing',{...version('missing',4),processable:false});assert.equal(referenceVersion(state,'missing'),false);
});
test('uncertain submission preserves exact payload and identity even if current Run changed',()=>{
 const state=createWorkspace('c1');mergeSnapshot(state,snapshot('c1',1));changeDraft(state.draft,'将木箱减面','v1');
 const original=startSubmission(state.draft,{kind:'message',text:'将木箱减面',version_id:'v1'},()=> 'client-1');state.pendingSlot='draft';
 submissionFailed(state.draft,new Error('response lost'));
 mergeSnapshot(state,snapshot('c1',8,'r2',{runs:[questionRun('r2','w2',1)]}));
 assert.equal(composerSlot(state).key,'draft');assert.equal(canSubmit(state),true);
 assert.equal(changeDraft(state.draft,'换个需求','v2'),false);
 const repeated=startSubmission(state.draft,{kind:'message',text:'不能替换'},()=> 'client-2');assert.strictEqual(repeated,original);assert.equal(repeated.client_message_id,'client-1');
 submissionAccepted(state.draft);state.pendingSlot=null;assert.equal(composerSlot(state).kind,'answer');
});
test('definitive rejection retains submission for retry but permits deliberate edit',()=>{
 const state=createWorkspace('c1');changeDraft(state.draft,'原请求');const first=startSubmission(state.draft,{text:'原请求'},()=> 'one');
 submissionFailed(state.draft,{status:409});assert.equal(state.draft.uncertain,false);assert.strictEqual(startSubmission(state.draft,{text:'原请求'},()=> 'two'),first);
 submissionFailed(state.draft,{status:409});changeDraft(state.draft,'修改后的请求');assert.equal(startSubmission(state.draft,{text:'修改后的请求'},()=> 'three').client_message_id,'three');
});
test('overlapping pages and snapshots update one operation card and preserve cross-Run cursor gaps',()=>{
 const state=createWorkspace('c1');const card={id:'operation-1',conversation_id:'c1',run_id:'r1',kind:'operation_card',seq:10,updated_seq:15,data:{progress:24}};
 mergeSnapshot(state,snapshot('c1',15,'r1',{messages:[card]}));
 mergeSnapshot(state,snapshot('c1',42,'r2',{messages:[{...card,updated_seq:40,data:{progress:100}},{id:'next',conversation_id:'c1',run_id:'r2',seq:42,kind:'user'}]}));
 mergeMessages(state,[{...card,updated_seq:12,data:{progress:1}}]);
 mergeSnapshot(state,snapshot('c1',16,'r1',{messages:[card]}));
 assert.equal(state.cursor,42);assert.equal(state.conversation.active_run_id,'r2');assert.equal(sortedMessages(state).length,2);assert.equal(state.messages.get('operation-1').data.progress,100);
 assert.equal(mergeSnapshot(state,snapshot('foreign',90)),false);assert.equal(state.cursor,42);
});
test('Run answer is not an asset delivery and remote success is not technical pass',()=>{
 assert.equal(operationStage({stage:'submitted',status:'success'}),'远端完成 · 等待文件检查');
 assert.equal(operationStage({stage:'done',status:'checked',report:{passed:false}}),'远端完成 · 技术检查未通过');
 const state=createWorkspace('c1');mergeSnapshot(state,snapshot('c1',9,'',{versions:[version('v1',1)],runs:[{id:'explain',outcome:{kind:'answer'},production:0}]}));
 assert.equal(state.versions.size,1);assert.equal(state.runs.get('explain').production,0);
});
test('legacy routes resolve only IDs and file links cannot expose supplier URLs',()=>{
 assert.equal(parseRoute('#session/old-id'),'old-id');assert.equal(parseRoute('#conversation/new-id'),'new-id');assert.equal(parseRoute('#conversation/%xx'),null);
 assert.equal(fileURL({id:'v1',url:'https://supplier.invalid/model?token=secret'},'c1'),'/api/conversations/c1/versions/v1/file');
});
test('intent review preserves explicit before/after facts and precedes accepted plan without authorizing production',()=>{
 const draft={version_id:'v1',action:'decimate',changes:[{field:'max_triangles',before:4500,after:3000},{field:'max_bytes',before:1048576,after:524288},{field:'constraints',before:['保持木纹'],after:['保持木纹','<img src=x onerror=alert(1)>']}],inherited:['use','style']};
 const original=JSON.stringify(draft),rows=intentReviewRows(draft);
 assert.deepEqual(rows[0],{field:'三角面上限',before:'4,500',after:'3,000'});
 assert.deepEqual(rows[1],{field:'文件体积上限',before:'1,048,576 字节',after:'524,288 字节'});
 assert.equal(rows[2].after,'保持木纹；<img src=x onerror=alert(1)>');assert.equal(JSON.stringify(draft),original);
 assert.deepEqual(draft.inherited.map(intentFieldLabel),['用途','风格']);
 const state=createWorkspace('c1'),review={id:'draft-hash',conversation_id:'c1',run_id:'r2',kind:'intent_review',seq:11,updated_seq:11,data:draft};
 mergeSnapshot(state,snapshot('c1',11,'r2',{messages:[review],runs:[{id:'r2',status:'understanding',intent:null,production:0}]}));
 assert.equal(state.runs.get('r2').intent,null);assert.equal(state.runs.get('r2').production,0);assert.equal(state.versions.size,0);assert.equal(canSubmit(state),false);
 mergeSnapshot(state,snapshot('c1',12,'r2',{messages:[{id:'plan-r2',run_id:'r2',kind:'accepted_plan',seq:12,updated_seq:12},review]}));
 assert.deepEqual(sortedMessages(state).map(message=>message.kind),['intent_review','accepted_plan']);assert.equal(state.messages.size,2);
});

function connectionFixture(load){
 const sockets=[],seen=[],statuses=[],timers=new Map();let time=0,serial=0;
 const connection=new ConversationConnection({load,onSnapshot:value=>seen.push(value),onStatus:value=>statuses.push(value),onUnavailable:error=>statuses.push(error.status),origin:()=> 'http://example.test',createSocket:url=>{const socket={url,close(){this.closed=true;}};sockets.push(socket);return socket;},now:()=>time,setTimer:(fn,delay)=>{const id=++serial;timers.set(id,{fn,at:time+delay});return id;},clearTimer:id=>timers.delete(id)});
 async function advance(ms){time+=ms;for(const [id,timer] of [...timers])if(timer.at<=time){timers.delete(id);await timer.fn();}await Promise.resolve();}
 return {connection,sockets,seen,statuses,timers,advance,jump:ms=>{time+=ms;}};
}
test('late HTTP and WS from a previously selected conversation cannot affect current page',async()=>{
 let resolveFirst;const first=new Promise(resolve=>{resolveFirst=resolve;});
 const f=connectionFixture(id=>id==='c1'?first:Promise.resolve(snapshot('c2',20)));
 const pending=f.connection.open('c1');await f.connection.open('c2');resolveFirst(snapshot('c1',99));await pending;
 assert.deepEqual(f.seen.map(item=>item.conversation.id),['c2']);assert.equal(f.sockets.length,1);
 const oldMessage=f.sockets[0].onmessage;await f.connection.open('c3');oldMessage({data:JSON.stringify(snapshot('c2',200))});
 assert.deepEqual(f.seen.map(item=>item.conversation.id),['c2']);
});
test('reconnection uses the latest conversation cursor across multiple Runs',async()=>{
 let server=snapshot('c1',12,'r1');const f=connectionFixture(()=>Promise.resolve(server));await f.connection.open('c1');
 f.sockets[0].onmessage({data:JSON.stringify(snapshot('c1',28,'r2'))});server=snapshot('c1',35,'r2');f.sockets[0].onclose();await f.advance(1500);
 assert.equal(f.sockets[1].url,'ws://example.test/api/conversations/c1/events?after=35');assert.equal(f.connection.cursor,35);
});
test('expired conversation stops reconnect attempts',async()=>{
 const f=connectionFixture(()=>Promise.reject({status:404}));await f.connection.open('gone');assert.equal(f.connection.id,null);assert.equal(f.timers.size,0);assert.equal(f.statuses.at(-1),404);
});

// 空值是明确的无上限，缺失数据仍为未知，不能混淆。
test('optional constraint values retain their meaning',async()=>{
 const {intentValue}=await import('../static/workspace-chat.mjs');
 assert.equal(intentValue(null,'max_triangles'),'未设置验收上限');
 assert.equal(intentValue(undefined,'max_triangles'),'未记录');
 assert.equal(intentValue(3000,'max_triangles'),'3,000');
 assert.equal(intentValue('further','reduction_mode'),'进一步降低面数');
 assert.match(intentValue({max_bytes:{kind:'cleared'}},'constraint_sources'),/本次明确取消/);
});

const {workspaceActivity,submissionActivity,progressValue}=await import('../static/workspace-activity.mjs');
function activityFixture(status='running',op) {
 const state=createWorkspace('c1');mergeSnapshot(state,snapshot('c1',100,'r1',{runs:[{id:'r1',status,current_operation:op}]}));state.connection.phase='connected';return state;
}
const activityEvent=(state,seq,kind,data={},runID='r1',conversationID='c1')=>state.events.set(seq,{seq,kind,data,run_id:runID,conversation_id:conversationID});
test('activity stages use current operations and never invent progress',()=>{
 const cases=[['understanding',null,null,'thinking'],['awaiting_answer',null,null,'processing'],['queued',null,null,'queued'],['queue_full',null,null,'queue_full'],['running',{stage:'ready'},null,'preparing'],['running',{stage:'submitting'},null,'preparing'],['running',{stage:'submitted'},'queued','provider_queued'],['running',{stage:'submitted'},'running','generating'],['running',{stage:'submitted',kind:'decimate'},'running','decimating'],['running',{stage:'submitted'},'success','files'],['running',{stage:'submitted'},'alien','processing'],['running',null,null,'processing'],['alien',null,null,'processing']];
 for(const [status,extra,provider,expected] of cases){const op=extra&&{id:'op1',kind:'generate',task_id:'task1',...extra},state=activityFixture(status,op);if(provider)activityEvent(state,20,'tripo_progress',{operation_id:'op1',status:provider});assert.equal(workspaceActivity(state).kind,expected,JSON.stringify([status,extra,provider]));}
 for(const value of [null,undefined,'',NaN,Infinity,-1,101,'24'])assert.equal(progressValue(value),null);
 for(const value of [0,24,100])assert.equal(progressValue(value),value);
});
test('thinking closes on model result and historical failures cannot mask correction or current tool',()=>{
 const state=activityFixture('running',{id:'new',stage:'done'});
 activityEvent(state,2,'model_call');activityEvent(state,3,'agent_proposal');assert.equal(workspaceActivity(state).kind,'processing');
 activityEvent(state,9,'model_call');activityEvent(state,8,'technical_report',{operation_id:'new'});assert.equal(workspaceActivity(state).kind,'thinking');
 activityEvent(state,10,'model_call',{},'old');activityEvent(state,11,'model_error',{},'r1','foreign');activityEvent(state,7,'tool_failed',{operation_id:'old'});assert.equal(workspaceActivity(state).kind,'thinking');
 activityEvent(state,12,'model_error');assert.equal(workspaceActivity(state).kind,'processing');
 state.runs.get('r1').current_operation={id:'next',kind:'generate',stage:'submitted',task_id:'task-next'};
 activityEvent(state,13,'model_call'); // 滞后、未闭合的调用也不能覆盖正在生产的操作。
 state.messages.set('new',{id:'new',run_id:'r1',kind:'operation_card',seq:14,updated_seq:20,data:{operation_id:'next',status:'running'}});
 activityEvent(state,18,'tripo_progress',{operation_id:'next',status:'queued'});
 activityEvent(state,30,'tripo_progress',{operation_id:'old',status:'success'});
 assert.equal(workspaceActivity(state).kind,'provider_queued'); // 较晚卡片更新不等于较晚供应商状态。
 activityEvent(state,21,'tripo_progress',{operation_id:'next',status:'running'});assert.equal(workspaceActivity(state).kind,'generating');
 activityEvent(state,22,'tripo_progress',{operation_id:'next',status:'success'});assert.equal(workspaceActivity(state).kind,'files');
 activityEvent(state,22,'tripo_progress',{operation_id:'next',status:'success'});assert.equal(workspaceActivity(state).kind,'files');
 state.conversation.active_run_id='r2';state.runs.set('r2',{id:'r2',status:'running'});assert.equal(workspaceActivity(state).kind,'processing');
});
test('local sending and uncertainty precede old questions without changing gates, identities or drafts',()=>{
 const state=activityFixture();state.runs.set('r1',questionRun('r1','w1',1));state.answers.set('r1:w1:1',{text:'回答',submission:null,submitting:false,uncertain:false});
 assert.equal(workspaceActivity(state).kind,'awaiting_answer');
 const slot=composerSlot(state),payload=startSubmission(slot.draft,{text:'回答'},()=> 'stable');state.pendingSlot=slot.key;
 assert.equal(workspaceActivity(state).kind,'sending');submissionFailed(slot.draft,new Error('lost'));assert.equal(workspaceActivity(state).kind,'uncertain');assert.strictEqual(slot.draft.submission,payload);
 submissionFailed(slot.draft,{status:400});assert.equal(workspaceActivity(state).kind,'awaiting_answer');
 const before=JSON.stringify(state,(_,value)=>value instanceof Map?[...value]:value),allowed=canSubmit(state);workspaceActivity(state);assert.equal(JSON.stringify(state,(_,value)=>value instanceof Map?[...value]:value),before);assert.equal(canSubmit(state),allowed);
 assert.equal(submissionActivity({submitting:true}).kind,'sending');assert.equal(submissionActivity({uncertain:true}).kind,'uncertain');assert.equal(submissionActivity({}),null);
 state.stopPending=true;state.connection.phase='reconnecting';assert.equal(workspaceActivity(state).kind,'stopping');assert.equal(canSubmit(state),false);
 state.stopPending=false;assert.equal(workspaceActivity(state).kind,'reconnecting');state.connection.phase='syncing';assert.equal(workspaceActivity(state).kind,'syncing');
 for(const status of ['completed','answered','failed','stopped']){state.runs.get('r1').status=status;assert.equal(workspaceActivity(state).animated,false);assert.equal(canSubmit(state),false);}
 state.conversation.active_run_id='';assert.equal(workspaceActivity(state),null);state.unavailable=true;assert.equal(workspaceActivity(state),null);
});
test('socket open alone is not fresh and same-cursor snapshots keep feedback live',async()=>{
 const f=connectionFixture(async()=>snapshot('c1',12));await f.connection.open('c1');const socket=f.sockets[0];socket.onopen();assert.equal(f.connection.phase,'syncing');
 socket.onmessage({data:JSON.stringify(snapshot('c1',12))});assert.equal(f.connection.phase,'connected');
 await f.advance(14000);socket.onmessage({data:JSON.stringify(snapshot('c1',12))});assert.equal(f.connection.lastSnapshotAt,14000);
 await f.advance(14000);assert.equal(f.connection.phase,'connected');assert.equal(f.sockets.length,1);
 socket.onmessage({data:JSON.stringify(snapshot('foreign',100))});socket.onmessage({data:JSON.stringify(snapshot('c1',11))});assert.equal(f.connection.lastSnapshotAt,14000);
 await f.advance(1000);assert.equal(f.connection.phase,'reconnecting');assert.equal(f.timers.size,1);f.connection.checkFreshness();assert.equal(f.timers.size,1);
});
test('foreground staleness and socket identity fence recover once and restore terminal state',async()=>{
 let loads=0,server=snapshot('c1',12,'r1');const f=connectionFixture(async()=>{loads++;return server;});await f.connection.open('c1');
 const old=f.sockets[0],lateMessage=old.onmessage,lateClose=old.onclose;old.onmessage({data:JSON.stringify(server)});
 f.jump(15000);f.connection.checkFreshness();assert.equal(f.connection.phase,'reconnecting');assert.equal(f.timers.size,1);assert.equal(old.closed,true);
 server=snapshot('c1',20,'',{runs:[{id:'r1',status:'completed'}]});await f.advance(1500);assert.equal(loads,2);const current=f.sockets[1];
 lateMessage({data:JSON.stringify(snapshot('c1',99,'old'))});lateClose();assert.equal(f.connection.cursor,20);assert.equal(f.connection.socket,current);
 current.onmessage({data:JSON.stringify(server)});assert.equal(f.connection.phase,'connected');assert.equal(f.seen.at(-1).conversation.active_run_id,'');
 // 同代但非当前 socket 的回调也不允许覆盖页面。
 const handler=current.onmessage;f.connection.socket={};handler({data:JSON.stringify(snapshot('c1',100))});assert.equal(f.connection.cursor,20);f.connection.socket=current;
 f.connection.close();assert.equal(f.timers.size,0);assert.equal(current.closed,true);f.jump(30000);f.connection.checkFreshness();assert.equal(loads,2);
});
test('HTTP and socket staleness cannot create parallel recovery chains or refresh rejected snapshots',async()=>{
 let resolve;const f=connectionFixture(()=>new Promise(done=>{resolve=done;}));const pending=f.connection.open('c1');
 await f.advance(15000);assert.equal(f.connection.phase,'reconnecting');resolve(snapshot('c1',80));await pending;assert.equal(f.seen.length,0);assert.equal(f.sockets.length,0);
 f.connection.close();assert.equal(f.timers.size,0);
 const g=connectionFixture(async()=>snapshot('c1',1));await g.connection.open('c1');g.connection.onSnapshot=()=>false;await g.advance(100);g.sockets[0].onmessage({data:JSON.stringify(snapshot('c1',2))});assert.equal(g.connection.lastSnapshotAt,0);assert.equal(g.connection.cursor,1);
});

test('local running inherited by operation cards does not prove supplier progress',()=>{
 const state=activityFixture('running',{id:'op1',kind:'generate',stage:'submitted',task_id:'task1'});
 state.messages.set('card',{id:'card',run_id:'r1',kind:'operation_card',seq:10,data:{operation_id:'op1',status:'running'}});
 assert.equal(workspaceActivity(state).kind,'processing');
 activityEvent(state,11,'tripo_progress',{operation_id:'op1',status:'running'});assert.equal(workspaceActivity(state).kind,'generating');
});
