# Tripo Agent

面向独立游戏开发者的静态道具制作应用：用自然语言提出需求，由一个 Agent 澄清、规划、调用 Tripo，并依据实际 GLB 的技术检查结果决定交付、减面、重新生成或停止。

Go + CloudWeGo Eino ADK + DeepSeek V4 Pro + Tripo API。支持匿名多会话、并发生产、WebSocket 时间线、模型预览和执行记录导出。

**当前状态：首条真实 DeepSeek / Tripo 木箱链路已于 2026-09-08 跑通，并通过技术检查、浏览器预览和真实任务重启恢复验证。** 实测 2,645 个三角面、910,572 字节，使用 5 次模型调用和 1 次生产提交。受控测试与真实运行的证据分别保存；60 次 Agent 案例评测尚未完成。详见 [验证记录](docs/demo-verification.md)。

[下载真实 GLB](docs/demo/live-crate.glb) · [技术报告](docs/demo/live-technical-report.json) · [完整执行记录](docs/demo/live-trace.json)

![真实生成模型的浏览器预览](docs/demo/live-model-preview.png)

## 本地启动

准备 Go 1.26.1 或兼容更新版本、Node.js 22 与 npm。首次安装前端查看器并构建：

```sh
npm ci --ignore-scripts
npm run build
```

首次使用时将 `.env.example` 复制为 `.env`，填写 `DEEPSEEK_API_KEY` 和 `TRIPO_API_KEY`。已有 `.env` 时保留原文件。密钥只写本地配置，不提交到 Git；进程环境变量优先于 `.env`。

```sh
go run ./cmd/server
```

打开 <http://127.0.0.1:8080>。没有密钥也可启动并查看配置提示，此时不会创建生产请求。配置密钥后重启服务，再点击“开始制作”。

需要独立可执行文件时，先完成上述查看器构建，再运行：

```sh
go build -o bin/tripo-agent ./cmd/server
./bin/tripo-agent
```

查看器会嵌入 Go 可执行文件，运行时不依赖 Node.js 或公共 CDN；未构建查看器时服务会提示具体补救步骤。服务默认读取当前工作目录中的 `.env`，使用 `./data` 保存状态。

| 配置 | 默认值 / 说明 |
| --- | --- |
| `DEEPSEEK_API_KEY` | DeepSeek API 密钥，模型固定为 `deepseek-v4-pro` |
| `TRIPO_API_KEY` | Tripo API 密钥，生成模型固定为 `v3.1-20260211` |
| `LISTEN_ADDR` | `127.0.0.1:8080` |
| `DATA_DIR` | `./data`，包含 SQLite 和模型文件，重启时继续使用同一目录 |
| `COOKIE_SECURE` | `false`，HTTPS 部署时设置为 `true` |

请仅运行一个服务进程管理同一数据目录。当前调度器的执行互斥属于单进程，SQLite 不承担多实例任务认领。

## 演示步骤

1. 提交“给我的俯视角游戏原型做一个低模木箱。”
2. 若 Agent 发起澄清，回答卡通风格、静态 GLB、最多 5,000 个三角面、10 MiB；最多澄清三轮。
3. 查看确认意图、默认假设、计划、实际工具提交与异步进度。Agent 依据检查报告选择纠偏或结束，首次通过就直接交付。
4. 查看最终候选的三类技术报告，旋转/缩放预览，下载 GLB，导出执行记录。可以切换查看此前未通过的候选。
5. 刷新仍可恢复会话，也可新建其他资产会话；停止按钮只停止本地后续执行，不能证明远端任务已经取消。

技术检查覆盖静态、自包含、非压缩三角面 GLB 的基础容器、缓冲区、几何与场景引用，以及所有网格合计面数和实际文件字节数。它不是完整 glTF 规范认证，不检查类别、风格或视觉质量。下载保护上限为 150 MiB，网格展开检查上限为 256 MiB，超出时明确报告无法验证。

## 执行边界

- 一个会话对应一个资产请求，由同一个 Agent 决策；会话间 Context、预算、检查点与产物隔离。
- 每个请求累计最多 20 次模型调用、3 次生产提交（首次生成 + 最多两次纠偏）；提交失败和结果未知也消耗额度。
- 从首次生产提交起最多执行 30 分钟，重启和断线不重置预算或时间。已知任务 ID 恢复查询，提交结果未知时停止，不自动重发。
- 同时最多 3 个生产阶段请求，另有 10 个 FIFO 等待位置；排队不扣生产额度，也不启动生产计时。
- 匿名 Cookie 连续 30 天未访问失效；生产前 24 小时无用户操作结束；结束后数据保留 7 天。更换浏览器或清除 Cookie 后不提供旧会话找回。

这些初值集中在 `internal/app/types.go` 的 `DefaultConfig`；已开始的请求使用持久化预算。并发数限制本地生产阶段请求，不能保证停止后的远端任务不再占用供应商配额。

## 代码组织

| 路径 | 职责 |
| --- | --- |
| `cmd/server` | 配置、启动和退出 |
| `internal/app/agent.go` | Eino Runner、模型调用计数、工具约束和检查点中断/恢复 |
| `internal/app/skills` | 当前 Agent 按需加载的三份只读 Skill |
| `internal/app/service.go` | FIFO 调度、生产状态、轮询、纠偏结果和停止/恢复 |
| `internal/app/store.go` | SQLite 会话、事件、匿名归属和 Eino 检查点 |
| `internal/tripo` | Tripo 生成、减面、任务查询及下载适配 |
| `internal/asset` | 根据真实文件生成技术报告 |
| `internal/app/http.go`、`internal/web` | HTTP、WebSocket、脱敏导出和最小页面 |

Agent 负责依赖当前证据的选择；Go 执行确定性步骤与约束。Eino 负责模型/工具循环和恢复，应用状态是预算、归属和产物事实来源。模型提议、Runtime 放行/拦截、工具结果分别记录，禁止将提议当成执行成功。

## 验证

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

测试不需要真实密钥，不调用外部供应商。包括实际 Eino Runner + 脚本模型、真实 DeepSeek SDK + 本地协议响应、Tripo HTTP 协议、GLB 实测、并发、会话隔离、停止、预算和持久恢复。

可选的受控浏览器演示：

```sh
TRIPO_BROWSER_TEST=1 go test ./internal/app -run '^TestBrowserHarness$' -v -count=1 -timeout=6m
```

打开 <http://127.0.0.1:48089>，5 分钟后自动退出。页面明确标注受控测试；其中的立方体由测试夹具构造，不能作为真实木箱演示成果。正式服务没有这个测试模式开关。

页面的单次规则核验只判断当前运行的可核验证据。`CONTEXT.md` 规定的 **20 案例 × 3 次真实 LLM 行为评测尚未实现和执行**，需单独完成，不能用本仓库系统测试的通过率替代。

项目范围以 [PROJECT_BRIEF.md](PROJECT_BRIEF.md)、[CONTEXT.md](CONTEXT.md) 和 [ADR](docs/adr/) 为准。本次规格与实现任务见 [first-asset-demo](openspec/changes/first-asset-demo/)。
