# 聊天反馈与局部样式验收

- 日期：2026-09-19
- 变更：`improve-chat-feedback-and-polish`
- 基线：`e9414ee`，分支 `mvp`
- 范围：前端显示、连接生命周期及受控测试；未修改 Go 业务代码、公开接口、数据库、Agent/Skill、生产预算或供应商配置。

## 实现结果

1. 首页占位示例改为木椅的用途、轮廓与材质，输入初值为空，示例不进入提交。用户主动填写的 5000 面及历史报告继续保留。
2. 新增纯函数模块 `workspace-activity.mjs`，按当前会话、活动 Run、当前操作及事件序号计算阶段。覆盖发送、送达待确认、思考、待答、两级排队、准备制作、生成/减面、文件准备、停止退出和连接恢复。
3. 生成/减面以当前操作的 `tripo_progress` 为依据。操作卡的 `running` 可能继承自本地入队，单凭它不宣称供应商已开始生成；没有可靠阶段时显示“正在处理模型任务”。供应商成功不等于文件技术检查通过。缺少进度不显示虚构的 0%。
4. 独立活动节点位于正式消息列表之外，只在阶段变化时更新文字。没有新增持久消息、事件、导出或模型上下文，也不会产生自动制作请求。
5. 15 秒没有有效快照时只读恢复连接；相同游标的有效快照续期，`onopen` 本身不恢复实时阶段。旧会话、旧代次和旧 socket 的回调不能影响当前页面。返回前台检查新鲜度，离开或会话不可用时清理计时器。
6. 保留原布局和绿色调，提高正文到 14 px，关键辅助文字至少 12 px。保留草稿、引用、预览、展开项、输入光标和证据焦点；上翻历史不拉回底部，底部阅读跟随新内容。减少动态效果偏好关闭装饰动画。

技术信息没有隐藏或迁移：预算、模型调用次数、TaskID、原始参数/JSON、诊断、需求草案、计划、技术报告、程序核验和导出仍按原规则呈现；正式回答及结果正文未改写。

## 验证结果

| 验证 | 结果 | 范围及证据 |
| --- | --- | --- |
| Node 状态/连接测试 | 22/22 通过，退出 0 | [原始输出](chat-feedback-and-polish/node-test.txt)，包含无副作用、乱序/重复事件、未知提交、停止门禁、过期连接、同游标续期、旧回调和供应商状态来源 |
| 浏览器 HTTP/WS 契约 | 20/20 通过，页面异常 0，退出 0 | [报告](chat-feedback-and-polish/browser-report.json)，全部请求拦截，人工推进模型/供应商阶段与时间 |
| 真实 Go + Eino 浏览器集成 | 13/13 通过，页面异常 0，退出 0 | [报告](chat-feedback-and-polish/go-browser-report.json)，仅本机 controlled 服务，真实持久快照、WebSocket、匿名会话与文件交付流程 |
| 前端依赖构建 | 通过，退出 0 | `npm run build`；仅准备 model-viewer，不将此项视为页面验收 |
| Go 全量测试 | 通过，退出 0 | `go test ./...`；app 包约 70 秒，Tripo 客户端与资产检查测试通过；未启用任何真实 API 测试开关 |
| 服务构建 | 通过，退出 0 | `go build -o /tmp/tripo-chat-feedback-build/server ./cmd/server`；嵌入最终静态资源，未启动或替换生产服务 |
| OpenSpec 严格校验 | 通过 | `openspec validate improve-chat-feedback-and-polish --strict` |
| 差异检查 | 通过 | `git diff --check`；本变更无 Go 业务代码或依赖变更 |

### 浏览器覆盖

