# Tripo Agent

面向 3D 创作者及行业从业者的资产创作 Agent，通过网页提供资产制作服务。优先承担资产制作助手的角色：围绕用户目标组织制作过程，交付可使用或继续加工的资产；长期逐步覆盖资产生成、处理、骨骼绑定和动画。

资产生产与加工长期限定在 Tripo API 实际开放的能力内，不接入其他 3D 生产工具补齐能力缺口。每项能力需经过项目接入与验证后才向用户提供；Agent 的模型服务、应用调度与技术检查仍由现有组件承担。

当前聊天工作台支持在同一会话围绕同一资产顺序提出多个目标：生成、明确引用本会话版本减面、解释技术结果。每个目标对应独立 Run，具有独立预算、检查点和结局；旧版本和旧报告保留。当前变更已按非视觉范围完成验收，见下方验证状态。

**当前生产能力仍限定为文本生成和静态资产减面。** 当前实现由一个 Agent 澄清、规划、调用 Tripo，并依据实际 GLB 的技术检查结果决定交付、减面、重新生成或停止。图片输入、已有资产导入、骨骼绑定和动画是后续方向，尚未实现。木箱是首条端到端演示案例，阶段验收仍须满足独立的系统回归与 Agent 评测要求。

静态资产 MVP 是基础阶段，聊天持续创作是当前阶段，两者均不等于最终能力上限；长期目标与阶段划分见 [PROJECT_BRIEF.md](PROJECT_BRIEF.md)。

Go + CloudWeGo Eino ADK + DeepSeek V4 Pro + Tripo API。支持匿名多会话、并发生产、WebSocket 时间线、模型预览和执行记录导出。

**历史连续创作阶段验证状态：变更验收完成，47/47。** 按用户授权，以[非视觉范围 `technical-only-v1`](docs/evaluations/2026-09-14-technical-only-evaluation-scope.md)重新审核第五批全部 84 例、344 条原始提议：基线解释 55/58（94.83%），连续创作解释 22/24（91.67%），两组意图和策略均超过 90%，关键错误均为 0，见[新审核报告](docs/evaluations/2026-09-14-technical-only-reassessment-results.md)。仍保留 6 项非关键失败，没有宣称全部案例零错误。本次没有新增模型或 Tripo 调用；结合[系统回归 8/8](docs/verification/input-binding-acceptance/system-checks.md)、浏览器、恢复与真实加工证据，完成 `10.6`、`11.2`，详见[阶段验证](docs/verification/conversation-workspace.md)。旧成绩完整保留，6 个能力规格已同步并完成归档，尚未部署。

历史[两例局部复测](docs/evaluations/2026-09-14-critical-cases-retest.md)保留 `product/1` 通过、`missing_output/3` 未通过的原始结果。此后完成[缺文件反馈证据修复](docs/verification/2026-09-14-failure-evidence-fix.md)与[输入版本绑定修复](docs/verification/2026-09-14-input-binding-fix.md)，分别运行第四、第五批完整评测；修复和局部通过均不代替整批验收。

2026-09-08 的首条真实木箱链路是独立历史证据：2,645 个三角面、910,572 字节，5 次模型调用和 1 次生产提交。保留原[验证记录](docs/demo-verification.md)与产物：

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

打开 <http://127.0.0.1:8080>。没有密钥也可启动并查看配置提示，此时不会创建生产请求。配置密钥后重启服务，再从首页发送首条制作请求。

需要独立可执行文件时，先完成上述查看器构建，再运行：

```sh
go build -o bin/tripo-agent ./cmd/server
./bin/tripo-agent
```

查看器会嵌入 Go 可执行文件，运行时不依赖 Node.js 或公共 CDN；未构建查看器时服务会提示具体补救步骤。服务默认读取当前工作目录中的 `.env`，使用 `./data` 保存状态。

