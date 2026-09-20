# 聊天资产工作台浏览器验收

验收日期：2026-09-13。变更：`add-conversational-asset-workspace`。

| 层次 | 结果 | 证据与边界 |
| --- | --- | --- |
| 原生 JS 状态/连接测试 | 12/12 通过 | `internal/web/tests/workspace.test.mjs`；提交身份、草稿隔离、版本引用、旧回调、游标恢复及需求差异 |
| 前端契约浏览器（原始记录） | 10/10 通过，0 个页面异常 | [报告](frontend-contract/report.json)；HTTP/WS 全拦截，真实 Chromium 和 model-viewer 加载固定 GLB |
| 需求差异卡补验及前端回归 | 11/11 通过，0 个页面异常 | [报告](frontend-contract-intent-review/report.json)；保存前差异卡、承接字段、无额外审批入口、390px 展示 |
| Go + Eino 浏览器集成（原始记录） | 9/9 通过，0 个页面异常 | [报告](go-eino/report.json)；本机独立临时数据库，实际 Go API、SQLite、Eino 和 WS，模型/Tripo 使用受控替身 |
| Go + Eino 最终集成回归 | 10/10 通过，0 个页面异常 | [报告](go-eino-intent-review/report.json)；包含真实保存前差异投影的顺序、来源约束继承和桌面/窄屏渲染 |

核心链路完成首次请求与澄清、v1 的 4,500 面生成、两次明确引用 v1 分别制作 3,000 面和 2,000 面版本。两个后续版本的 `parent_version_id` 均为 v1，文件 SHA256 不同。纯解释只新增回答，生产次数为 0，不新增版本。

最终集成报告保留两次 `intent_review` 的实际协议数据：差异消息的序号分别为 26、45，早于对应已保存计划的 27、46。差异只有三角面上限从 4,500 改为 3,000 或 2,000，来源文件体积、风格、用途等要求沿用。页面分别展示保存前草案与已保存计划，没有增加用户审批步骤；未设置的约束不会因 `null` 与空数组的表示差异产生虚假变更。

同时验证：澄清回答与忙碌期草稿独立、草稿不自动发送、切换预览不改变提交引用、停止后等待执行退出再发送、刷新恢复、多 Run 的 WS 重连、旧 `#session/` 路由、会话与单 Run 导出、390px 页面无横向溢出。前端契约验收另外覆盖首次创建响应丢失后使用相同提交身份重试。

截图已目视核对，桌面与窄屏都可见实际夹具几何。测试等待 model-viewer 携带当前 URL 的公开 `load` 事件，避免将首帧绘制前的 `loaded=true` 误判为预览已经呈现。

- [首次聊天页面](frontend-contract/home-desktop.png)
- [Go 集成桌面工作台](go-eino/workspace-desktop.png)
- [Go 集成窄屏工作台](go-eino/workspace-mobile.png)
- [前端契约桌面工作台](frontend-contract/workspace-desktop.png)
- [前端契约窄屏工作台](frontend-contract/workspace-mobile.png)
- [新增差异卡桌面截图](frontend-contract-intent-review/intent-review-desktop.png)
- [新增差异卡窄屏截图](frontend-contract-intent-review/intent-review-mobile.png)
- [最终 Go 集成差异卡桌面截图](go-eino-intent-review/intent-review-desktop.png)
- [最终 Go 集成差异卡窄屏截图](go-eino-intent-review/intent-review-mobile.png)
- [最终 Go 集成桌面工作台](go-eino-intent-review/workspace-desktop.png)
- [最终 Go 集成窄屏工作台](go-eino-intent-review/workspace-mobile.png)

报告中的绝对截图路径保留原始运行记录；仓库内副本在各报告的同级目录。这里没有用户数据、登录凭证或真实供应商任务。截图中的立方体是几何夹具，不代表真实木箱生成效果；本记录不是正式 Agent 案例集评测，也不是任意崩溃恢复证明。

复现状态测试：

```sh
node --test internal/web/tests/workspace.test.mjs
```

浏览器脚本需要环境中已安装 Playwright/Chromium。前端契约脚本无需服务器；Go 集成脚本只接受本机 `controlled-*` 服务：

```sh
NODE_PATH="$(npm root -g)" node internal/web/tests/workspace.browser.mjs
CONVERSATION_BROWSER_TEST=1 go test ./internal/app -run '^TestConversationBrowserHarness$' -count=1 -v -timeout=32m
```

受控 Go 服务启动后，在另一个终端执行：

```sh
TRIPO_WORKSPACE_TEST_URL=http://127.0.0.1:48090 NODE_PATH="$(npm root -g)" node internal/web/tests/workspace.go-browser.mjs
```

不要将浏览器脚本指向真实服务或用户的数据目录。此服务默认仅存活 30 分钟；它与日常 8080 服务独立。