- 首页延迟发送、首次响应丢失后使用相同 `client_message_id` 确认，实际发送内容仅为用户输入。
- 澄清回答发送 → Agent 静默思考 → 生成；后续目标发送 → 仅回答。
- 本地排队、队列满、提交准备、供应商排队、实际 24% 进度、远端成功后文件准备、旧失败候选后的思考/减面纠偏。
- 相同阶段保持同一 DOM 节点且不重复修改 live region；缺失进度无 0%；历史消息、生产数和提交身份不被活动提示修改。
- 连接中断、连接打开无快照、同游标心跳、15 秒静默、终态恢复、只读恢复不增加 POST；停止退出仍禁止新目标。
- 重复/旧快照、刷新、返回首页、切换会话；保留草稿、版本引用与手动预览。
- 输入焦点、光标、证据展开项及焦点；新增模型预览按钮时仍保持证据焦点。上翻阅读和底部跟随分别验证。
- 1440、390、320 px 下没有页面整体横向溢出，发送/停止、模型切换、引用、下载、报告和导出可达；长 ID/JSON 在局部换行或滚动。

### Go 集成边界

6 个 Run 的生产计数为 `[1, 1, 1, 0, 1, 1]`，依次包含首次生成、两次基于 v1 减面、仅回答、停止和取消可选上限后的继续减面。4 个版本实测面数为 `4500 / 3000 / 2000 / 2250`，旧版本和父版本关系保留。仅回答不新增模型；停止不删除历史。导出仍包含相应 Run、版本和结果证据，不包含新增活动提示。

原有 Go 供应商夹具在延迟后直接返回 success，没有逐步 queued/running 回报，因此本机集成实际观察到 `syncing / awaiting_answer / sending / thinking / processing`；细分的供应商阶段由 HTTP/WS 契约测试验证，没有为制造通过结果修改后端夹具或虚构真实供应商回报。

浏览器测试最终使用 `clock.fastForward` 跨越心跳窗口，不逐帧推进 viewer 的动画；一轮使用 `runFor` 的补充回归因此耗时过长被主动中断，改用跳时后完整 20 项通过。此处只改变测试时钟驱动，15 秒边界和恢复断言保留。

两次受控服务都在浏览器脚本退出 0 后主动终止。服务夹具原本持续等待，手动终止会使该服务进程打印 `signal: interrupt` / `FAIL`，这不是全量 Go 测试失败；该进程仅用于提供受控服务。

## 网页检查证据

浏览器报告保留测试时的原始临时截图路径；以下代表截图另存入仓库。截图仅检查网页布局、字号、可达性及反馈；合成模型用于验证 viewer 加载，不评价模型外观，也不进入 Agent 视觉评测案例集。

- [桌面思考提示](chat-feedback-and-polish/feedback-thinking-desktop.png)
- [桌面生成与真实进度](chat-feedback-and-polish/feedback-generating-desktop.png)
- [390 px 聊天](chat-feedback-and-polish/feedback-chat-390.png)
- [320 px 聊天](chat-feedback-and-polish/feedback-chat-320.png)
- [320 px 模型区](chat-feedback-and-polish/feedback-model-320.png)

## 复现

```sh
node --test internal/web/tests/workspace.test.mjs
npm run build
NODE_PATH=$(npm root -g) node internal/web/tests/workspace.browser.mjs
go test ./...
go build -o /tmp/tripo-chat-feedback-build/server ./cmd/server
openspec validate improve-chat-feedback-and-polish --strict
```

本机集成需在单独终端启动 `CONVERSATION_BROWSER_TEST=1 go test ./internal/app -run '^TestConversationBrowserHarness$' -count=1 -v`，再执行：

```sh
TRIPO_WORKSPACE_TEST_URL=http://127.0.0.1:48090 NODE_PATH=$(npm root -g) node internal/web/tests/workspace.go-browser.mjs
```

脚本拒绝非本机或非 controlled 服务。完成后终止这一个受控测试进程。macOS 沙箱内无法启动 Chromium 的 Mach 服务时，需要允许本地浏览器进程；Go 构建也可能需要写入本机缓存。本次均在正常本机权限下通过，未读取或使用生产密钥。

## 发布状态

实施完成后可单独归档或提交。本轮未 commit、push、归档或部署，也没有启动真实 DeepSeek/Tripo 评测或修改线上数据。后续发布必须重新构建并替换 Go 程序，刷新浏览器加载嵌入的新页面；保留当前数据库、模型文件和国内站配置。