| 配置 | 默认值 / 说明 |
| --- | --- |
| `DEEPSEEK_API_KEY` | DeepSeek API 密钥，模型固定为 `deepseek-v4-pro` |
| `TRIPO_API_KEY` | 与所选站点匹配的 Tripo API 密钥，生成模型固定为 `v3.1-20260211` |
| `TRIPO_BASE_URL` | `https://openapi.tripo3d.com/v3`，国内站 V3；仅接受官方 V3 HTTPS 地址，不自动回退 |
| `LISTEN_ADDR` | `127.0.0.1:8080` |
| `DATA_DIR` | `./data`，包含 SQLite 和模型文件，重启时继续使用同一目录 |
| `COOKIE_SECURE` | `false`，HTTPS 部署时设置为 `true` |

请仅运行一个服务进程管理同一数据目录。当前调度器的执行互斥属于单进程，SQLite 不承担多实例任务认领。

生产前可运行 `go run ./cmd/check-tripo` 做只读余额鉴权预检；它复用服务配置，不创建 Run 或付费任务。API V3、生成模型 H3.1、减面算法 `v2.0` 是不同层次的版本。配置、诊断字段和本次 ECS 切换步骤见[国内站发布说明](docs/tripo-china-deployment.md)，实施证据见[验证记录](docs/verification/tripo-china-access/verification.md)。

## 演示步骤

1. 提交“给我的俯视角游戏原型做一个低模木箱。”
2. 若 Agent 发起澄清，按需要补充用途和风格；最多澄清三轮。可明确要求最多 5,000 个三角面、10 MiB，也可不设置这两个上限。
3. 查看确认意图、默认假设、计划、实际工具提交与异步进度。Agent 依据检查报告选择纠偏或结束，首次通过就直接交付。
4. 查看最终候选的三类技术报告，旋转/缩放预览，下载 GLB，导出执行记录。可以切换查看此前未通过的候选。
5. 在版本列表中明确引用 v1，发送“将这个模型减面至最多 3,000 个三角面”，得到新版本；再次引用 v1 提出另一目标时，父版本仍是 v1。预览其他模型不会改变引用。
6. 可以询问已保存版本的技术数据，纯回答不创建新版本。运行期间可保留下一目标草稿，完成或停止退出后再发送；停止只结束本地后续执行，不保证远端取消，也不支持恢复已停止目标。

技术检查覆盖静态、自包含、非压缩三角面 GLB 的基础容器、缓冲区、几何与场景引用，以及所有网格合计面数和实际文件字节数。它不是完整 glTF 规范认证，不检查类别、风格或视觉质量。下载保护上限为 150 MiB，网格展开检查上限为 256 MiB，超出时明确报告无法验证。

## 执行边界

- 一个会话围绕一个资产主题，可顺序执行多个目标；同会话一次只执行一个 Run。引用限定为本会话的单个版本，其他独立资产应新建会话。
- 每个请求累计最多 20 次模型调用、3 次生产提交（首次生成 + 最多两次纠偏）；提交失败和结果未知也消耗额度。
- 从首次生产提交起最多执行 30 分钟，重启和断线不重置预算或时间。已知任务 ID 恢复查询，提交结果未知时停止，不自动重发。
- 同时最多 3 个生产阶段请求，另有 10 个 FIFO 等待位置；排队不扣生产额度，也不启动生产计时。
- 匿名 Cookie 连续 30 天未访问失效；生产前 24 小时无用户操作结束；最后一个 Run 结束后整段会话保留 7 天。到期前正式发送新目标保护全部历史文件，结束后重新计算期限；浏览、下载和草稿不续期。更换浏览器或清除 Cookie 后不提供旧会话找回。

这些初值集中在 `internal/app/types.go` 的 `DefaultConfig`；已开始的请求使用持久化预算。并发数限制本地生产阶段请求，不能保证停止后的远端任务不再占用供应商配额。

## 代码组织

