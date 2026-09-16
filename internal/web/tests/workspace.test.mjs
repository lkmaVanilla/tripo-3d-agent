import test from 'node:test';
import assert from 'node:assert/strict';
import {createWorkspace,mergeSnapshot,mergeMessages,sortedMessages,composerSlot,currentQuestion,canSubmit,changeDraft,startSubmission,submissionFailed,submissionAccepted,previewVersion,referenceVersion,parseRoute} from '../static/workspace-state.mjs';
import {ConversationConnection} from '../static/workspace-connection.mjs';
import {operationStage,intentReviewRows,intentFieldLabel} from '../static/workspace-chat.mjs';
import {fileURL} from '../static/workspace-api.mjs';

const snapshot=(id,cursor,active='',extra={})=>({conversation:{id,title:'木箱',active_run_id:active},messages:[],runs:[],versions:[],events:[],cursor,...extra});
const version=(id,number)=>({id,conversation_id:'c1',version_number:number,source_run_id:'r1',source_operation_id:id,processable:true,report:{valid:true,passed:true,triangles:4500,bytes:2048}});
const questionRun=(id,waitID,generation)=>({id,status:'awaiting_answer',question:'请明确用途',wait_id:waitID,generation});

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
 const sockets=[],seen=[],statuses=[],timers=[];
 const connection=new ConversationConnection({load,onSnapshot:value=>seen.push(value),onStatus:value=>statuses.push(value),onUnavailable:error=>statuses.push(error.status),origin:()=> 'http://example.test',createSocket:url=>{const socket={url,close(){this.closed=true;}};sockets.push(socket);return socket;},setTimer:fn=>{timers.push(fn);return timers.length;},clearTimer:()=>{}});
 return {connection,sockets,seen,statuses,timers};
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
 f.sockets[0].onmessage({data:JSON.stringify(snapshot('c1',28,'r2'))});server=snapshot('c1',35,'r2');f.sockets[0].onclose();await f.timers[0]();
 assert.equal(f.sockets[1].url,'ws://example.test/api/conversations/c1/events?after=35');assert.equal(f.connection.cursor,35);
});
test('expired conversation stops reconnect attempts',async()=>{
 const f=connectionFixture(()=>Promise.reject({status:404}));await f.connection.open('gone');assert.equal(f.connection.id,null);assert.equal(f.timers.length,0);assert.equal(f.statuses.at(-1),404);
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
