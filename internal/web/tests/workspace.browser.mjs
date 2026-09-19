// 可选前端契约验收：HTTP/WS 全部由本文件拦截，不调用 Go/模型/Tripo。
// 执行：NODE_PATH=$(npm root -g) node internal/web/tests/workspace.browser.mjs
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {readFile,mkdtemp,writeFile} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {resolve,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
const require=createRequire(import.meta.url);
const {chromium}=require('playwright');
const {expect}=require('playwright/test');
const staticDir=resolve(dirname(fileURLToPath(import.meta.url)),'../static');
const output=await mkdtemp(resolve(tmpdir(),'tripo-workspace-browser-'));
const base='http://localhost:48991';
const date='2026-09-13T12:00:00Z',expiry='2026-09-20T12:00:00Z';
let cursor=0,runNumber=0,abortCreation=true;
const conversations=new Map(),commands=[],creationKeys=new Map(),messageKeys=new Map(),sockets=new Set(),pageErrors=[];
const screenshots=[],checks=[];
let postGate=null,deferAnswer=false,pauseSnapshots=false;
function holdPost(){let release;const wait=new Promise(done=>{release=done;});postGate=wait;return release;}
function event(state,kind,data={},runID=state.conversation.active_run_id){const seq=tick();state.events.push({seq,conversation_id:state.conversation.id,run_id:runID,kind,data});state.cursor=seq;}
const tick=()=>++cursor;
function envelope(id,title='低模木箱') {return {conversation:{id,title,created:date,updated:date,expires:expiry,active_run_id:''},messages:[],versions:[],runs:[],events:[],cursor:tick(),has_more:false};}
function addMessage(state,kind,text,runID,data={},versionID='',waitID='') {
 const seq=tick();const message={id:'m'+seq,conversation_id:state.conversation.id,run_id:runID,kind,text,seq,updated_seq:seq,created:date,data,version_id:versionID,wait_id:waitID};state.messages.push(message);state.cursor=seq;return message;
}
function newRun(state,text,versionID='',clarification=false) {
 const id='run'+(++runNumber),run={id,request:text,status:clarification?'awaiting_answer':'running',question:clarification?'这件木箱将用于什么场景？':'',wait_id:clarification?'wait1':'',generation:clarification?1:0,production:0,max_submissions:3,model_calls:1,max_model_calls:20,created:date,input_version_id:versionID,prompt_version:'asset-agent-v3'};
 state.runs.push(run);state.conversation.active_run_id=id;addMessage(state,'user',text,id,{},versionID);
 if(clarification)addMessage(state,'clarification',run.question,id,{generation:1},'','wait1');
 else startOperation(state,run);
 return run;
}
function startOperation(state,run) {
 run.status='running';run.production=1;run.current_operation={id:'op-'+run.id,kind:run.input_version_id?'decimate':'generate',stage:'submitted',task_id:'fixture-'+run.id};
 if(run.input_version_id){const source=state.versions.find(version=>version.id===run.input_version_id),target=run.request.includes('2000')?2000:3000;addMessage(state,'intent_review','已记录拟采用的需求变化；展示时尚未接受为本次正式意图。',run.id,{version_id:run.input_version_id,action:'decimate',intent:{asset:'木箱',use:'产品展示',max_triangles:target,max_bytes:10485760,plan:['按明确输入减面并进行技术检查']},changes:[{field:'max_triangles',before:source?.report.triangles,after:target}],inherited:['asset','use','max_bytes']});}
 addMessage(state,'accepted_plan','',run.id,{intent:{asset:'木箱',use:'产品展示',max_triangles:4500,max_bytes:10485760,plan:['依据明确引用制作','下载并检查真实文件']}});
 addMessage(state,'operation_card','',run.id,{operation_id:'op-'+run.id,operation_kind:run.input_version_id?'decimate':'generate',stage:'submitted',status:'running',progress:24,task_id:'fixture-'+run.id});event(state,'tripo_progress',{operation_id:'op-'+run.id,status:'running',progress:24});
}
function glb(faces) {
 const positions=Buffer.from(new Float32Array([-.5,-.5,-.5,.5,-.5,-.5,.5,.5,-.5,-.5,.5,-.5,-.5,-.5,.5,.5,-.5,.5,.5,.5,.5,-.5,.5,.5]).buffer);
 const cube=[0,2,1,0,3,2,4,5,6,4,6,7,0,1,5,0,5,4,3,7,6,3,6,2,0,4,7,0,7,3,1,2,6,1,6,5];
 const indices=Buffer.alloc(Math.ceil(faces*6/4)*4);for(let i=0;i<faces*3;i++)indices.writeUInt16LE(cube[i%cube.length],i*2);
 const bin=Buffer.concat([positions,indices]);
 const doc={asset:{version:'2.0'},scene:0,scenes:[{nodes:[0]}],nodes:[{mesh:0}],meshes:[{primitives:[{attributes:{POSITION:0},indices:1,material:0}]}],materials:[{pbrMetallicRoughness:{baseColorFactor:[.46,.56,.35,1],metallicFactor:0,roughnessFactor:.8}}],buffers:[{byteLength:bin.length}],bufferViews:[{buffer:0,byteOffset:0,byteLength:positions.length,target:34962},{buffer:0,byteOffset:positions.length,byteLength:faces*6,target:34963}],accessors:[{bufferView:0,componentType:5126,count:8,type:'VEC3',min:[-.5,-.5,-.5],max:[.5,.5,.5]},{bufferView:1,componentType:5123,count:faces*3,type:'SCALAR'}]};
 const raw=Buffer.from(JSON.stringify(doc));const json=Buffer.alloc(Math.ceil(raw.length/4)*4,0x20);raw.copy(json);
 const header=Buffer.alloc(12),jc=Buffer.alloc(8),bc=Buffer.alloc(8);header.writeUInt32LE(0x46546c67);header.writeUInt32LE(2,4);header.writeUInt32LE(12+8+json.length+8+bin.length,8);jc.writeUInt32LE(json.length);jc.writeUInt32LE(0x4e4f534a,4);bc.writeUInt32LE(bin.length);bc.writeUInt32LE(0x004e4942,4);return Buffer.concat([header,jc,json,bc,bin]);
}
function finish(state,faces=4500,parent='') {
 const run=state.runs.find(item=>item.id===state.conversation.active_run_id),bytes=glb(faces).length,id='v'+(state.versions.length+1);
 const report={valid:true,passed:true,triangles:faces,bytes,checks:[{name:'GLB 文件有效性',status:'passed',detail:'静态、自包含 GLB 文件'},{name:'三角面数',status:'passed',detail:`实测 ${faces} 个三角面`},{name:'文件体积',status:'passed',detail:`${bytes} 字节`}]};
 const version={id,conversation_id:state.conversation.id,version_number:state.versions.length+1,source_run_id:run.id,source_operation_id:'op-'+run.id,task_id:'fixture-'+run.id,operation_kind:parent?'decimate':'generate',parent_version_id:parent,sha256:'fixture-hash-'+id,bytes,format:'glb',report,created:date,provenance_status:'verified',processable:true,url:`/api/conversations/${state.conversation.id}/versions/${id}/file`};
 state.versions.push(version);Object.assign(run,{status:'completed',selected_artifact:version.source_operation_id,model_calls:5,final:`已交付本次模型。实测 ${faces} 个三角面，文件 ${bytes} 字节。未进行视觉检查。`,ended:date,outcome:{kind:'delivery',source:'runtime'},result:{status:'completed',artifact_id:version.source_operation_id,evidence:'verified'},evaluation:{checks:[{name:'结果证据一致性',status:'passed',detail:'本次报告与交付引用一致'}]}});
 const card=state.messages.find(message=>message.kind==='operation_card'&&message.run_id===run.id);card.data={...card.data,stage:'done',status:'checked',progress:100,report};card.version_id=id;card.updated_seq=tick();
 addMessage(state,'run_result',run.final,run.id,{source:'runtime',outcome:run.outcome,result:run.result});state.conversation.active_run_id='';state.cursor=tick();publish(state);return version;
}
function publish(state){if(pauseSnapshots)return;for(const item of sockets)if(item.id===state.conversation.id)item.socket.send(JSON.stringify(state));}
const other=envelope('other','路灯');conversations.set('other',other);
const browser=await chromium.launch({headless:true,args:['--enable-unsafe-swiftshader']});
const context=await browser.newContext({viewport:{width:1440,height:1000},deviceScaleFactor:1});
// loaded 在首帧绘制前就可能为 true；等待当前 URL 对应的公开 load 事件。
await context.addInitScript(()=>{document.addEventListener('load',event=>{if(event.target?.tagName==='MODEL-VIEWER')window.__fixtureLoadedModelURL=new URL(event.detail.url,location.href).href;},true);});
const page=await context.newPage();page.on('pageerror',error=>pageErrors.push(error.stack||error.message));
await page.routeWebSocket('**/api/conversations/*/events*',socket=>{const id=new URL(socket.url()).pathname.split('/')[3];const item={id,socket};sockets.add(item);socket.onClose(()=>sockets.delete(item));const state=conversations.get(id);if(state&&!pauseSnapshots)socket.send(JSON.stringify(state));});
await page.route('**/*',async route=>{
 const request=route.request(),url=new URL(request.url()),parts=url.pathname.split('/').filter(Boolean);
 if(url.origin!==base)return route.abort();
 if(request.method()==='POST'&&postGate){const gate=postGate;postGate=null;await gate;}
 const json=value=>route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(value)});
 if(url.pathname==='/api/config')return json({ready:true,mode:'controlled-test',model:'fixture'});
 if(url.pathname==='/api/conversations'&&request.method()==='GET')return json([...conversations.values()].map(state=>state.conversation));
 if(url.pathname==='/api/conversations'&&request.method()==='POST'){
  const payload=request.postDataJSON();commands.push(payload);let id=creationKeys.get(payload.client_message_id);
  if(!id){id='crate';const state=envelope(id);conversations.set(id,state);creationKeys.set(payload.client_message_id,id);newRun(state,payload.request,'',true);}
  if(abortCreation){abortCreation=false;return route.abort('failed');}return json(conversations.get(id));
 }
 if(parts[0]==='api'&&parts[1]==='conversations'){
  const state=conversations.get(parts[2]);if(!state)return route.fulfill({status:404,contentType:'application/json',body:JSON.stringify({error:'会话不可用'})});
  if(parts[3]==='messages'&&request.method()==='POST'){
   const payload=request.postDataJSON();commands.push(payload);const key=state.conversation.id+payload.client_message_id;
   if(messageKeys.has(key))return json(state);messageKeys.set(key,true);
   if(payload.kind==='answer') {const run=state.runs.find(item=>item.id===payload.run_id);assert.equal(payload.wait_id,run.wait_id);assert.equal(payload.generation,run.generation);addMessage(state,'user',payload.text,run.id,{},payload.version_id,payload.wait_id);if(deferAnswer){run.status='understanding';event(state,'model_call');}else startOperation(state,run);}
   else if(payload.text.includes('解释')){const run=newRun(state,payload.text,payload.version_id);state.messages=state.messages.filter(message=>message.run_id!==run.id||message.kind==='user');Object.assign(run,{production:0,status:'answered',model_calls:1,ended:date,final:'本次仅回答，未创建模型。',outcome:{kind:'answer',text:'该版本的面数来自文件实测，未检查外观。',source:'agent'}});addMessage(state,'answer',run.outcome.text,run.id,{source:'agent',verification:'unverifiable',outcome:run.outcome});state.conversation.active_run_id='';}
   else newRun(state,payload.text,payload.version_id);
   state.cursor=tick();publish(state);return json(state);
  }
  if(parts[3]==='runs'&&parts[5]==='stop'){const run=state.runs.find(item=>item.id===parts[4]);run.status='stopped';run.final='用户已停止本地执行。本地停止不表示远端已取消。';run.ended=date;run.outcome={kind:'ended',source:'runtime'};addMessage(state,'run_result',run.final,run.id,{source:'runtime'});return json(state);}
  if(parts[3]==='versions'&&parts[5]==='file'){const version=state.versions.find(item=>item.id===parts[4]);return route.fulfill({status:200,contentType:'model/gltf-binary',body:glb(version.report.triangles)});}
  return json(state);
 }
 if(parts[0]==='api'&&parts[1]==='sessions')return json({fixture:true});
 const file=resolve(staticDir,url.pathname==='/'?'index.html':'.'+url.pathname);
 if(!file.startsWith(staticDir+'/'))return route.abort();
 try{const body=await readFile(file);return route.fulfill({status:200,contentType:file.endsWith('.css')?'text/css':file.endsWith('.html')?'text/html':'text/javascript',body});}catch{return route.fulfill({status:404,body:'not found'});}
});
try {
 await page.goto(base);await expect(page.locator('#home')).toBeVisible();
 await expect(page.locator('#request')).toHaveValue('');assert.doesNotMatch(await page.locator('#request').getAttribute('placeholder'),/5,?000|最多/);
 const homeImage=resolve(output,'home-desktop.png');await page.screenshot({path:homeImage});screenshots.push(homeImage);
 const releaseHome=holdPost();await page.locator('#request').fill('为产品展示制作一个低模木箱，最多 5000 个三角面。');await page.locator('#submit').click();await expect(page.locator('#home-hint')).toHaveText('正在发送…');await expect(page.locator('#submit')).toBeDisabled();releaseHome();
 await expect(page.locator('#submit')).toHaveText('确认原提交');await expect(page.locator('#request')).toHaveValue('为产品展示制作一个低模木箱，最多 5000 个三角面。');
 await page.locator('#submit').click();await expect(page.locator('#workspace')).toBeVisible();await expect(page.locator('#question-context')).toBeVisible();assert.equal(commands[0].client_message_id,commands[1].client_message_id);assert.equal(conversations.size,2);assert.equal(commands[0].request,'为产品展示制作一个低模木箱，最多 5000 个三角面。');checks.push('first-response-loss-preserves-creation-identity');checks.push('placeholder-empty-sending-feedback-explicit-5000-retained');
 await page.locator('#draft-mode').click();await page.locator('#message').fill('稍后把这个版本减到 3000 个三角面。');await expect(page.locator('#send')).toBeDisabled();
 await page.locator('#answer-mode').click();await page.locator('#message').fill('用于电商产品展示，静态模型。');deferAnswer=true;const releaseAnswer=holdPost();await page.locator('#send').click();await expect(page.locator('#chat-activity')).toHaveAttribute('data-kind','sending');releaseAnswer();await expect(page.locator('#chat-activity')).toHaveAttribute('data-kind','thinking');deferAnswer=false;const answering=conversations.get('crate');startOperation(answering,answering.runs[0]);answering.cursor=tick();publish(answering);await expect(page.locator('#chat-activity')).toHaveAttribute('data-kind','generating');checks.push('clarification-sending-thinking-generating-continuity');await expect(page.locator('#message')).toHaveValue('稍后把这个版本减到 3000 个三角面。');await expect(page.locator('#send')).toBeDisabled();checks.push('answer-and-future-draft-isolation');
 await page.locator('[data-conversation-id="other"]').click();await page.locator('[data-conversation-id="crate"]').click();await expect(page.locator('#message')).toHaveValue('稍后把这个版本减到 3000 个三角面。');
 const state=conversations.get('crate');finish(state);await expect(page.locator('#versions-total')).toHaveText('1');await expect(page.locator('#send')).toBeEnabled();assert.equal(state.runs.length,1);checks.push('draft-survives-switch-and-is-not-auto-sent');
 await page.getByRole('button',{name:'预览 v1',exact:true}).click();await page.getByRole('button',{name:'引用 v1 到聊天',exact:true}).click();await expect(page.locator('#reference-label')).toHaveText('本条引用：v1');await page.locator('#send').click();assert.equal(commands.at(-1).version_id,'v1');
 await expect(page.locator('.intent-review')).toHaveCount(1);await expect(page.locator('.intent-diff')).toContainText('4,500');await expect(page.locator('.intent-diff')).toContainText('3,000');await expect(page.locator('.intent-review')).toContainText('保持的要求：资产主体、用途、文件体积上限');await expect(page.locator('.intent-review').getByRole('button',{name:/确认|接受/})).toHaveCount(0);
 await page.locator('.intent-review').scrollIntoViewIfNeeded();const reviewDesktop=resolve(output,'intent-review-desktop.png');await page.screenshot({path:reviewDesktop});screenshots.push(reviewDesktop);
 await page.setViewportSize({width:390,height:844});await page.locator('#tab-chat').click();await page.locator('.intent-review').scrollIntoViewIfNeeded();assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);const reviewMobile=resolve(output,'intent-review-mobile.png');await page.screenshot({path:reviewMobile});screenshots.push(reviewMobile);await page.setViewportSize({width:1440,height:1000});checks.push('intent-difference-before-accepted-plan-without-approval');
 finish(state,3000,'v1');await expect(page.locator('#versions-total')).toHaveText('2');await expect(page.locator('#preview-title')).toHaveText('v1 · 模型预览');
 await page.getByRole('button',{name:'引用 v1 到聊天',exact:true}).click();await page.getByRole('button',{name:'预览 v2',exact:true}).click();await expect(page.locator('#reference-label')).toHaveText('本条引用：v1');await page.locator('#message').fill('从引用的 v1 再做一个 2000 面版本。');await page.locator('#send').click();assert.equal(commands.at(-1).version_id,'v1');finish(state,2000,'v1');await expect(page.locator('#versions-total')).toHaveText('3');await expect(page.locator('#preview-title')).toHaveText('v2 · 模型预览');assert.equal(state.versions[2].parent_version_id,'v1');checks.push('three-runs-explicit-historical-parent-and-preview-independence');
 await page.locator('#message').fill('解释这个版本的技术检查。');await page.locator('#send').click();await expect(page.locator('#messages')).toContainText('Agent 回答');await expect(page.locator('#versions-total')).toHaveText('3');const answer=page.locator('.message').filter({hasText:'Agent 回答'});await expect(answer.locator('.result-version')).toHaveCount(0);checks.push('pure-answer-has-no-delivery-card');
 const count=state.messages.length;await page.reload();await expect(page.locator('.message')).toHaveCount(count);await expect(page.locator('#versions-total')).toHaveText('3');checks.push('reload-restores-all-runs-and-versions');
 await page.getByRole('button',{name:'预览 v1',exact:true}).click();await page.waitForFunction(()=>{const viewer=document.querySelector('model-viewer'),src=viewer?.getAttribute('src');return src&&viewer.loaded&&window.__fixtureLoadedModelURL===new URL(src,location.href).href;});checks.push('actual-model-viewer-loads-fixture-glb');
 const desktop=resolve(output,'workspace-desktop.png');await page.screenshot({path:desktop});screenshots.push(desktop);
 await page.locator('#message').fill('重新生成一个木箱版本。');await page.locator('#send').click();await expect(page.locator('#stop')).toBeVisible();await page.locator('#stop').click();await expect(page.locator('.workspace-toolbar #status')).toHaveText('正在停止');await page.locator('#message').fill('停止后保留的草稿');await expect(page.locator('#send')).toBeDisabled();
 state.conversation.active_run_id='';state.cursor=tick();publish(state);await expect(page.locator('#send')).toBeEnabled();await expect(page.locator('#message')).toHaveValue('停止后保留的草稿');checks.push('stop-waits-for-worker-idle');
 const beforeReconnect=await page.locator('.message').count();for(const item of [...sockets])if(item.id==='crate')item.socket.close({code:1001,reason:'fixture disconnect'});await expect(page.locator('#connection')).toHaveText('进度已连接',{timeout:6000});await expect(page.locator('.message')).toHaveCount(beforeReconnect);checks.push('websocket-reconnect-keeps-messages-unique');
 await page.setViewportSize({width:390,height:844});await expect(page.locator('#tab-chat')).toBeVisible();await page.locator('#tab-chat').click();await expect(page.locator('#chat-pane')).toBeVisible();await page.locator('#tab-model').click();await expect(page.locator('#model-pane')).toBeVisible();await expect(page.locator('#chat-pane')).toBeHidden();const overflow=await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth);assert.equal(overflow,false);const mobile=resolve(output,'workspace-mobile.png');await page.screenshot({path:mobile});screenshots.push(mobile);await page.locator('#tab-chat').click();await expect(page.locator('#message')).toHaveValue('停止后保留的草稿');checks.push('390px-tabs-and-no-horizontal-overflow');
 const {verifyFeedback}=await import('./workspace-feedback.browser.mjs');
 await verifyFeedback({page,expect,assert,conversations,envelope,addMessage,event,tick,publish,sockets,commands,holdPost,setPaused:value=>{pauseSnapshots=value;},output,screenshots,checks,base});
 assert.deepEqual(pageErrors,[]);const report={scope:'frontend-contract-only',backend:'mock HTTP and WebSocket',provider:'not called',passed:checks.length,checks,screenshots,page_errors:pageErrors};await writeFile(resolve(output,'report.json'),JSON.stringify(report,null,2));console.log(JSON.stringify({output,...report},null,2));
} catch(error){const failure=resolve(output,'failure.png');await page.screenshot({path:failure});console.error(JSON.stringify({output,checks,pageErrors,failure}));throw error;}finally{await browser.close();}
