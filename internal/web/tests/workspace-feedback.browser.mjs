// 沿用主浏览器验收的 HTTP/WS 夹具，控制阶段与传输延迟，不接触真实供应商。
import {resolve} from 'node:path';
export async function verifyFeedback({page,expect,assert,conversations,envelope,addMessage,event,tick,publish,sockets,commands,holdPost,setPaused,output,screenshots,checks,base}) {
 await page.setViewportSize({width:1440,height:1000});
 const state=envelope('feedback','连续反馈验收'),run={id:'feedback-run',status:'understanding',production:0,max_submissions:3,model_calls:1,max_model_calls:20};
 state.runs.push(run);state.conversation.active_run_id=run.id;addMessage(state,'user','制作一个木箱',run.id);event(state,'model_call');conversations.set('feedback',state);
 await page.goto(base+'/#conversation/feedback');
 const activity=page.locator('#chat-activity');
 const phase=kind=>expect(activity).toHaveAttribute('data-kind',kind);
 const update=()=>{state.cursor=tick();publish(state);};
 await phase('thinking');const thinkingImage=resolve(output,'feedback-thinking-desktop.png');await page.screenshot({path:thinkingImage});screenshots.push(thinkingImage);await expect(activity).toHaveCount(1);await expect(page.locator('#messages #chat-activity')).toHaveCount(0);
 await page.evaluate(()=>{window.__activity=document.querySelector('#chat-activity');window.__activityChanges=0;new MutationObserver(()=>window.__activityChanges++).observe(window.__activity.querySelector('.activity-text'),{childList:true,characterData:true,subtree:true});});
 publish(state);publish(state);await page.waitForTimeout(100);
 assert.equal(await page.evaluate(()=>window.__activity===document.querySelector('#chat-activity')&&window.__activityChanges===0),true);
 await expect(page.locator('.message')).toHaveCount(1);assert.equal(state.messages.length,1);checks.push('single-transient-node-outside-messages-and-no-repeat-announcement');
 run.status='queued';update();await phase('queued');run.status='queue_full';update();await phase('queue_full');
 run.status='running';run.current_operation={id:'op-feedback',kind:'generate',stage:'ready'};update();await phase('preparing');
 run.current_operation.stage='submitting';event(state,'tool_submitting',{operation_id:'op-feedback'});publish(state);await phase('preparing');
 run.production=1;Object.assign(run.current_operation,{stage:'submitted',task_id:'fixture-feedback'});
 const card=addMessage(state,'operation_card','',run.id,{operation_id:'op-feedback',operation_kind:'generate',stage:'submitted',task_id:'fixture-feedback',status:'queued',progress:null});event(state,'tripo_progress',{operation_id:'op-feedback',status:'queued',progress:null});publish(state);await phase('provider_queued');
 await expect(page.locator('.operation-facts')).not.toContainText('0%');await expect(page.locator('progress')).not.toHaveAttribute('value');
 function progress(status,value){card.data.status=status;card.data.progress=value;card.updated_seq=tick();event(state,'tripo_progress',{operation_id:'op-feedback',status,progress:value});publish(state);}
 progress('running',24);await phase('generating');const generatingImage=resolve(output,'feedback-generating-desktop.png');await page.screenshot({path:generatingImage});screenshots.push(generatingImage);await expect(page.locator('.operation-facts').filter({hasText:'远端进度'})).toHaveText('远端进度 24%');
 await page.emulateMedia({reducedMotion:'reduce'});assert.equal(await activity.locator('.activity-dot').evaluate(element=>getComputedStyle(element).animationName),'none');await expect(activity).toContainText('正在生成模型');
 await page.locator('#message').focus();await page.keyboard.type('test');await page.keyboard.press('Tab');assert.equal(await page.evaluate(()=>document.activeElement.id==='export'||document.activeElement.tagName!=='BODY'),true);
 const focused=await page.evaluate(()=>({tag:document.activeElement.tagName,style:getComputedStyle(document.activeElement).outlineStyle}));assert.notEqual(focused.style,'none');await page.emulateMedia({reducedMotion:'no-preference'});
 // 插入长历史与技术证据，检查上翻、焦点与展开状态不被瞬态刷新打断。
 for(let i=0;i<12;i++)addMessage(state,'answer','已有记录 '+i+'：'+('模型制作说明。'.repeat(20)),'history');
 card.data.diagnostic={message:'提交 Tripo：请求超时。',request_id:'x'.repeat(180)};card.updated_seq=tick();publish(state);await expect(page.locator('.message')).toHaveCount(state.messages.length);
 const evidence=page.locator(`[data-message-id="${card.id}"] details`);await evidence.locator('summary').click();await page.locator('#message').fill('保留中的草稿');await page.locator('#message').focus();
 await page.evaluate(()=>{document.querySelector('#chat-scroll').scrollTop=50;document.querySelector('#message').setSelectionRange(2,2);});
 const scroll=await page.locator('#chat-scroll').evaluate(element=>element.scrollTop);publish(state);await page.waitForTimeout(100);
 assert.equal(await page.locator('#chat-scroll').evaluate(element=>element.scrollTop),scroll);await expect(page.locator('#message')).toBeFocused();assert.equal(await page.locator('#message').evaluate(element=>element.selectionStart),2);await expect(evidence).toHaveAttribute('open','');
 // 当前生产推进时不把阅读位置拖到底部；在底部则继续跟随。
 await evidence.locator('summary').focus();progress('running',25);await expect(evidence.locator('summary')).toBeFocused();await expect(evidence).toHaveAttribute('open','');await page.locator('#message').focus();
 progress('success',100);await phase('files');assert.equal(await page.locator('#chat-scroll').evaluate(element=>element.scrollTop),scroll);
 await page.locator('#chat-scroll').evaluate(element=>{element.scrollTop=element.scrollHeight;});
 await evidence.locator('summary').focus();await page.locator('#chat-scroll').evaluate(element=>{element.scrollTop=element.scrollHeight;});
 const candidate={...conversations.get('crate').versions[0],id:'focus-version',conversation_id:'feedback',source_run_id:run.id,source_operation_id:'op-feedback'};state.versions.push(candidate);card.version_id=candidate.id;
 run.current_operation.stage='done';card.data.stage='done';card.data.report={passed:false,triangles:7000};card.updated_seq=tick();event(state,'technical_report',{operation_id:'op-feedback'});event(state,'model_call');publish(state);await phase('thinking');await expect(evidence.locator('summary')).toBeFocused();
 assert.equal(await page.locator('#chat-scroll').evaluate(element=>element.scrollHeight-element.scrollTop-element.clientHeight<10),true);
 run.current_operation={id:'correction',kind:'decimate',stage:'submitted',task_id:'fixture-correction'};
 event(state,'tool_submitted',{operation_id:'correction'});event(state,'tripo_progress',{operation_id:'correction',status:'running',progress:33});publish(state);await phase('decimating');
 // 未闭合旧 model_call、旧候选失败/成功及重复事件都不能覆盖纠偏操作。
 const stale=JSON.parse(JSON.stringify(state));stale.cursor--;stale.runs[0].current_operation={id:'op-feedback',stage:'done'};for(const item of sockets)if(item.id==='feedback')item.socket.send(JSON.stringify(stale));publish(state);await phase('decimating');
 checks.push('queues-production-file-preparation-correction-and-real-progress');checks.push('scroll-focus-caret-details-draft-preserved-and-reduced-motion');
 // 相同游标可以续期；连接打开但没有快照仍为同步中。
 const commandCount=commands.length,production=run.production,messageCount=state.messages.length;
 for(const item of [...sockets])if(item.id==='feedback')item.socket.close();setPaused(true);
 await phase('reconnecting');await phase('syncing');await expect(page.locator('#connection')).toHaveText('正在同步进度');
 setPaused(false);publish(state);await phase('decimating');
 // 跨越心跳窗口时每个计时器至多触发一次，避免逐帧驱动 3D viewer。
 await page.clock.install();await page.clock.fastForward(14000);publish(state);await page.waitForTimeout(50);await page.clock.fastForward(14000);await phase('decimating');
 // 不推进 cursor 的心跳已续期；停止快照后经过阈值才降级。
 setPaused(true);await page.clock.fastForward(1100);await phase('reconnecting');
 run.status='completed';state.conversation.active_run_id='';addMessage(state,'run_result','已完成，保留原事实。',run.id);await page.clock.fastForward(1500);await expect(activity).toBeHidden();
 setPaused(false);publish(state);await expect(page.locator('#connection')).toHaveText('进度已连接');await expect(activity).toBeHidden();
 assert.equal(commands.length,commandCount);assert.equal(run.production,production);assert.equal(state.messages.length,messageCount+1);await expect(page.locator('#message')).toHaveValue('保留中的草稿');checks.push('same-cursor-heartbeat-stale-open-without-snapshot-readonly-recovery-to-terminal');
 // 后续请求提交延迟时，当前已完成的 Run 不会被误报成正在思考。
 const release=holdPost();await page.locator('#message').fill('解释这个模型');await page.locator('#send').click();await phase('sending');release();await expect(activity).toBeHidden();assert.equal(state.runs.at(-1).production,0);checks.push('followup-send-to-answer-no-extra-production');
 // 失败/停止的最终结果消除忙碌提示；活动指针释放之前禁止发送。
 state.conversation.active_run_id=run.id;run.status='failed';update();await phase('ending');await page.locator('#message').fill('未发送');await expect(page.locator('#send')).toBeDisabled();state.conversation.active_run_id='';update();await expect(activity).toBeHidden();
 run.status='running';state.conversation.active_run_id=run.id;update();await phase('decimating');await page.locator('#stop').click();await phase('stopping');await expect(page.locator('#send')).toBeDisabled();
 state.conversation.active_run_id='';update();await expect(activity).toBeHidden();await expect(page.locator('#send')).toBeEnabled();
 await page.reload();await expect(activity).toBeHidden();await expect(page.locator('.message')).toHaveCount(state.messages.length);
 run.status='understanding';delete run.current_operation;state.conversation.active_run_id=run.id;event(state,'model_call');publish(state);await phase('thinking');await page.locator('#new-session').click();await expect(activity).toBeHidden();await page.locator('[data-conversation-id="crate"]').click();await expect(activity).toBeHidden();checks.push('terminal-stop-idle-gate-refresh-home-and-conversation-switch');
 // 各尺寸保留证据、报告与版本入口，长 ID/JSON 不撑开页面。
 for(const width of [1440,390,320]){
  await page.setViewportSize({width,height:width===1440?1000:844});
  if(width<800)await page.locator('#tab-chat').click();
  await expect(page.locator('#send')).toBeVisible();await expect(page.locator('#export')).toBeVisible();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  assert.equal(await page.locator('#message').evaluate(element=>parseFloat(getComputedStyle(element).fontSize)>=14),true);
  const file=resolve(output,`feedback-chat-${width}.png`);await page.screenshot({path:file});screenshots.push(file);
  if(width<800)await page.locator('#tab-model').click();
  await expect(page.locator('#reference-preview')).toBeVisible();await expect(page.locator('#download')).toBeVisible();await expect(page.locator('.report-details summary')).toBeVisible();
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  const model=resolve(output,`feedback-model-${width}.png`);await page.screenshot({path:model});screenshots.push(model);
 }
 checks.push('1440-390-320-readable-controls-reports-exports-and-no-page-overflow');
}
