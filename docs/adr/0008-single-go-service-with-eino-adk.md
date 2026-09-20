# 采用单个 Go 服务与 Eino ADK 单 Agent

本项目需要单 Agent 决策、多轮澄清以及可靠的异步生产执行。采用单个 Go 服务，使用 Eino ADK 的 `ChatModelAgent` 和 `Runner` 承担 Agent 循环，将业务边界与确定性操作放在 Go 业务执行层。

| 部分 | 职责 |
|---|---|
| Eino `ChatModelAgent` + `Runner` | 模型与工具决策循环、澄清时暂停及收到回答后恢复、运行事件输出 |
| Go 业务执行层 | 约束校验、预算扣减、生产并发与队列调度、模型调用计数、截止时间与终止控制、Tripo 任务跟踪、轮询、下载和技术检查 |
| 事件记录与推送 | 按请求保存并关联 Agent 提议、Runtime 处理、工具执行和任务进度，用于时间线、证据展示、JSON 导出、评测及 WebSocket 推送 |

同一请求的生产操作串行执行，使下一次纠偏基于上一次检查结果。不同会话的资产请求可以并发推进，各自的 Context、预算、检查点、任务状态和事件相互隔离。单 Agent 指每次请求由一个 Agent 负责决策，不是全服务仅运行一个请求；匿名会话与并发边界见 [ADR 0014](0014-anonymous-sessions-and-concurrent-access.md)。

Eino 的模型循环次数上限不能代替最多三轮澄清或最多三次生产提交，业务计数与上限仍由 Go 应用强制执行。

Go 执行层管理最多 3 个生产阶段请求和最多 10 个按序等待请求，队列状态与顺序持久化到 SQLite。等待用户澄清不占生产名额；排队不消耗生产提交额度，也不启动执行计时，见 [ADR 0016](0016-bounded-production-concurrency-and-queue.md)。

单请求累计模型调用额度和执行截止时间由 Go 应用持久化并强制执行，失败及重试调用均受额度约束，不单独依赖框架循环次数。模型额度耗尽后，Go 执行层仍在期限内完成已提交任务的查询与检查，并输出结果说明，见 [ADR 0013](0013-execution-limits-and-local-stop.md)。

这一选择复用框架提供的工具决策循环与中断恢复机制，同时将后台任务、超时控制、类型化接口及业务预算落实在 Go 应用中。Eino 检查点所需存储由应用提供；断线及服务重启后的恢复范围见 [ADR 0009](0009-recover-known-executions-without-resubmission.md)，SQLite 与本地模型目录的持久化方案见 [ADR 0010](0010-sqlite-and-local-artifacts.md)。

项目内 Skill 使用 Eino 的按需加载能力，由当前 Agent 继续执行，范围见 [ADR 0011](0011-request-context-and-readonly-skills.md)。

按用户确认，Agent 的大语言模型采用 DeepSeek V4 Pro，API 型号为 `deepseek-v4-pro`，作为首版演示和案例集评测的模型基线。该型号对应供应商持续更新的版本，固定型号字符串不等于冻结模型权重；执行与评测记录需保存请求型号、模型配置、运行日期及供应商实际返回的版本信息，未提供的精确版本标记为不可得。[DeepSeek 官方更新说明](https://api-docs.deepseek.com/updates/)。

实现时优先使用 Eino 官方 DeepSeek 适配器 `github.com/cloudwego/eino-ext/components/model/deepseek`，通过 `deepseek.NewChatModel` 接入 `ChatModelAgent`，调用 DeepSeek 的 Chat Completions 接口。2026-09-07 核查的官方主分支实现提供工具调用接口，并读写 `reasoning_content`；这是接入路径的源码依据，不代表本项目已完成 V4 兼容性实测。[Eino DeepSeek 适配器源码](https://github.com/cloudwego/eino-ext/blob/main/components/model/deepseek/deepseek.go)。

使用思考模式并携带工具定义时，后续请求须保留全部历史助手消息的 `reasoning_content`，包括未发生工具调用的轮次。模型协议状态与工具调用关联信息必须随请求和检查点保留，不能只保存对话正文；页面仍按既定范围展示简短决策依据和执行证据。[DeepSeek 思考模式与工具调用要求](https://api-docs.deepseek.com/guides/thinking_mode/)。

实现阶段锁定 Eino 与适配器的具体依赖版本，并验证多轮工具调用、历史字段完整续传、澄清中断与重启恢复；若采用流式模型响应，还须验证字段拼接。模型参数在首次运行与恢复时保持一致，预算与终止边界仍由 Go 执行层强制执行。记录本决策时尚未安装依赖、调用真实模型或运行案例集评测；后续验证进展见下文。

执行记录由 Agent 输出与 Go 执行层事实共同组成，明确区分计划、提议、放行或拦截和实际执行结果，保存所用模型、Prompt 与 Skill 版本；展示与导出范围见 [ADR 0017](0017-execution-timeline-and-evidence-export.md)。

依据：[Eino 官方 Agent 实现](https://github.com/cloudwego/eino/blob/v0.9.19/adk/chatmodel.go)、[Runner 与中断恢复文档](https://www.cloudwego.io/docs/eino/core_modules/eino_adk/agent_extension/)。以上说明记录构件选择时的依据。

2026-09-08 验证进展：项目已锁定 Eino v0.9.19 与 DeepSeek 适配器 v0.1.7，使用非流式模型响应。首条真实 DeepSeek V4 Pro / Tripo 木箱链路通过，包含 5 次模型调用、按需 Skill、真实工具反馈及生产中重启后续查同一任务。澄清中断与历史字段续传由独立受控集成和 SDK 协议测试覆盖；本次真实请求因需求明确未发起澄清，不能将其算作真实多轮澄清验证。60 次 Agent 案例评测尚未运行。证据见 [演示验证记录](../demo-verification.md)。