| 路径 | 职责 |
| --- | --- |
| `cmd/server` | 配置、启动和退出 |
| `internal/app/agent.go` | Eino Runner、模型调用计数、工具约束和检查点中断/恢复 |
| `internal/app/skills` | 按协议版本冻结的只读 Skill；v3 增加资产加工指引，v4 支持可选技术上限 |
| `internal/app/service.go` | FIFO 调度、生产状态、轮询、纠偏结果和停止/恢复 |
| `internal/app/store.go`、`conversation_store.go`、`conversation_projection.go` | SQLite 会话/Run/版本、幂等消息、公开事件投影和 Eino 检查点 |
| `internal/tripo` | Tripo 生成、内部 GLB 上传、减面、任务查询及下载适配 |
| `internal/asset` | 根据真实文件生成技术报告 |
| `internal/app/http.go`、`conversation_http.go`、`internal/web` | 兼容单次入口、聊天接口、WebSocket、导出和资产工作台 |

Agent 负责依赖当前证据的选择；Go 执行确定性步骤与约束。Eino 负责模型/工具循环和恢复，应用状态是预算、归属和产物事实来源。模型提议、Runtime 放行/拦截、工具结果分别记录，禁止将提议当成执行成功。

## 验证

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

默认测试不需要真实密钥，不调用外部供应商；真实供应商与模型评测通过专用环境变量显式启用。包括实际 Eino Runner + 脚本模型、真实 DeepSeek SDK + 本地协议响应、Tripo HTTP 协议、GLB 实测、并发、会话隔离、停止、预算和持久恢复。

可选的受控浏览器演示：

```sh
TRIPO_BROWSER_TEST=1 go test ./internal/app -run '^TestBrowserHarness$' -v -count=1 -timeout=6m
```

打开 <http://127.0.0.1:48089>，5 分钟后自动退出。页面明确标注受控测试；其中的立方体由测试夹具构造，不能作为真实木箱演示成果。正式服务没有这个测试模式开关。

页面的单次规则核验只判断当前运行的可核验证据，不能替代 Agent 决策质量评测。当前阶段以 `technical-only-v1` 保留 **20 个基线场景 × 3 次 + 8 个连续创作场景 × 3 次** 的非视觉覆盖，不再评分外观、材质配色、产物实际风格、部件、旧模型具体外形或未视觉检查声明。五批实测与原始评分作为历史保留；第五批完整轨迹已另行重新审核并达到新范围门槛，未新增模型调用。技术事实与执行决策仍需独立评测，Runtime 成功拦截违规提议不能抵消 Agent 的错误。

浏览器契约、真实 Go/Eino 配合固定 Provider 的操作记录及截图见[浏览器验证](docs/verification/conversation-browser/README.md)；真实供应商链路见[Tripo 验证](docs/verification/conversation-tripo/README.md)。升级前遵循[停写备份与回退说明](docs/conversation-upgrade.md)，数据库与模型目录须一起处理。

项目范围以 [PROJECT_BRIEF.md](PROJECT_BRIEF.md)、[CONTEXT.md](CONTEXT.md) 和 [ADR](docs/adr/) 为准。现行行为见 [主规格](openspec/specs/)，本次规格与任务见 [add-conversational-asset-workspace 归档](openspec/changes/archive/2026-09-14-add-conversational-asset-workspace/)。历史 [first-asset-demo](openspec/changes/first-asset-demo/) 保持原验收记录，不随本变更自动归档。

## 可选技术约束变更

新版聊天目标不再强制补全 5,000 面、10 MiB；仅验收用户明确或继承的上限。没有上限仍会检查文件并展示实测值，下载与解析保护保持。旧执行和旧单请求 API 保留原协议。实施及验证进度见 [任务清单](openspec/changes/make-asset-limits-optional/tasks.md)，不能以历史 84 次评测代替新版结果。

2026-09-17 已完成新版 **84 次主评测 + 18 次补充评测**及全部 **423 条原始提议**的非视觉审核，四组分别达到原定门槛，关键错误均为 0；仍保留 1 次响应截断导致的失败，详见[本次结果](docs/evaluations/2026-09-17-optional-v4-evaluation-results.md)。随后完成[真实 Tripo 无上限生成与相对减面](docs/verification/optional-asset-limits/tripo-live.md)，实测 **11,344→6,544 面**，旧版本与父关系验证通过。当前进度 **24/24**，变更验收完成，尚未提交、归档或部署。
